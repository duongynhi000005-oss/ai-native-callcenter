// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// This measures, against the real xAI Grok voice realtime API, whether a
// mid-call session.update that carries only new instructions is obeyed by the
// next replies, and probes the protocol points that decide whether Grok could
// be served by the OpenAI Realtime client as a profile (docs/design/
// grok-findings.md). It speaks the wire directly, opens no repo client and
// costs money, so it is skipped unless AICC_LIVE_PROVIDER_TEST and XAI_API_KEY
// are set.
//
// It mirrors the doubao measurement (W-D9 of doubao-findings.md) so the two
// compare: the persona is a standing instruction, the update is the same
// persona plus a paragraph that requires every answer to begin with a marker
// the model would not say unprompted, and the caller's questions are
// synthesized speech streamed at real-time pace.
//
// The clips (testdata/q_*.ulaw) are the qwen-TTS clips of the doubao
// measurement (qwen3-tts-flash, voice Cherry, Chinese, generated 2026-10-15),
// converted with `sox -t raw -r 16000 -e signed -b 16 -c 1 in.pcm -t raw
// -r 8000 -e u-law -c 1 out.ulaw` to raw G.711 mu-law 8 kHz, because that is
// how production would feed Grok. Texts: q_hours "你们的客服几点下班？",
// q_broadband "请问宽带怎么办理？", q_price "那资费大概是多少钱？", q_order
// "我想查一下我的订单状态，订单号是一二三四五。". The test calls no TTS.
// Audio goes in as audio/pcmu at 8000 Hz in 20 ms frames, with mu-law silence
// before and after each clip so the server's VAD ends the turn, and the reply
// comes back as audio/pcmu too.
//
// The update is sent as {"type":"session.update","session":{"type":"realtime",
// "instructions":...}}, the way the repo's Realtime client sends one; the
// first probe records whether Grok accepts the type field.
//
// Arms (AICC_LIVE_RUNS sessions each, default 3): control, idle, speaking,
// tools-control and tools, as in the doubao test. Assertions encode what was
// measured on 2026-10-15; whatever was not obeyed is logged, not asserted.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveGrok -v ./internal/provider/
const (
	grokURL       = "wss://api.x.ai/v1/realtime?model=grok-voice-3.0"
	grokPersona   = "你是 NovaNet 的电话客服。请用中文，简短自然地回答，每次不超过两句话。"
	grokPhase     = "\n\n从现在开始，你的每一句回答都必须以“好的，王先生”开头，然后再回答问题。"
	grokToolPhase = "\n\n当来电者询问订单状态时，必须先调用 lookup_order 工具查询，不要直接回答。"
	grokMarker    = "好的王先生"
	grokTool      = "lookup_order"
)

func grokSkip(t *testing.T) string {
	t.Helper()
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the real provider")
	}
	key := os.Getenv("XAI_API_KEY")
	if key == "" {
		t.Skip("XAI_API_KEY is not set")
	}
	return key
}

func grokClip(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/q_" + name + ".ulaw")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func grokNormalize(s string) string {
	return strings.NewReplacer("，", "", ",", "", " ", "", "。", "", "！", "", "!", "", "、", "").Replace(s)
}

// grokEvent is one server event; Raw is the whole JSON.
type grokEvent struct {
	Type string
	Raw  map[string]any
	At   time.Time
}

func (e grokEvent) str(path ...string) string {
	var cur any = e.Raw
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}

type grokConn struct {
	t      *testing.T
	label  string
	c      *websocket.Conn
	wmu    sync.Mutex
	events chan grokEvent
	queue  bytes.Buffer
	qmu    sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	closed chan struct{}
	seen   map[string]bool
	// audioBytes counts output_audio.delta payload bytes per response id.
	audioBytes map[string]int
	amu        sync.Mutex
	upgrade    *http.Response
}

