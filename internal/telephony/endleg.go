// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// LegEnder is what ending a leg once takes: the platform's cause stamped on
// it, the kill, and the question whether it is still there. *Adapter is one.
type LegEnder interface {
	SetVariable(channelID, name, value string) error
	Hangup(channelID, cause string) error
	ChannelExists(channelID string) (bool, error)
}

// LegEnding is one leg the platform has decided to end.
type LegEnding struct {
	ChannelID string
	// Cause is the switch's cause the kill carries.
	Cause string
	// PlatformCause, when set, is stamped on the leg (VarHangupCause) before
	// the kill, because uuid_kill takes the switch's causes only.
	PlatformCause string
	// What names the leg in the log ("an overflowed leg").
	What string
	// Attrs ride every log line (a callId, say).
	Attrs []any
}

// HangupCheckAfter is how long after a kill the switch did not confirm the leg
// is looked for again.
//
// A switch that runs the kill late runs it the moment it recovers, and the
// leg's hangup follows at once, so the question is only whether it has
// recovered. Two command timeouts: past the reply that never came, and long
// enough that a switch answering again has had the time to run what it read
// before the question, which it answers in order.
const HangupCheckAfter = 2 * esl.CommandTimeout

// EndLeg hangs up a leg the platform has decided to end, once (#303).
//
// The kill is sent once. A switch that confirms or refuses it has decided, and
// a refusal for a channel it no longer has is the leg already gone. One it does
// not answer (esl.ErrTimeout) is not a failure: a stalled switch runs it when
// it recovers. So it is neither logged as the switch refusing nor sent again
// at once, which would kill twice blindly; after HangupCheckAfter the switch
// is asked whether the leg is still there, and only a leg it still has is
// stamped and killed a second time. A switch that still cannot answer by then
// is left to finish what it was sent: the kill is on its socket, and asking
// again would only queue behind it.
//
// after starts the timer for the check; nil is time.AfterFunc.
func EndLeg(sw LegEnder, after func(time.Duration, func()), e LegEnding, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	attrs := append([]any{"channelId", e.ChannelID, "cause", e.Cause}, e.Attrs...)
	if e.PlatformCause != "" {
		attrs = append(attrs, "platformCause", e.PlatformCause)
	}
	if !killLeg(sw, e, log, attrs, "") {
		return
	}
	if after == nil {
		after = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	}
	log.Warn("hangup of "+e.What+" sent and not confirmed; the switch may still run it, "+
		"so it is checked before anything else is sent",
		append(attrs, "checkAfterSec", int(HangupCheckAfter.Seconds()))...)
	after(HangupCheckAfter, func() {
		isUp, err := sw.ChannelExists(e.ChannelID)
		switch {
		case err != nil:
			log.Warn("hangup of "+e.What+" still unconfirmed: the switch cannot say whether the leg is up; "+
				"leaving the kill it was sent to run",
				append(attrs, "error", err)...)
		case !isUp:
			log.Info("hangup of "+e.What+" settled: the switch ran it late", attrs...)
		default:
			log.Warn("hangup of "+e.What+" never ran: the leg is still up; ending it now", attrs...)
			killLeg(sw, e, log, attrs, " (again)")
		}
	})
}

// killLeg stamps and kills a leg once, logging what the switch said. It
// reports whether the kill is unconfirmed.
func killLeg(sw LegEnder, e LegEnding, log *slog.Logger, attrs []any, again string) (isUnconfirmed bool) {
	if e.PlatformCause != "" {
		if err := sw.SetVariable(e.ChannelID, VarHangupCause, e.PlatformCause); err != nil {
			level := slog.LevelError
			if errors.Is(err, esl.ErrTimeout) {
				level = slog.LevelWarn
			}
			log.Log(context.Background(), level, "could not confirm the cause stamped on "+e.What+again,
				append(attrs, "error", err)...)
		}
	}
	err := sw.Hangup(e.ChannelID, e.Cause)
	switch {
	case err == nil:
		return false
	case errors.Is(err, esl.ErrTimeout):
		return true
	case isNoSuchChannel(err):
		log.Info("hangup of "+e.What+again+": the leg was already gone", attrs...)
	default:
		log.Error("hangup of "+e.What+again+" failed", append(attrs, "error", err)...)
	}
	return false
}

// isNoSuchChannel recognises the switch's refusal of a command for a channel
// it does not have.
func isNoSuchChannel(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such channel")
}
