// SPDX-License-Identifier: Apache-2.0

package outbound

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rasonyang/ai-native-callcenter/internal/obs"
	"github.com/rasonyang/ai-native-callcenter/internal/telephony"
)

// The outbound line account of the DIDs (design 10 section 6.6).
//
// A carrier sells a number a fixed count of simultaneous outbound calls. The
// account has two parts, and a DID's lines in use is their sum:
//
//   - reserved: a call this platform places (single call, campaign attempt,
//     click-to-dial) takes a line under its call id BEFORE the originate is
//     sent and gives it back when the call ends or the originate fails. Taking
//     it first is what stops originates fired in the same instant from all
//     slipping past the limit before their channels exist.
//   - observed: an outbound line leg the platform did not place (an agent
//     dialling by hand) is counted when its channel is created and uncounted
//     when it hangs up. A leg whose call id already holds a reservation is not
//     counted twice.
//
// Reserved lines are released on every ending: the call's last leg hanging up
// (Service.CallEnded, wired to the registry), a failed originate (synchronous
// or by job result), and, for a release that was somehow never signalled, the
// sweep, which gives back a reservation older than its placement window whose
// call is not live.

// lineHold is one reserved line.
type lineHold struct {
	did   string
	since time.Time
}

// Lines is the account. The zero value is not usable; use NewLines.
type Lines struct {
	mu       sync.Mutex
	now      func() time.Time
	reserved map[uuid.UUID]*lineHold
	observed map[string]string // channel id -> DID
	inUse    map[string]int    // DID -> reserved + observed
	lastSwp  time.Time
	// onFree is told, outside the lock, that lines were given back. It must not
	// block (the dialer's is a mailbox post).
	onFree func()
}

// SetOnFree installs the function told whenever lines are given back, so a
// dialer holding admission for want of a line looks again at once. Set before
// the account is used.
func (l *Lines) SetOnFree(f func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onFree = f
}

func (l *Lines) freed() {
	l.mu.Lock()
	f := l.onFree
	l.mu.Unlock()
	if f != nil {
		f()
	}
}

// NewLines builds an empty account.
func NewLines() *Lines {
	return &Lines{
		now:      time.Now,
		reserved: map[uuid.UUID]*lineHold{},
		observed: map[string]string{},
		inUse:    map[string]int{},
	}
}

func (l *Lines) addLocked(did string, delta int) {
	l.inUse[did] += delta
	if l.inUse[did] <= 0 {
		delete(l.inUse, did)
	}
	obs.DIDOutboundInUseChanged(did, int64(delta))
}

// Reserve takes a line on did for callID, unless that would put the lines in
// use above limit (nil: no limit). A second reservation under a call id that
// holds one succeeds without taking another, so a retried placement does not
// take two lines.
func (l *Lines) Reserve(callID uuid.UUID, did string, limit *int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.reserved[callID]; ok {
		return h.did == did
	}
	if limit != nil && l.inUse[did] >= *limit {
		return false
	}
	l.reserved[callID] = &lineHold{did: did, since: l.now()}
	l.addLocked(did, 1)
	return true
}

// Adopt records a line a call already holds, without a limit check: the call
// exists whether or not the account has room for it (a restart rebuilding the
// account from the switch). Idempotent.
func (l *Lines) Adopt(callID uuid.UUID, did string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.reserved[callID]; ok {
		return
	}
	l.reserved[callID] = &lineHold{did: did, since: l.now()}
	l.addLocked(did, 1)
}

// Release gives back the line callID holds. False when it held none.
func (l *Lines) Release(callID uuid.UUID) bool {
	l.mu.Lock()
	h, ok := l.reserved[callID]
	if ok {
		delete(l.reserved, callID)
		l.addLocked(h.did, -1)
	}
	l.mu.Unlock()
	if ok {
		l.freed()
	}
	return ok
}

// Holds reports whether callID holds a reserved line.
func (l *Lines) Holds(callID uuid.UUID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.reserved[callID]
	return ok
}