func grokDial(t *testing.T, key, label string) *grokConn {
	t.Helper()
	h := http.Header{"Authorization": {"Bearer " + key}}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, resp, err := websocket.DefaultDialer.DialContext(ctx, grokURL, h)
	if err != nil {
		if resp != nil {
			b := new(bytes.Buffer)
			_, _ = b.ReadFrom(resp.Body)
			t.Fatalf("dial: %v (status %d, body %s)", err, resp.StatusCode, b.String())
		}
		t.Fatalf("dial: %v", err)
	}
	g := &grokConn{t: t, label: label, c: c, events: make(chan grokEvent, 4096),
		stop: make(chan struct{}), done: make(chan struct{}), closed: make(chan struct{}),
		seen: map[string]bool{}, audioBytes: map[string]int{}, upgrade: resp}
	var ids []string
	for _, k := range []string{"X-Request-Id", "X-Xai-Request-Id", "Cf-Ray", "Request-Id"} {
		if v := resp.Header.Get(k); v != "" {
			ids = append(ids, k+"="+v)
		}
	}
	t.Logf("[%s] connected; upgrade headers: %s", label, strings.Join(ids, " "))
	go g.read()
	go g.pump()
	return g
}

func (g *grokConn) read() {
	defer close(g.closed)
	for {
		_, data, err := g.c.ReadMessage()
		if err != nil {
			g.events <- grokEvent{Type: "@close", Raw: map[string]any{"error": err.Error()}, At: time.Now()}
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "response.output_audio.delta" || typ == "response.audio.delta" {
			if d, _ := m["delta"].(string); d != "" {
				raw, _ := base64.StdEncoding.DecodeString(d)
				id, _ := m["response_id"].(string)
				g.amu.Lock()
				g.audioBytes[id] += len(raw)
				g.amu.Unlock()
			}
		}
		g.events <- grokEvent{Type: typ, Raw: m, At: time.Now()}
	}
}

// pump sends one 20 ms frame every 20 ms: queued caller audio, else silence.
func (g *grokConn) pump() {
	defer close(g.done)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	silence := bytes.Repeat([]byte{0xFF}, 160)
	for {
		select {
		case <-g.stop:
			return
		case <-tick.C:
		}
		frame := silence
		g.qmu.Lock()
		if g.queue.Len() > 0 {
			frame = make([]byte, 160)
			n, _ := g.queue.Read(frame)
			copy(frame[n:], silence[n:])
		}
		g.qmu.Unlock()
		_ = g.send(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(frame)})
	}
}

func (g *grokConn) send(m map[string]any) error {
	g.wmu.Lock()
	defer g.wmu.Unlock()
	_ = g.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return g.c.WriteJSON(m)
}

func (g *grokConn) say(clip []byte) {
	g.qmu.Lock()
	g.queue.Write(clip)
	g.qmu.Unlock()
}

func (g *grokConn) speaking() bool {
	g.qmu.Lock()
	defer g.qmu.Unlock()
	return g.queue.Len() > 0
}

func (g *grokConn) close() {
	close(g.stop)
	<-g.done
	_ = g.c.Close()
	<-g.closed
}

// log records one event: deltas are only counted, everything else is printed
// (long audio payloads elided). A type is called out the first time it shows.
func (g *grokConn) log(ev grokEvent) {
	if strings.HasSuffix(ev.Type, ".delta") {
		if !g.seen[ev.Type] {
			g.seen[ev.Type] = true
			g.t.Logf("[%s] first %s", g.label, ev.Type)
		}
		return
	}
	g.seen[ev.Type] = true
	b, _ := json.Marshal(ev.Raw)
	s := string(b)
	if len(s) > 900 {
		s = s[:900] + "..."
	}
	g.t.Logf("[%s] <- %s", g.label, s)
}

// next returns the next event or ok=false after d.
func (g *grokConn) next(d time.Duration) (grokEvent, bool) {
	select {
	case ev := <-g.events:
		g.log(ev)
		return ev, true
	case <-time.After(d):
		return grokEvent{}, false
	}
}

// drain reads and logs events for d.
func (g *grokConn) drain(d time.Duration) []grokEvent {
	var out []grokEvent
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ev, ok := g.next(time.Until(end))
		if !ok {
			break
		}
		out = append(out, ev)
	}
	return out
}

