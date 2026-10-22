// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// This measures W-D10 of docs/design/doubao-findings.md against the real
// provider and costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST is
// set. The question (#286): a wait line is committed while a tool call is
// still owed. Is the tool result sent after that line still answered?
//
// Live calls on 2026-10-22 said no: the line was heard, the result was sent,
// and the model never spoke again. The arms replay that on one session each:
//
//   - control: the result 2.5 s after the call, no line;
//   - line: the line committed 1 s after the call (SayHello,
//     speech_text_buffer.commit, what SpeakText sends), the result once the
//     line's turn has ended, which is what the calls did;
//   - replacement: the same line through ChatTTSText
//     (speech_text_buffer.replacement.append/commit), then the result;
//   - line-commit: as line, then an input_audio_buffer.commit after the
//     result, the one frame that forces a turn.
//
// Measured 2026-10-22, one session each: control answered (1 of 1); line,
// replacement and line-commit 0 of 1 each, with no error and no ack for the
// result. Only control is asserted. The wire is logged, audio frames left out,
// so the events around the line and the result can be read in the output.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveDoubaoWaitLine -v ./internal/provider/doubao/
//
// AICC_LIVE_RUNS (default 1) is how many sessions each arm opens;
// AICC_LIVE_ARMS (default all) is a comma-separated list of arms. Sessions are
// spaced a minute apart: the vendor counts a closed session for a while and
// refuses the next with ExceededConcurrentLimit.
const liveWaitLine = "请稍等，我查一下。"

func TestLiveDoubaoWaitLine(t *testing.T) {
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	if os.Getenv("DOUBAO_API_KEY") == "" {
		t.Skip("DOUBAO_API_KEY is not set")
	}
	runs := 1
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	arms := []string{"control", "line", "replacement", "line-commit"}
	if v := os.Getenv("AICC_LIVE_ARMS"); v != "" {
		arms = strings.Split(v, ",")
	}
	order := readPCM(t, "q_order")

	sessions := 0
	results := map[string][2]int{}
	for _, arm := range arms {
		for run := 1; run <= runs; run++ {
			if sessions > 0 {
				time.Sleep(60 * time.Second)
			}
			sessions++
			answered := runWaitLineArm(t, arm, run, order)
			got := results[arm]
			got[1]++
			if answered {
				got[0]++
			}
			results[arm] = got
		}
	}
	for _, arm := range arms {
		t.Logf("RESULT [%s] the result was answered in %d of %d sessions",
			arm, results[arm][0], results[arm][1])
	}
	if got, ok := results["control"]; ok && got[0] != got[1] {
		t.Errorf("control: a result with no line before it was answered in %d of %d sessions", got[0], got[1])
	}
}

