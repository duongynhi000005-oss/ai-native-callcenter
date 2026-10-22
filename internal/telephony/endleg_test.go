// SPDX-License-Identifier: Apache-2.0

package telephony

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/esl"
)

// enderSwitch is a switch for EndLeg: it records what it was sent, and
// answers kills and the question whether the leg is up as the test says.
type enderSwitch struct {
	mu      sync.Mutex
	ops     []string
	killErr []error // per kill, in order; past the end: nil
	isUp    bool
	upErr   error
}

func (s *enderSwitch) SetVariable(channelID, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, "set "+channelID+" "+name+"="+value)
	return nil
}

func (s *enderSwitch) Hangup(channelID, cause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, op := range s.ops {
		if len(op) > 4 && op[:4] == "kill" {
			n++
		}
	}
	s.ops = append(s.ops, "kill "+channelID+" "+cause)
	if n < len(s.killErr) {
		return s.killErr[n]
	}
	return nil
}

func (s *enderSwitch) ChannelExists(channelID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ops = append(s.ops, "exists "+channelID)
	return s.isUp, s.upErr
}

func (s *enderSwitch) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.ops)
}

// heldTimers keeps the timers EndLeg starts, for the test to fire.
type heldTimers struct {
	mu     sync.Mutex
	after  []time.Duration
	pended []func()
}

func (h *heldTimers) start(d time.Duration, f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.after = append(h.after, d)
	h.pended = append(h.pended, f)
}

func (h *heldTimers) fire(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	if len(h.pended) != 1 {
		h.mu.Unlock()
		t.Fatalf("%d timers started, want one check", len(h.pended))
	}
	f := h.pended[0]
	h.pended = nil
	h.mu.Unlock()
	f()
}

func (h *heldTimers) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.after)
}

func overflowEnding() LegEnding {
	return LegEnding{ChannelID: "leg-1", Cause: "NORMAL_CLEARING", PlatformCause: "OVERFLOW", What: "a test leg"}
}

// A kill the switch did not answer is checked once, after HangupCheckAfter,
// and acted on by what the switch then says: a leg it ran the kill on late is
// left alone, a leg it still has is stamped and killed once more, and a switch
// that still answers nothing is left to run the kill it was sent (#303).
func TestAnUnansweredHangupIsCheckedOnceBeforeAnythingElseIsSent(t *testing.T) {
	t.Parallel()
	timeout := fmt.Errorf("%w: uuid_kill", esl.ErrTimeout)
	for _, tc := range []struct {
		name  string
		isUp  bool
		upErr error
		want  []string
	}{
		{name: "the switch ran it late", isUp: false,
			want: []string{"set leg-1 aicc_hangup_cause=OVERFLOW", "kill leg-1 NORMAL_CLEARING", "exists leg-1"}},
		{name: "the switch never ran it", isUp: true,
			want: []string{"set leg-1 aicc_hangup_cause=OVERFLOW", "kill leg-1 NORMAL_CLEARING", "exists leg-1",
				"set leg-1 aicc_hangup_cause=OVERFLOW", "kill leg-1 NORMAL_CLEARING"}},
		{name: "the switch still answers nothing", upErr: timeout,
			want: []string{"set leg-1 aicc_hangup_cause=OVERFLOW", "kill leg-1 NORMAL_CLEARING", "exists leg-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sw := &enderSwitch{killErr: []error{timeout, timeout}, isUp: tc.isUp, upErr: tc.upErr}
			timers := &heldTimers{}
			EndLeg(sw, timers.start, overflowEnding(), quietLog())

			if got := sw.recorded(); len(got) != 2 {
				t.Fatalf("before the check the switch was sent %q, want the stamp and one kill", got)
			}
			if timers.after[0] != HangupCheckAfter {
				t.Fatalf("the check was set for %s, want %s", timers.after[0], HangupCheckAfter)
			}
			timers.fire(t)
			if got := sw.recorded(); !slices.Equal(got, tc.want) {
				t.Errorf("the switch was sent %q, want %q", got, tc.want)
			}
			// A second kill that is not answered either is the end of it:
			// nothing is checked or sent again.
			if timers.count() != 1 {
				t.Errorf("%d checks started, want one", timers.count())
			}
		})
	}
}

// A kill the switch answered needs no check: done, or refused for a channel it
// no longer has (gone already), or refused outright (logged as such).
func TestAnAnsweredHangupIsNotChecked(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"done":            nil,
		"no such channel": errors.New("uuid_kill leg-1 NORMAL_CLEARING: -ERR No such channel!"),
		"link down":       fmt.Errorf("%w: not sent", esl.ErrDown),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			sw := &enderSwitch{killErr: []error{err}}
			timers := &heldTimers{}
			EndLeg(sw, timers.start, overflowEnding(), quietLog())
			if timers.count() != 0 {
				t.Errorf("a hangup the switch answered started %d checks", timers.count())
			}
			if got := sw.recorded(); len(got) != 2 {
				t.Errorf("the switch was sent %q, want the stamp and one kill", got)
			}
		})
	}
}

// uuid_exists answers true or false; anything else is not an answer.
func TestChannelExistsReadsTheSwitchsAnswer(t *testing.T) {
	t.Parallel()
	a, cmd := newTestAdapter()
	for reply, want := range map[string]bool{"true": true, "false": false, "true\n": true} {
		cmd.reply = reply
		got, err := a.ChannelExists("leg-1")
		if err != nil || got != want {
			t.Errorf("reply %q: got %v, %v; want %v", reply, got, err, want)
		}
		if cmd.last() != "uuid_exists leg-1" {
			t.Errorf("sent %q", cmd.last())
		}
	}
	cmd.reply = "-ERR no reply"
	if _, err := a.ChannelExists("leg-1"); err == nil {
		t.Error("an unreadable reply was taken as an answer")
	}
	cmd.err = fmt.Errorf("%w: uuid_exists", esl.ErrTimeout)
	if _, err := a.ChannelExists("leg-1"); !errors.Is(err, esl.ErrTimeout) {
		t.Errorf("a timeout came back as %v", err)
	}
}