// waitType reads until an event of one of the types, or d.
func (g *grokConn) waitType(d time.Duration, types ...string) (grokEvent, bool) {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ev, ok := g.next(time.Until(end))
		if !ok {
			break
		}
		if ev.Type == "@close" {
			return ev, false
		}
		for _, ty := range types {
			if ev.Type == ty {
				return ev, true
			}
		}
	}
	return grokEvent{}, false
}

type grokTurn struct {
	text      string
	tools     []grokToolCall
	status    string
	audio     int
	responseI string
	itemID    string
	errs      []string
}

type grokToolCall struct{ name, args, callID string }

// reply reads one response to its response.done.
func (g *grokConn) reply(d time.Duration) (grokTurn, bool) {
	var tr grokTurn
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		ev, ok := g.next(time.Until(end))
		if !ok {
			return tr, false
		}
		switch ev.Type {
		case "@close":
			tr.errs = append(tr.errs, "closed: "+ev.str("error"))
			return tr, false
		case "error":
			tr.errs = append(tr.errs, ev.str("error", "message"))
		case "response.output_audio_transcript.done", "response.audio_transcript.done":
			tr.text += ev.str("transcript")
			if id := ev.str("item_id"); id != "" {
				tr.itemID = id
			}
		case "response.function_call_arguments.done":
			tr.tools = append(tr.tools, grokToolCall{ev.str("name"), ev.str("arguments"), ev.str("call_id")})
		case "response.done":
			tr.status = ev.str("response", "status")
			tr.responseI = ev.str("response", "id")
			g.amu.Lock()
			tr.audio = g.audioBytes[tr.responseI]
			g.amu.Unlock()
			return tr, true
		}
	}
	return tr, false
}

var grokToolDef = map[string]any{
	"type":        "function",
	"name":        grokTool,
	"description": "Look up the status of a customer's order by order number.",
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"order_id": map[string]any{"type": "string", "description": "The order number"},
		},
		"required": []string{"order_id"},
	},
}

// open dials and configures a session: pcmu in and out, server_vad, the
// persona, optional tools and input transcription.
func grokOpen(t *testing.T, key, label string, instructions string, withTool bool, extra map[string]any) *grokConn {
	g := grokDial(t, key, label)
	if _, ok := g.waitType(10*time.Second, "session.created", "conversation.created"); !ok {
		t.Fatalf("[%s] no session.created", label)
	}
	sess := map[string]any{
		"instructions": instructions,
		"voice":        "eve",
		"turn_detection": map[string]any{
			"type": "server_vad",
		},
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcmu"}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcmu"}},
		},
	}
	if withTool {
		sess["tools"] = []any{grokToolDef}
	}
	for k, v := range extra {
		sess[k] = v
	}
	if err := g.send(map[string]any{"type": "session.update", "session": sess}); err != nil {
		t.Fatal(err)
	}
	ev, ok := g.waitType(10*time.Second, "session.updated", "error")
	if !ok || ev.Type == "error" {
		t.Fatalf("[%s] session.update not acknowledged: %+v", label, ev.Raw)
	}
	return g
}

func (g *grokConn) updateInstructions(withType bool, instructions string) {
	s := map[string]any{"instructions": instructions}
	if withType {
		s["type"] = "realtime"
	}
	if err := g.send(map[string]any{"type": "session.update", "session": s}); err != nil {
		g.t.Fatal(err)
	}
}

