// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// This measures, against the real providers, that a line asked for with
// SpeakText while a tool call is still unanswered is said, and is not answered
// with a second call to the same tool. It costs money, so it is skipped unless
// AICC_LIVE_PROVIDER_TEST and the profile's key are set.
//
// The live round of #286 (2026-10-22, qwen, DID 95012) found the wait line's
// response.create answered with a second, identical function call: the
// backend was hit twice and the two results made two answer turns. A line's
// request carries a response-level tool_choice "none" where the profile obeys
// one (Profile.AcceptsResponseToolChoice), and whatever the profile, a call
// made in a line's turn is never handed out and is taken back out of the
// conversation when the turn ends (Realtime.onLineCall).
//
// One session per run: the caller asks for an order's status (testdata
// q_order, the clip the mid-call measurement uses), the model calls
// lookup_order, and with that call unanswered the test asks for the wait line.
// The line's turn must say the line and hand out no tool call. Then the tool's
// result is sent, and exactly one answer turn must follow. What the provider
// did on the wire (a call made in the line's turn, its delete and the
// provider's answer to it) is read from the client's own log and printed.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenLineCallsNoTool -v ./internal/provider/
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveGrokLineCallsNoTool -v ./internal/provider/
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveOpenAILineCallsNoTool -v ./internal/provider/
const liveWaitLine = "请稍等，我查一下。"

func TestLiveQwenLineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, QwenProfile()) }

func TestLiveGrokLineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, GrokProfile()) }

func TestLiveOpenAILineCallsNoTool(t *testing.T) { runLineCallsNoTool(t, OpenAIProfile()) }

func runLineCallsNoTool(t *testing.T, profile Profile) {
	requireLive(t, profile)
	input, _ := profile.FormatsFor(media.LawMu)
	order := readMidClip(t, input, "q_order")

	tap := &liveLogTap{}
	c := openMidCallLogged(t, profile, true, slog.New(tap))
	defer c.close(t)
	defer tap.print(t)
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)

	c.say(order)
	asked := c.expectReply(t, "the order question")
	t.Logf("reply to the question: %q, %d tool call(s)", asked.text, len(asked.tools))
	if len(asked.tools) == 0 {
		t.Fatalf("the model did not call %s, so there is no unanswered call to test against", midTool)
	}
	for _, call := range asked.tools {
		t.Logf("TOOL_CALL %s %s (%s)", call.ToolName, call.ToolArgs, call.ToolCallID)
	}

	// The call is unanswered, as a slow backend leaves it at the 1 s mark.
	sentAt := time.Now()
	if err := c.session.SpeakText(liveWaitLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	line := collectTurn(t, c, "the wait line")
	t.Logf("line turn: started=%v isSpokenLine=%v firstAudioAfter=%v audioBytes=%d text=%q tools=%d",
		line.started, line.isSpokenLine, line.firstAudio.Sub(sentAt).Round(time.Millisecond),
		line.audioBytes, line.text, len(line.tools))
	for _, call := range line.tools {
		t.Errorf("the line's turn called %s %s: a line must not call a tool", call.ToolName, call.ToolArgs)
	}
	if line.audioBytes == 0 {
		t.Errorf("the line's turn carried no audio")
	}
	if !line.isSpokenLine {
		t.Errorf("the line's turn was not reported as a spoken line")
	}
	if !strings.Contains(midNormalize(line.text), midNormalize(liveWaitLine)) {
		t.Errorf("the line's turn did not say the line: %q", line.text)
	}

	for _, call := range asked.tools {
		if err := c.session.SendToolResult(call.ToolCallID, `{"status":"已发货，预计明天送达"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
	}
	answer := collectTurn(t, c, "the answer")
	t.Logf("answer turn: audioBytes=%d text=%q tools=%d", answer.audioBytes, answer.text, len(answer.tools))
	if answer.audioBytes == 0 || answer.text == "" {
		t.Errorf("the tool result was not answered")
	}
	for _, call := range answer.tools {
		t.Errorf("the answer called %s %s again", call.ToolName, call.ToolArgs)
	}

	// Exactly one answer: nothing else is started while the caller is silent.
	if extra := c.countTurnsFor(t, 6*time.Second); extra != 0 {
		t.Errorf("%d more turn(s) started after the answer", extra)
	}
	t.Logf("calls the provider made in the line's turn and the client kept back: %d",
		tap.count("the turn asked for a line called a tool"))
}

// liveLogTap keeps what the client logs about function calls and items, and
// passes warnings and worse to stderr as the other live tests do.
type liveLogTap struct {
	mu    sync.Mutex
	lines []string
}

func (h *liveLogTap) Enabled(context.Context, slog.Level) bool { return true }

func (h *liveLogTap) Handle(_ context.Context, record slog.Record) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", record.Time.Format("15:04:05.000"), record.Level, record.Message)
	record.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	line := b.String()
	if record.Level >= slog.LevelWarn {
		fmt.Fprintln(os.Stderr, line)
	}
	h.mu.Lock()
	h.lines = append(h.lines, line)
	h.mu.Unlock()
	return nil
}

func (h *liveLogTap) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *liveLogTap) WithGroup(string) slog.Handler { return h }

// count is how many lines carry message.
func (h *liveLogTap) count(message string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, line := range h.lines {
		if strings.Contains(line, message) {
			n++
		}
	}
	return n
}

// print logs the lines about calls, items and errors.
func (h *liveLogTap) print(t *testing.T) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, line := range h.lines {
		for _, words := range []string{"called a tool", "function call", "removed an item", "error", "refus"} {
			if strings.Contains(line, words) {
				t.Logf("client log: %s", line)
				break
			}
		}
	}
}

// liveTurn is what one model turn carried.
type liveTurn struct {
	started      bool
	isSpokenLine bool
	firstAudio   time.Time
	audioBytes   int
	text         string
	tools        []Event
}

// collectTurn reads one turn: from its start to its end, then a moment longer
// for the final transcript, which can trail the end of the audio.
func collectTurn(t *testing.T, c *midCall, what string) liveTurn {
	t.Helper()
	var turn liveTurn
	deadline := time.After(40 * time.Second)
	var grace <-chan time.Time
	for {
		select {
		case <-deadline:
			t.Fatalf("%s: no complete turn within 40 s (text so far %q)", what, turn.text)
		case <-grace:
			return turn
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatalf("%s: the session ended", what)
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				turn.started = true
				turn.isSpokenLine = turn.isSpokenLine || ev.IsSpokenLine
			case EventTypeAudioDelta:
				if turn.audioBytes == 0 {
					turn.firstAudio = time.Now()
				}
				turn.audioBytes += len(ev.Audio)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					turn.text += ev.Text
				}
			case EventTypeToolCall:
				turn.tools = append(turn.tools, ev)
			case EventTypeResponseDone:
				if grace == nil {
					grace = time.After(1500 * time.Millisecond)
				}
			case EventTypeError:
				t.Logf("%s: provider error (fatal=%v): %v", what, ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("%s: fatal provider error", what)
				}
			}
		}
	}
}

// countTurnsFor counts the turns started within d.
func (c *midCall) countTurnsFor(t *testing.T, d time.Duration) int {
	t.Helper()
	n := 0
	until := time.After(d)
	for {
		select {
		case <-until:
			return n
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeResponseStarted:
				n++
				t.Logf("a turn started (isSpokenLine=%v)", ev.IsSpokenLine)
			case EventTypeToolCall:
				t.Logf("TOOL_CALL %s %s", ev.ToolName, ev.ToolArgs)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					t.Logf("said: %q", ev.Text)
				}
			}
		}
	}
}