// runWaitLineArm runs one session of an arm and reports whether the model
// answered the tool result.
func runWaitLineArm(t *testing.T, arm string, run int, order []byte) bool {
	t.Helper()
	r := openTracedLiveCall(t, arm, run)
	defer r.close(t)

	opening := r.expectReply(t, "opening")
	t.Logf("[%s #%d] X-Tt-Logid %s; opening %q", arm, run, r.session.logID(), opening.text)

	r.say(order)
	reply := r.expectReply(t, "order question")
	if len(reply.tools) == 0 {
		t.Logf("[%s #%d] no tool call; the reply was %q", arm, run, reply.text)
		return false
	}
	call := reply.tools[0]
	called := time.Now()
	t.Logf("[%s #%d] TOOL_CALL %s %s", arm, run, call.ToolName, call.ToolArgs)
	result := `{"ok":"1","status":"已发货，预计明天送达"}`

	switch arm {
	case "control":
		time.Sleep(2500 * time.Millisecond)
		if err := r.session.SendToolResult(call.ToolCallID, result); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
	case "line":
		time.Sleep(time.Second)
		if err := r.session.SpeakText(liveWaitLine, false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		line := r.expectReply(t, "wait line")
		t.Logf("[%s #%d] line said %q, %.1fs after the call", arm, run, line.text,
			time.Since(called).Seconds())
		if err := r.session.SendToolResult(call.ToolCallID, result); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
	case "line-commit":
		// The line as the call said it, the result after it, and then the
		// one frame this protocol has that asks for a turn: an audio commit,
		// which forces the end of the caller's (silent) query.
		time.Sleep(time.Second)
		if err := r.session.SpeakText(liveWaitLine, false); err != nil {
			t.Fatalf("speak: %v", err)
		}
		line := r.expectReply(t, "wait line")
		t.Logf("[%s #%d] line said %q", arm, run, line.text)
		if err := r.session.SendToolResult(call.ToolCallID, result); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		if err := r.session.sendEvent(simpleEvent{Type: "input_audio_buffer.commit",
			EventID: r.session.nextEventID()}); err != nil {
			t.Fatalf("send commit: %v", err)
		}
	case "replacement":
		// The same line through ChatTTSText (speech_text_buffer.replacement),
		// the event the documents give for "say this text instead of the
		// model's reply", rather than SayHello (speech_text_buffer.commit).
		time.Sleep(time.Second)
		speechID := "wait-" + strconv.Itoa(run)
		for _, frame := range []map[string]any{
			{"type": "speech_text_buffer.replacement.append", "event_id": r.session.nextEventID(),
				"speech_id": speechID, "text": liveWaitLine},
			{"type": "speech_text_buffer.replacement.commit", "event_id": r.session.nextEventID(),
				"speech_id": speechID},
		} {
			if err := r.session.sendEvent(frame); err != nil {
				t.Fatalf("send %s: %v", frame["type"], err)
			}
		}
		line, ok := r.awaitReply(5 * time.Second)
		t.Logf("[%s #%d] replacement line: started=%v %q", arm, run, ok, line.text)
		if err := r.session.SendToolResult(call.ToolCallID, result); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
	default:
		t.Fatalf("unknown arm %q", arm)
	}
	t.Logf("[%s #%d] result sent %.1fs after the call", arm, run, time.Since(called).Seconds())

	answer, ok := r.awaitReply(15 * time.Second)
	t.Logf("[%s #%d] ANSWER answered=%v %q", arm, run, ok, answer.text)
	return ok && answer.text != ""
}

// openTracedLiveCall is openLiveCall with the wire logged, the order tool
// declared and the instruction to use it.
func openTracedLiveCall(t *testing.T, arm string, run int) *liveCall {
	t.Helper()
	profile := provider.Profile{
		Name:         "doubao",
		Endpoint:     "wss://openspeech.bytedance.com/api/v3/duplex/realtime/dialogue",
		APIKeyEnv:    "DOUBAO_API_KEY",
		Voice:        "zh_female_vv_jupiter_bigtts",
		LinearInput:  media.PCM16Format(media.RateProviderIn),
		LinearOutput: media.PCM16Format(media.RateProviderOut),
	}
	session, err := newSession(profile, slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	began := time.Now()
	var mu sync.Mutex
	session.wireTap = func(isOutbound bool, data []byte) {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(data, &head)
		if head.Type == "response.output_audio.delta" || head.Type == "response.output_text.delta" ||
			head.Type == "conversation.item.input_audio_transcription.delta" {
			return
		}
		text := string(data)
		if len(text) > 400 {
			text = text[:400] + "…"
		}
		dir := "<-"
		if isOutbound {
			dir = "->"
		}
		mu.Lock()
		defer mu.Unlock()
		t.Logf("[%s #%d] %7.3fs %s %s", arm, run, time.Since(began).Seconds(), dir, text)
	}
	cfg := provider.SessionConfig{
		Instructions: livePersona + liveToolPhase,
		Language:     "zh",
		Turn:         provider.DefaultTurnDetection(),
		OpeningText:  "您好，这里是 NovaNet 客服中心，请问有什么可以帮您？",
		InputFormat:  media.PCM16Format(media.RateProviderIn),
		OutputFormat: media.PCM16Format(media.RateProviderOut),
		Tools: []provider.ToolSpec{{
			Name:        liveTool,
			Description: "Look up the status of a customer's order.",
			Parameters: json.RawMessage(`{"type":"object","properties":` +
				`{"orderId":{"type":"string","description":"the order number"}}}`),
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := session.Start(ctx, cfg); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := &liveCall{session: session, stop: make(chan struct{}), done: make(chan struct{})}
	go c.feed()
	return c
}

// awaitReply is expectReply that reports a missing reply instead of failing.
func (c *liveCall) awaitReply(within time.Duration) (liveReply, bool) {
	var reply liveReply
	deadline := time.After(within)
	var grace <-chan time.Time
	started := false
	for {
		select {
		case <-deadline:
			return reply, started
		case <-grace:
			return reply, true
		case ev, ok := <-c.session.Events():
			if !ok {
				return reply, started
			}
			switch ev.Type {
			case provider.EventTypeResponseStarted:
				started = true
			case provider.EventTypeOutputTranscript:
				if ev.IsFinal {
					reply.text += ev.Text
				}
			case provider.EventTypeToolCall:
				reply.tools = append(reply.tools, ev)
			case provider.EventTypeResponseDone, provider.EventTypeInterrupted:
				if grace == nil {
					grace = time.After(1500 * time.Millisecond)
				}
			}
		}
	}
}