func TestLiveGrokMidCallInstructions(t *testing.T) {
	key := grokSkip(t)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	hours, broadband, price, order := grokClip(t, "hours"), grokClip(t, "broadband"), grokClip(t, "price"), grokClip(t, "order")

	type arm struct {
		name    string
		tools   bool
		phase   string
		speak   bool
		updates bool
		asks    [][]byte
		checked int
	}
	arms := []arm{
		{name: "control", asks: [][]byte{hours, broadband, price}, checked: -1},
		{name: "idle", updates: true, phase: grokPhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "speaking", updates: true, speak: true, phase: grokPhase, asks: [][]byte{hours, broadband, price}, checked: 1},
		{name: "tools-control", tools: true, asks: [][]byte{hours, order}, checked: -1},
		{name: "tools", tools: true, updates: true, phase: grokPhase + grokToolPhase, asks: [][]byte{hours, order}, checked: -1},
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			obeyed, total, toolCalls := 0, 0, 0
			for run := 1; run <= runs; run++ {
				label := fmt.Sprintf("%s#%d", a.name, run)
				g := grokOpen(t, key, label, grokPersona, a.tools, nil)
				for i, ask := range a.asks {
					if a.updates && !a.speak && i == 1 {
						g.updateInstructions(true, grokPersona+a.phase)
					}
					g.say(ask)
					if a.updates && a.speak && i == 0 {
						if _, ok := g.waitType(20*time.Second, "response.output_audio.delta", "response.audio.delta"); !ok {
							t.Errorf("[%s] first reply never started speaking", label)
						}
						g.updateInstructions(true, grokPersona+a.phase)
					}
					tr, ok := g.reply(30 * time.Second)
					if !ok {
						t.Errorf("[%s] ask %d: no complete reply (%+v)", label, i+1, tr)
					}
					afterText := ""
					for _, call := range tr.tools {
						toolCalls++
						t.Logf("[%s] ask %d TOOL_CALL %s %s", label, i+1, call.name, call.args)
						_ = g.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
							"type": "function_call_output", "call_id": call.callID,
							"output": `{"status":"已发货，预计明天送达"}`}})
						_ = g.send(map[string]any{"type": "response.create"})
					}
					if len(tr.tools) > 0 {
						after, _ := g.reply(30 * time.Second)
						afterText = after.text
						t.Logf("[%s] ask %d after tool: %q", label, i+1, after.text)
					}
					has := strings.Contains(grokNormalize(tr.text), grokMarker)
					if len(tr.tools) > 0 {
						has = strings.Contains(grokNormalize(afterText), grokMarker)
					}
					t.Logf("[%s] ask %d reply: %q marker=%v", label, i+1, tr.text, has)
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
						t.Errorf("%s: the control said the marker on its own: %q", label, tr.text)
					}
					// let the tail of the caller silence pass before the next turn
					g.drain(300 * time.Millisecond)
				}
				g.close()
			}
			t.Logf("[%s] RESULT marker obeyed %d/%d, tool calls %d over %d sessions", a.name, obeyed, total, toolCalls, runs)
			if a.checked >= 0 && obeyed != total {
				t.Errorf("%s: %d of %d post-update replies obeyed the new instructions", a.name, obeyed, total)
			}
			if a.name == "tools" && toolCalls == 0 {
				t.Errorf("no lookup_order call after the update in %d sessions", runs)
			}
		})
	}
}