// Observe counts a line leg this platform did not place. It does nothing, and
// reports false, when callID holds a reservation (the leg belongs to a call
// that is counted already) or the channel is counted already.
func (l *Lines) Observe(channelID string, callID uuid.UUID, did string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if callID != uuid.Nil {
		if _, ok := l.reserved[callID]; ok {
			return false
		}
	}
	if _, ok := l.observed[channelID]; ok {
		return false
	}
	l.observed[channelID] = did
	l.addLocked(did, 1)
	return true
}

// Unobserve uncounts an observed leg that hung up.
func (l *Lines) Unobserve(channelID string) {
	l.mu.Lock()
	did, ok := l.observed[channelID]
	if ok {
		delete(l.observed, channelID)
		l.addLocked(did, -1)
	}
	l.mu.Unlock()
	if ok {
		l.freed()
	}
}

// KeepObserved drops every observed leg that is not among the live channels:
// its hangup was lost while the link was down. Called with a complete listing
// only.
func (l *Lines) KeepObserved(live map[string]bool) int {
	l.mu.Lock()
	n := 0
	for ch, did := range l.observed {
		if !live[ch] {
			delete(l.observed, ch)
			l.addLocked(did, -1)
			n++
		}
	}
	l.mu.Unlock()
	if n > 0 {
		l.freed()
	}
	return n
}

// InUse is the lines held on did: reserved plus observed.
func (l *Lines) InUse(did string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inUse[did]
}

// Sweep releases reservations older than maxAge whose call is not live, and
// reports the call ids it released. It is the net under the explicit release
// paths: a signal that never arrived must not hold a line for ever. At most one
// sweep a minute does any work.
func (l *Lines) Sweep(maxAge time.Duration, isLive func(uuid.UUID) bool) []uuid.UUID {
	l.mu.Lock()
	now := l.now()
	if now.Sub(l.lastSwp) < time.Minute {
		l.mu.Unlock()
		return nil
	}
	l.lastSwp = now
	var stale []uuid.UUID
	for id, h := range l.reserved {
		if now.Sub(h.since) > maxAge {
			stale = append(stale, id)
		}
	}
	l.mu.Unlock()

	var out []uuid.UUID
	for _, id := range stale {
		if isLive != nil && isLive(id) {
			continue
		}
		if l.Release(id) {
			out = append(out, id)
		}
	}
	return out
}

// observeLine counts a created channel that takes a carrier line this
// application did not place. A destination that is a number of the switch
// itself (an extension or a queue) is no carrier line.
func (s *Service) observeLine(ev telephony.SwitchEvent) {
	did, ok := telephony.OutboundLineLeg(ev)
	if !ok {
		return
	}
	callID, _ := uuid.Parse(ev.Raw.Variable("aicc_call_id"))
	if s.isInternalNumber(ev.DestinationNumber) {
		return
	}
	if s.lines.Observe(ev.ChannelID, callID, did) {
		s.log.Debug("observed an outbound line leg", "channelId", ev.ChannelID, "did", did)
	}
}

func (s *Service) isInternalNumber(number string) bool {
	if s.numbers == nil || number == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	internal, err := s.numbers.IsInternalNumber(ctx, number)
	return err == nil && internal
}

// rebuildLines is the line account after a restart or a reconnect, read from
// the channels the switch holds: a call this platform placed holds its line
// under its call id, and a leg it did not place is observed. Everything is
// idempotent, so a pass over channels already counted changes nothing. Observed
// legs that the switch no longer lists lost their hangup and are dropped.
func (s *Service) rebuildLines(live []telephony.LiveChannel) {
	liveIDs := make(map[string]bool, len(live))
	for _, ch := range live {
		liveIDs[ch.ChannelID] = true
		if ch.IsBotLeg || ch.IsInbound {
			continue
		}
		switch {
		case ch.PlacedBy == telephony.PlacedByDialAI && ch.DID != "":
			s.lines.Adopt(ch.CallID, ch.DID)
		case ch.PlacedBy == "" && ch.OutboundLineDID != "" && !s.isInternalNumber(ch.DialedNumber):
			s.lines.Observe(ch.ChannelID, ch.CallID, ch.OutboundLineDID)
		}
	}
	s.lines.KeepObserved(liveIDs)
}
