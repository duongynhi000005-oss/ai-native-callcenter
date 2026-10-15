// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// This measures W-D9 of docs/design/doubao-findings.md against the real
// provider and costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST is
// set. The question: after Session.UpdateInstructions (a full session.update),
// does the next reply obey the new instructions, and do the tools survive?
//
// A flow changes phase by appending a paragraph to the persona, so the update
// here is the same persona plus a paragraph that makes the model begin every
// answer with a marker it would never say on its own. The provider takes no
// text user turns, so the caller's questions are synthesized speech
// (testdata/q_*.pcm, 16 kHz mono PCM16) streamed at real-time pace between
// stretches of room tone, which is what lets the server's VAD end each turn.
//
// The clips were generated on 2026-10-15 with Aliyun qwen TTS (model
// qwen3-tts-flash, voice Cherry, language_type Chinese, DashScope
// multimodal-generation endpoint), converted with afconvert to raw 16 kHz mono
// PCM16 little-endian, header stripped. Texts: q_hours "你们的客服几点下班？",
// q_broadband "请问宽带怎么办理？", q_price "那资费大概是多少钱？", q_order
// "我想查一下我的订单状态，订单号是一二三四五。". The test itself calls no TTS.
//
// Arms: control (no update; the marker must never appear), idle (update while
// the model is silent, then two more questions: immediacy and persistence),
// speaking (update sent while the model is still talking, then two more
// questions), tools-control (no update; does the order question call the tool
// on its own) and tools (the update adds the marker rule and tells the model
// to call lookup_order on an order question, and the reply after the tool
// result must carry the marker; the tool was declared at session start, so a
// tool lost to the overwrite shows as no call).
//
// The assertions encode what was measured on 2026-10-15 (see W-D9): the control
// never carries the marker, and every post-update reply in the update arms
// does. The numbers and transcripts are logged for the findings document.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveDoubaoMidCall -v ./internal/provider/doubao/
//
// AICC_LIVE_RUNS (default 3) is how many sessions each arm opens.
const (
	livePersona   = "你是 NovaNet 的电话客服。请用中文，简短自然地回答，每次不超过两句话。"
	livePhase     = "\n\n从现在开始，你的每一句回答都必须以“好的，王先生”开头，然后再回答问题。"
	liveMarker    = "好的王先生"
	liveTool      = "lookup_order"
	liveToolPhase = "\n\n当来电者询问订单状态时，必须先调用 lookup_order 工具查询，不要直接回答。"
)

func TestLiveDoubaoMidCallInstructions(t *testing.T) {
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	if os.Getenv("DOUBAO_API_KEY") == "" {
		t.Skip("DOUBAO_API_KEY is not set")
	}
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}

	hours, broadband, price, order := readPCM(t, "q_hours"), readPCM(t, "q_broadband"),
		readPCM(t, "q_price"), readPCM(t, "q_order")

	type arm struct {
		name    string
		tools   bool
		phase   string
		speak   bool // update while the first reply is being spoken
		updates bool
		asks    [][]byte
		// checked is the index of the first ask whose reply must carry the marker.
		checked int
	}
	arms := []arm{
		{name: "control", asks: [][]byte{hours, broadband, price}, checked: -1},
		{name: "idle", updates: true, phase: livePhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "speaking", updates: true, speak: true, phase: livePhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "tools-control", tools: true, asks: [][]byte{hours, order}, checked: -1},
		{name: "tools", tools: true, updates: true, phase: livePhase + liveToolPhase, asks: [][]byte{hours, order}, checked: -1},
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			obeyed, total, toolCalls := 0, 0, 0
			for run := 1; run <= runs; run++ {
				r := openLiveCall(t, a.tools)
				opening := r.expectReply(t, "opening")
				t.Logf("[%s #%d] X-Tt-Logid %s", a.name, run, r.session.logID())
				t.Logf("[%s #%d] opening: %q", a.name, run, opening.text)

				for i, ask := range a.asks {
					if a.updates && i == 1 {
						if !a.speak {
							r.update(t, livePersona+a.phase)
						}
					}
					r.say(ask)
					if a.updates && a.speak && i == 0 {
						r.waitSpeaking(t)
						r.update(t, livePersona+a.phase)
					}
					reply := r.expectReply(t, fmt.Sprintf("ask %d", i+1))
					for _, call := range reply.tools {
						toolCalls++
						t.Logf("[%s #%d] ask %d TOOL_CALL %s %s", a.name, run, i+1, call.ToolName, call.ToolArgs)
						_ = r.session.SendToolResult(call.ToolCallID, `{"status":"已发货，预计明天送达"}`, "")
					}
					afterText := ""
					if len(reply.tools) > 0 {
						after := r.expectReply(t, "after tool result")
						afterText = after.text
						t.Logf("[%s #%d] ask %d after tool: %q", a.name, run, i+1, after.text)
					}
					has := strings.Contains(normalize(reply.text), liveMarker)
					if len(reply.tools) > 0 {
						has = strings.Contains(normalize(afterText), liveMarker)
					}
					t.Logf("[%s #%d] ask %d reply: %q marker=%v", a.name, run, i+1, reply.text, has)
					if a.checked >= 0 && i >= a.checked {
						total++
						if has {
							obeyed++
						}
					}
					if a.name == "tools" && i == 1 {
						total++
						if has {
							obeyed++
						}
					}
					if a.checked < 0 && !a.tools && has {
						t.Errorf("%s #%d: the control said the marker on its own: %q", a.name, run, reply.text)
					}
				}
				r.close(t)
			}
			t.Logf("[%s] RESULT marker obeyed %d/%d, tool calls %d over %d sessions",
				a.name, obeyed, total, toolCalls, runs)
			if a.checked >= 0 && obeyed != total {
				t.Errorf("%s: %d of %d post-update replies obeyed the new instructions", a.name, obeyed, total)
			}
			if a.name == "tools" && toolCalls == 0 {
				t.Errorf("no lookup_order call after the update in %d sessions", runs)
			}
		})
	}
}

