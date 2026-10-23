// SPDX-License-Identifier: Apache-2.0

package streamin

import (
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// SwitchTap is the slice of the switch adapter this package needs. Command
// strings are built in the adapter and nowhere else, so this is an interface
// over four of them rather than a place that composes a fifth.
type SwitchTap interface {
	StartAudioStream(channelID, wsURL string, rateHz int, metadata string) error
	StopAudioStream(channelID string) error
	PauseAudioStream(channelID string) error
	ResumeAudioStream(channelID string) error
	IsUp() bool
}

// tapKey identifies one attachment rather than one channel: the call it
// belongs to, the channel it runs on, and which attempt it is.
//
// A bare channel id is not an identity. It is the *place* a tap lives, and the
// same place is occupied by different taps over a call's life — a leg whose
// call id changes under it when two calls turn out to be one conversation, a
// re-attach after a stream that never connected. Keyed by channel alone, a
// late continuation from the first attachment retires the second, and the
// stop command lands on a stream somebody else owns.
//
// callID + channel is the same composite the ingest already keys its sessions
// and expectations on (Server.sessions, Server.pending); epoch distinguishes
// two attachments of the same pair, which is the case those two never see
// because the switch dials back only once per attach.
type tapKey struct {
	callID  uuid.UUID
	channel string
	epoch   uint64
}

// ErrSwitchDown reports that the tap could not be commanded because the ESL
// link is not up. Distinct from telephony.ErrNoTap: there is a tap, and we
// could not reach it.
var ErrSwitchDown = errors.New("streamin: the switch link is down")

// Tap starts and stops the media tap. It implements telephony.Tapper, which is
// how the coordinator drives transcription without knowing anything about it.
type Tap struct {
	srv       *Server
	sw        SwitchTap
	publicURL string
	rateHz    int
	ttl       time.Duration
	log       Logger

	mu    sync.Mutex
	epoch uint64
	// active holds every live attachment, so a call can retire all of its own
	// without knowing which channels they were on.
	active map[tapKey]struct{}
	// current is the channel's live attachment, which is what a switch event
	// naming only a channel resolves through.
	current map[string]tapKey
	// tokens is the attach token each live attachment's stream carries. It is
	// what lets a detach revoke that one stream — and only that one — while a
	// `start` the switch has not run yet is still in flight (#312).
	tokens map[tapKey]string
}

// NewTap builds the coordinator's handle on the ingest.
//
// publicURL is what the *switch* dials, which is not necessarily what we bind:
// in a container the two differ, exactly as the ESL address already does. The
// scheme comes from configuration and is never composed here, so a deployment
// that needs wss:// is a config change rather than a patch.
//
// ttl is how long an attach token stays usable, and it must outlast how late a
// stalled switch may run a timed-out start (41 s measured, #312): a start run
// after its token expired is refused as expired, and its leg goes untranscribed.
func NewTap(srv *Server, sw SwitchTap, publicURL string, rateHz int, ttl time.Duration, log Logger) *Tap {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	t := &Tap{
		srv: srv, sw: sw, publicURL: publicURL, rateHz: rateHz,
		ttl: ttl, log: log,
		active:  map[tapKey]struct{}{},
		current: map[string]tapKey{},
		tokens:  map[tapKey]string{},
	}
	if srv != nil {
		srv.AttachStreamEndHandler(t.streamEnded)
	}
	return t
}

// streamEnded drops an attachment the module has already finished with.
//
// It stops tracking; it does not command anything. The stream is over either
// way — what this prevents is the *next* Detach, on the hangup that follows,
// sending a stop for a channel FreeSWITCH has already destroyed. The module
// logs that at ERR, once for every tapped call, and a log where teardown is
// always an error is a log where a real error is invisible.
//
// Matched on the stream's token, not on its call and channel: the channel may
// already carry the next attachment — a re-attach under a new call, or under
// the same call after a detach — and a late close from the stream it replaced
// would otherwise forget the live one, so that no detach ever stops it.
func (t *Tap) streamEnded(channelID, token string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	key, ok := t.current[channelID]
	ok = ok && t.tokens[key] == token
	t.mu.Unlock()
	if !ok {
		return
	}
	t.forget(key)
}

// Attach taps one agent leg.
//
// A channel already tapped for this call is left alone: one bug per channel,
// which the module enforces too — unless the connect grace gave up on that
// attachment, in which case it is replaced, because otherwise a start the
// switch never ran leaves the leg untranscribed for the rest of the call. A
// channel tapped for a *different* call has had its conversation renamed under
// it — two calls turned out to be one — and the old stream is reporting into a
// transcript that is no longer this conversation's, so it is stopped before the
// new one starts.
//
// A timed-out `uuid_audio_stream … start` is not a failed attach. The command
// was written, and a stalled switch runs what it read when it recovers (#302).
// So the attachment and its expectation are kept, and the connect grace decides
// whether the stream ever arrived; only a command the switch cannot have read
// (esl.ErrDown) or refused (-ERR) forgets the tap. Before this, a timeout threw
// the attachment away and the stream that came up 41 seconds later was
// tracked by nothing — no detach could stop it, and no state was ever published
// for it (#312).
func (t *Tap) Attach(callID uuid.UUID, agentID, partyID *uuid.UUID, channelID, language string) {
	if t == nil || t.sw == nil || !t.sw.IsUp() || t.publicURL == "" {
		return
	}

	claim := Claim{
		CallID:   callID,
		Channel:  channelID,
		Language: language,
		Expires:  time.Now().Add(t.ttl),
	}
	if agentID != nil {
		claim.AgentID = *agentID
	}
	if partyID != nil {
		claim.PartyID = *partyID
	}

	// The channel's attachment is replaced under one lock acquisition, so two
	// attaches racing for the same channel have exactly one winner in the
	// index. The loser's stream is still stopped below — it is just no longer
	// the thing the index points at, which is why forget refuses to clear an
	// index entry that has moved on.
	token := t.srv.Token(claim)
	t.mu.Lock()
	prev, replaced := t.current[channelID]
	// The Server is asked under t.mu so that the answer and the replacement
	// are one decision. That is safe because the Server never calls into the
	// Tap while holding its own lock.
	neverCame := replaced && prev.callID == callID && t.srv.hasGivenUp(t.tokens[prev])
	if replaced && prev.callID == callID && !neverCame {
		t.mu.Unlock()
		return
	}
	t.epoch++
	key := tapKey{callID: callID, channel: channelID, epoch: t.epoch}
	t.active[key] = struct{}{}
	t.current[channelID] = key
	t.tokens[key] = token
	t.mu.Unlock()

	if replaced {
		if neverCame {
			// Detaching revokes the old token, so if the switch runs that
			// start after all, its stream is refused rather than racing this
			// one for the channel.
			t.log.Info("transcription tap replaces an attach whose stream never connected",
				"callId", callID, "channelId", channelID)
		} else {
			t.log.Info("transcription tap follows the call its leg was folded into",
				"channelId", channelID, "from", prev.callID, "to", callID)
		}
		_ = t.detach(prev)
	}

	dialURL, err := withToken(t.publicURL, token)
	if err != nil {
		t.forget(key)
		t.srv.Revoke(token)
		t.log.Error("transcription tap has an unusable stream url",
			"url", t.publicURL, "error", err)
		return
	}

	// The token identifies this attachment, not the channel: a re-attach after
	// a stream that never came gets one of its own.
	if err := t.sw.StartAudioStream(channelID, dialURL, t.rateHz, ""); err != nil {
		if errors.Is(err, esl.ErrTimeout) {
			t.srv.Expect(token, claim)
			t.log.Warn("transcription tap sent, not confirmed",
				"callId", callID, "channelId", channelID, "error", err)
			return
		}
		t.forget(key)
		t.srv.Revoke(token)
		t.log.Warn("transcription tap could not attach",
			"callId", callID, "channelId", channelID, "error", err)
		return
	}
	// +OK means the switch accepted the command, not that the socket came up:
	// the connect is asynchronous, and an attach to a port with nothing
	// listening succeeds just as loudly. So the ingest is told to expect this
	// stream, and to say so if it never arrives. It may already have dialled
	// back while the command was in flight; Expect knows, because the ingest
	// records the arrival (#287).
	t.srv.Expect(token, claim)
	t.log.Info("transcription tap attached",
		"callId", callID, "channelId", channelID, "rateHz", t.rateHz)
}

// Detach stops the tap on a channel the switch has just told us about.
// Harmless when there is none: a hangup arrives for every leg of every call
// and most of them were never tapped.
func (t *Tap) Detach(channelID string) {
	if t == nil {
		return
	}
	key, ok := t.currentKey(channelID)
	if !ok {
		return
	}
	_ = t.detach(key)
}

// DetachCall stops every tap this call has, whatever the switch did or did not
// say about the individual legs.
//
// This is the convergence guarantee. Detach is driven by CHANNEL_UNBRIDGE and
// CHANNEL_HANGUP, and neither is owed to us: a leg transferred away, an ESL
// link that reconnected across the hangup, an event this application filtered
// before the tap ever saw it. Any of those leaves the module pumping audio at
// an ingest whose transcript actor has been closed, for the life of the
// process. So the call's own termination path drives this unconditionally, and
// it is idempotent because it will usually run second.
func (t *Tap) DetachCall(callID uuid.UUID) {
	if t == nil {
		return
	}
	t.mu.Lock()
	keys := make([]tapKey, 0, 2)
	for key := range t.active {
		if key.callID == callID {
			keys = append(keys, key)
		}
	}
	t.mu.Unlock()
	for _, key := range keys {
		_ = t.detach(key)
	}
}

// Pause and Resume bracket hold, which is a private side-call and music,
// neither of which is this conversation.
//
// They report rather than shrug. A hold on a channel with no live tap is
// ordinary — the caller's own leg is never tapped — but it is the *caller's*
// business to decide that, and silently doing nothing makes a tap that died
// under us indistinguishable from one that was never there.
func (t *Tap) Pause(channelID string) error {
	return t.command(channelID, SwitchTap.PauseAudioStream)
}

func (t *Tap) Resume(channelID string) error {
	return t.command(channelID, SwitchTap.ResumeAudioStream)
}

// command is written over a method expression rather than a bound method so
// that a nil Tap or a nil switch is a returned error and not a panic in the
// argument list.
func (t *Tap) command(channelID string, send func(SwitchTap, string) error) error {
	if t == nil {
		return telephony.ErrNoTap
	}
	key, ok := t.currentKey(channelID)
	if !ok {
		return telephony.ErrNoTap
	}
	if t.sw == nil || !t.sw.IsUp() {
		return ErrSwitchDown
	}
	return send(t.sw, key.channel)
}

// detach stops one attachment, and only if it is still the live one.
func (t *Tap) detach(key tapKey) error {
	live, token := t.retire(key)
	if t.srv != nil {
		// Its `start` may still be in flight — a command that timed out is
		// one the switch may run late — so the claim is retired too. A stop
		// sent now would be overtaken by that start; revoking the token is
		// what makes the late dial-back be refused instead of leaving a
		// stream on a call nobody owns (#312).
		t.srv.Revoke(token)
	}
	if !live {
		return telephony.ErrNoTap
	}
	if t.sw == nil || !t.sw.IsUp() {
		return ErrSwitchDown
	}
	if err := t.sw.StopAudioStream(key.channel); err != nil {
		// Expected whenever the channel ended first, which is most of the
		// time, so this is not a warning.
		t.log.Debug("transcription tap was already gone",
			"channelId", key.channel, "error", err)
	}
	return nil
}

func (t *Tap) currentKey(channelID string) (tapKey, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key, ok := t.current[channelID]
	return key, ok
}

// forget drops one attachment and reports whether it was still live.
func (t *Tap) forget(key tapKey) bool {
	live, _ := t.retire(key)
	return live
}

// retire drops one attachment and the token its stream carries, and reports
// whether the attachment was still live. The channel index is cleared only when
// it still points at this attachment: a continuation from an attachment that
// has already been replaced must not evict its successor.
func (t *Tap) retire(key tapKey) (live bool, token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, live = t.active[key]
	token = t.tokens[key]
	delete(t.tokens, key)
	if !live {
		return false, token
	}
	delete(t.active, key)
	if cur, ok := t.current[key.channel]; ok && cur == key {
		delete(t.current, key.channel)
	}
	return true, token
}

// withToken appends the credential to the stream URL, preserving whatever the
// deployment already put there.
func withToken(base, token string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", &url.Error{Op: "parse", URL: base,
			Err: errScheme}
	}
	q := u.Query()
	q.Set("t", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

var errScheme = &schemeError{}

type schemeError struct{}

func (*schemeError) Error() string {
	return "stream url must be ws:// or wss://"
}

// StreamPath is the path the ingest serves, so a deployment that configures a
// bare host still reaches it.
const StreamPath = "/stream"

// NormalizePublicURL fills in the path when a deployment gives only a host.
func NormalizePublicURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if strings.TrimSuffix(u.Path, "/") == "" {
		u.Path = StreamPath
	}
	return u.String()
}