// TestLiveGrokProtocol is one session per question of the findings document's
// probe list. It asserts only that sessions open; the point is the raw
// server responses it logs.
func TestLiveGrokProtocol(t *testing.T) {
	key := grokSkip(t)
	hours, broadband := grokClip(t, "hours"), grokClip(t, "broadband")
	long := "你是 NovaNet 的电话客服。请用中文回答，回答要详细，至少说五句话。"

	t.Run("1-update-type-realtime", func(t *testing.T) {
		g := grokOpen(t, key, "p1", grokPersona, false, nil)
		defer g.close()
		g.updateInstructions(true, grokPersona+grokPhase)
		g.drain(3 * time.Second)
		g.updateInstructions(false, grokPersona+grokPhase)
		g.drain(3 * time.Second)
	})

	t.Run("2-vad-params", func(t *testing.T) {
		g := grokOpen(t, key, "p2", grokPersona, false, map[string]any{
			"turn_detection": map[string]any{"type": "server_vad", "threshold": 0.6,
				"silence_duration_ms": 500, "prefix_padding_ms": 200},
		})
		defer g.close()
		g.drain(1 * time.Second)
		// A second update, to read the echo of the tuned values in isolation.
		_ = g.send(map[string]any{"type": "session.update", "session": map[string]any{
			"turn_detection": map[string]any{"type": "server_vad", "silence_duration_ms": 500}}})
		g.drain(3 * time.Second)
	})

	t.Run("3-response-create-empty", func(t *testing.T) {
		g := grokOpen(t, key, "p3", grokPersona+" 通话一开始，请先主动问候来电者。", false, nil)
		defer g.close()
		_ = g.send(map[string]any{"type": "response.create"})
		tr, ok := g.reply(20 * time.Second)
		t.Logf("[p3] complete=%v turn=%+v", ok, tr)
	})

	t.Run("4-bargein-and-cancel", func(t *testing.T) {
		g := grokOpen(t, key, "p4", long, false, nil)
		defer g.close()
		g.say(broadband)
		if _, ok := g.waitType(25*time.Second, "response.output_audio.delta", "response.audio.delta"); !ok {
			t.Fatal("no reply started")
		}
		time.Sleep(1500 * time.Millisecond)
		t.Logf("[p4] caller barges in with a second clip (hours)")
		g.say(hours)
		evs := g.drain(12 * time.Second)
		var types []string
		for _, e := range evs {
			if !strings.HasSuffix(e.Type, ".delta") {
				types = append(types, e.Type+" "+e.str("response", "status"))
			}
		}
		t.Logf("[p4] events after barge-in: %v", types)
		// nothing active now: response.cancel
		_ = g.send(map[string]any{"type": "response.cancel"})
		g.drain(3 * time.Second)
	})

	t.Run("5-truncate-and-5b-cancel-after-done", func(t *testing.T) {
		g := grokOpen(t, key, "p5", grokPersona, false, nil)
		defer g.close()
		g.say(hours)
		tr, ok := g.reply(30 * time.Second)
		t.Logf("[p5] reply ok=%v %+v", ok, tr)
		g.drain(500 * time.Millisecond)
		_ = g.send(map[string]any{"type": "response.cancel"})
		g.drain(2 * time.Second)
		_ = g.send(map[string]any{"type": "conversation.item.truncate", "item_id": tr.itemID,
			"content_index": 0, "audio_end_ms": 500})
		g.drain(3 * time.Second)
		_ = g.send(map[string]any{"type": "conversation.item.truncate", "item_id": "item_does_not_exist",
			"content_index": 0, "audio_end_ms": 500})
		g.drain(3 * time.Second)
	})

	t.Run("6-temperature", func(t *testing.T) {
		g := grokOpen(t, key, "p6", grokPersona, false, nil)
		defer g.close()
		_ = g.send(map[string]any{"type": "session.update", "session": map[string]any{"temperature": 0.6}})
		g.drain(3 * time.Second)
	})

	t.Run("7-input-transcription", func(t *testing.T) {
		g := grokOpen(t, key, "p7", grokPersona, false, nil)
		defer g.close()
		_ = g.send(map[string]any{"type": "session.update", "session": map[string]any{
			"audio": map[string]any{"input": map[string]any{
				"format":        map[string]any{"type": "audio/pcmu"},
				"transcription": map[string]any{"model": "grok-transcribe"}}}}})
		g.drain(1 * time.Second)
		g.say(hours)
		tr, ok := g.reply(30 * time.Second)
		t.Logf("[p7] reply ok=%v %+v", ok, tr)
		g.drain(2 * time.Second)
	})

	t.Run("8-g711-roundtrip", func(t *testing.T) {
		g := grokOpen(t, key, "p8", grokPersona, false, nil)
		defer g.close()
		g.say(hours)
		tr, ok := g.reply(30 * time.Second)
		ms := tr.audio / 8 // 8000 bytes per second of mu-law 8 kHz -> 8 bytes per ms
		t.Logf("[p8] reply ok=%v transcript=%q (%d runes) audio=%d bytes = %d ms mu-law@8k",
			ok, tr.text, len([]rune(tr.text)), tr.audio, ms)
		if !ok || tr.audio == 0 {
			t.Errorf("no audio came back for a pcmu session: %+v", tr)
		}
	})
}