type liveReply struct {
	text  string
	tools []provider.Event
}

type liveCall struct {
	session *Session
	mu      sync.Mutex
	queue   []byte
	stop    chan struct{}
	done    chan struct{}
}

func openLiveCall(t *testing.T, withTool bool) *liveCall {
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
		&slog.HandlerOptions{Level: slog.LevelWarn})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	cfg := provider.SessionConfig{
		Instructions: livePersona,
		Language:     "zh",
		Turn:         provider.DefaultTurnDetection(),
		OpeningText:  "您好，这里是 NovaNet 客服中心，请问有什么可以帮您？",
		InputFormat:  media.PCM16Format(media.RateProviderIn),
		OutputFormat: media.PCM16Format(media.RateProviderOut),
	}
	if withTool {
		cfg.Tools = []provider.ToolSpec{{
			Name:        liveTool,
			Description: "Look up the status of a customer's order.",
			Parameters: json.RawMessage(`{"type":"object","properties":` +
				`{"orderId":{"type":"string","description":"the order number"}}}`),
		}}
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

// feed is the caller's microphone: queued speech when there is some, room tone
// otherwise, one 20 ms frame per tick.
func (c *liveCall) feed() {
	defer close(c.done)
	ticker := time.NewTicker(provider.FrameInterval)
	defer ticker.Stop()
	const frameBytes = 640
	frame := make([]byte, frameBytes)
	phase := 0
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		if len(c.queue) >= frameBytes {
			copy(frame, c.queue[:frameBytes])
			c.queue = c.queue[frameBytes:]
			c.mu.Unlock()
		} else {
			c.queue = nil
			c.mu.Unlock()
			for i := range frameBytes / 2 {
				binary.LittleEndian.PutUint16(frame[2*i:],
					uint16(int16(30*math.Sin(float64(phase+i)/7.0))))
			}
			phase += frameBytes / 2
		}
		if c.session.SendAudio(frame) != nil {
			return
		}
	}
}

// say queues a question behind a short stretch of room tone.
func (c *liveCall) say(pcm []byte) {
	c.mu.Lock()
	c.queue = append(c.queue, make([]byte, 640*10)...)
	c.queue = append(c.queue, pcm...)
	c.mu.Unlock()
}

// expectReply reads events until the model's turn ends, then a moment longer
// for the final transcript, which can trail the end of the audio.
func (c *liveCall) expectReply(t *testing.T, what string) liveReply {
	t.Helper()
	var reply liveReply
	deadline := time.After(40 * time.Second)
	ended := false
	var grace <-chan time.Time
	for {
		select {
		case <-deadline:
			t.Fatalf("%s: no complete reply within 40 s (text so far %q)", what, reply.text)
		case <-grace:
			return reply
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatalf("%s: the session ended", what)
			}
			switch ev.Type {
			case provider.EventTypeOutputTranscript:
				if ev.IsFinal {
					reply.text += ev.Text
				}
			case provider.EventTypeToolCall:
				reply.tools = append(reply.tools, ev)
			case provider.EventTypeResponseDone:
				if !ended {
					ended = true
					grace = time.After(1500 * time.Millisecond)
				}
			case provider.EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
				if ev.IsFatal {
					t.Fatalf("%s: fatal provider error", what)
				}
			}
		}
	}
}

// waitSpeaking returns once the model has started a reply.
func (c *liveCall) waitSpeaking(t *testing.T) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("the model never started to answer")
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			if ev.Type == provider.EventTypeAudioDelta {
				// Pushed back so that expectReply still sees what follows;
				// the transcript of this reply is read from the stream.
				return
			}
		}
	}
}

func (c *liveCall) update(t *testing.T, text string) {
	t.Helper()
	if err := c.session.UpdateInstructions(text); err != nil {
		t.Fatalf("update instructions: %v", err)
	}
}

func (c *liveCall) close(t *testing.T) {
	t.Helper()
	close(c.stop)
	<-c.done
	if err := c.session.Close(context.Background()); err != nil {
		t.Errorf("close: %v", err)
	}
}

func readPCM(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".pcm")
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

// normalize drops the punctuation and spaces a transcript is free to vary.
func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '，', ',', '。', '.', '！', '!', '？', '?', '、', '“', '”', '"', '：', ':':
			return -1
		}
		return r
	}, s)
}
