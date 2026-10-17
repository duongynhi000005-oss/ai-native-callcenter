// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// Grok speaks the OpenAI Realtime protocol with a few dialect quirks that the
// one client has to absorb without a branch on the provider's name
// (docs/design/grok-findings.md). These tests replay a recorded event stream
// with every one of them.

// syncBuffer is a log sink the read loop writes to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// loggedSession is testSession with a debug log the test can read, because the
// "unmapped provider event" line is logged at debug.
func loggedSession(t *testing.T, f *fakeProvider, profile Profile) (*Realtime, *syncBuffer) {
	t.Helper()
	t.Setenv(profile.APIKeyEnv, "test-key")
	profile.Endpoint = f.endpoint()
	logs := &syncBuffer{}
	session, err := New(profile, slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close(t.Context()) })
	return session, logs
}

func grokConfig() SessionConfig {
	cfg := basicConfig()
	cfg.InputFormat, cfg.OutputFormat = GrokProfile().FormatsFor(media.LawMu)
	return cfg
}

// The long Chinese sentence of the replay: 51 characters.
const grokSentence = "你好我想查一下我上个月的宽带账单为什么多收了二十块钱请帮我核对一下另外我还想问问升级套餐需要多少钱谢谢"

// cumulativeUpdates are the running texts of grokSentence, one per two
// characters, as .updated delivers them.
func cumulativeUpdates() []string {
	runes := []rune(grokSentence)
	var out []string
	for n := 3; n < len(runes); n += 2 {
		out = append(out, string(runes[:n]))
	}
	return append(out, grokSentence)
}

func transcriptionEvent(kind, itemID, text, status string) map[string]any {
	event := map[string]any{
		"type":       "conversation.item.input_audio_transcription." + kind,
		"item_id":    itemID,
		"transcript": text,
	}
	if status != "" {
		event["status"] = status
	}
	return event
}

// drainUntil collects events until the marker speech event arrives.
func drainUntil(t *testing.T, session *Realtime, marker EventType) []Event {
	t.Helper()
	var got []Event
	deadline := time.After(3 * time.Second)
	for {
		select {
		case event, ok := <-session.Events():
			if !ok {
				t.Fatalf("event stream closed before %s (saw %v)", marker, got)
			}
			if event.Type == marker {
				return got
			}
			got = append(got, event)
		case <-deadline:
			t.Fatalf("no %s event arrived (saw %v)", marker, got)
		}
	}
}

func TestAGrokEventStreamIsAbsorbedWithoutNoise(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session, logs := loggedSession(t, f, GrokProfile())
	if err := session.Start(t.Context(), grokConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)

	ping := func(n int) {
		f.send(map[string]any{"type": "ping", "timestamp": 1760000000 + n})
	}

	// A reply with pings in the middle of it.
	ping(0)
	f.send(map[string]any{"type": "response.created"})
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item-1", "type": "message"}})
	ping(1)
	f.send(map[string]any{"type": "response.output_audio.delta", "delta": "AAEC"})
	ping(2)
	f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})

	// The first utterance: cumulative updates (15 or more), then .completed repeated while it is
	// settling, then the final one.
	updates := cumulativeUpdates()
	if len(updates) < 15 {
		t.Fatalf("the replay has %d updates, want 15 or more", len(updates))
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	for i, text := range updates {
		f.send(transcriptionEvent("updated", "user-1", text, ""))
		if i%4 == 0 {
			ping(10 + i)
		}
	}
	f.send(transcriptionEvent("completed", "user-1", updates[len(updates)-2], "in_progress"))
	f.send(transcriptionEvent("completed", "user-1", grokSentence, "in_progress"))
	f.send(transcriptionEvent("completed", "user-1", grokSentence, "completed"))

	// A second utterance in OpenAI's shape: no status at all means final.
	f.send(transcriptionEvent("completed", "user-2", "好的谢谢", ""))
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})

	got := drainUntil(t, session, EventTypeSpeechStopped)

	var finals []string
	for _, event := range got {
		switch event.Type {
		case EventTypeInputTranscript:
			if !event.IsFinal {
				t.Errorf("a partial caller transcript %q reached the call", event.Text)
				continue
			}
			finals = append(finals, event.Text)
		case EventTypeError:
			t.Errorf("the session reported an error: %v", event.Err)
		}
	}
	want := []string{grokSentence, "好的谢谢"}
	if fmt.Sprint(finals) != fmt.Sprint(want) {
		t.Errorf("caller lines = %q, want one per utterance %q", finals, want)
	}
	if n := len([]rune(grokSentence)); n < 40 {
		t.Errorf("the replayed sentence has %d characters, want 40 or more", n)
	}
	if strings.Contains(logs.String(), "unmapped") {
		t.Errorf("an event was logged as unmapped:\n%s", logs.String())
	}
}

// Both answers to the question "does the provider end its own response when it
// hears the caller" go through the same code: where it does, speech sends no
// cancel; where it does not, a cancel goes out. Grok's refusal of a cancel with
// nothing active, and its silence for a truncate of an unknown item, are
// neither a session failure nor an error event.
func TestGrokAnswersToACancelWithNothingActiveAndATruncateOfAnUnknownItem(t *testing.T) {
	for _, tc := range []struct {
		name          string
		cancelsItself bool
		wantCancels   int
	}{
		{"the provider cancels itself", true, 0},
		{"the client cancels", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := GrokProfile()
			profile.CancelsResponseItself = tc.cancelsItself
			f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
				acceptSession(f, message)
				if message["type"] == "response.cancel" {
					f.send(map[string]any{"type": "error", "error": map[string]any{
						"type": "invalid_request_error", "code": "invalid_request_error",
						"message": "Cancellation failed: no active response found",
					}})
				}
			})
			session, logs := loggedSession(t, f, profile)
			if err := session.Start(t.Context(), grokConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.output_item.added",
				"item": map[string]any{"id": "unknown-to-the-server", "type": "message"}})
			// Audio after the item, so the item is known when Interrupt reads it.
			f.send(map[string]any{"type": "response.output_audio.delta", "delta": "AAEC"})
			awaitEvent(t, session, EventTypeAudioDelta)
			// The response ends on the provider's side in the same breath as
			// the barge-in, so the cancel meets nothing.
			if err := session.Interrupt(InterruptReasonSpeech, 300); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			f.awaitMessages("response.cancel", tc.wantCancels)
			truncates := f.awaitMessages("conversation.item.truncate", 1)
			if truncates[0]["item_id"] != "unknown-to-the-server" {
				t.Errorf("truncate item = %v", truncates[0]["item_id"])
			}

			// The marker: anything the refusal produced is in front of it.
			f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
			for _, event := range drainUntil(t, session, EventTypeSpeechStarted) {
				if event.Type == EventTypeError || event.IsFatal {
					t.Errorf("unexpected %s event: %v", event.Type, event.Err)
				}
			}
			// The session carries on.
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			if strings.Contains(logs.String(), "unmapped") {
				t.Errorf("an event was logged as unmapped:\n%s", logs.String())
			}
		})
	}
}

// The same words from a vendor code that is not one of ours stay an error, and
// a cancel nobody sent is still reported: the widening to Grok's generic code
// is narrow.
func TestAGenericErrorCodeIsNotSwallowedWithoutOurCancel(t *testing.T) {
	f := newFakeProvider(t, acceptSession)
	session, _ := loggedSession(t, f, GrokProfile())
	if err := session.Start(t.Context(), grokConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	f.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_request_error",
		"message": "Cancellation failed: no active response found",
	}})
	awaitEvent(t, session, EventTypeError)
}

// The traits that are not measured yet are profile values; each value has to
// work, so flipping one is a one-line change and a golden regeneration.
func TestGrokTraitsWorkInBothValues(t *testing.T) {
	temperature := 0.4
	for _, isAccepted := range []bool{false, true} {
		profile := GrokProfile()
		profile.AcceptsTemperature = isAccepted
		client := &Realtime{profile: profile}
		for _, isReduced := range []bool{false, true} {
			session := client.buildSessionUpdate(SessionConfig{Temperature: &temperature}, isReduced)["session"].(map[string]any)
			_, isPresent := session["temperature"]
			if want := isAccepted && !isReduced; isPresent != want {
				t.Errorf("AcceptsTemperature=%v reduced=%v: temperature present = %v, want %v",
					isAccepted, isReduced, isPresent, want)
			}
		}
	}

	for _, model := range []string{"", "grok-transcribe"} {
		profile := GrokProfile()
		profile.TranscribeModel = model
		client := &Realtime{profile: profile}
		session := client.buildSessionUpdate(grokConfig(), false)["session"].(map[string]any)
		input := nested(t, session, "audio", "input").(map[string]any)
		transcription, isPresent := input["transcription"]
		if isPresent != (model != "") {
			t.Errorf("TranscribeModel=%q: transcription present = %v", model, isPresent)
		}
		if isPresent && transcription.(map[string]any)["model"] != model {
			t.Errorf("transcription = %v, want model %s", transcription, model)
		}
	}
}

func TestTheGrokProfileIsTheMeasuredOne(t *testing.T) {
	profile, err := ProfileFor(" Grok ", Override{})
	if err != nil {
		t.Fatalf("grok is not a provider this build can run: %v", err)
	}
	if profile.Name != NameGrok || profile.Endpoint != "wss://api.x.ai/v1/realtime" ||
		profile.Model != "grok-voice-3.0" || profile.APIKeyEnv != "XAI_API_KEY" ||
		profile.Style != styleGA || !profile.AcceptsG711 || profile.Voice == "" {
		t.Errorf("profile = %+v", profile)
	}
	if profile.NeedsCueForFirstTurn || profile.NeedsDirectedLineInConversation {
		t.Error("grok greets from the instructions: no cue, no directed line")
	}
	if got := profile.endpointURL(); got != "wss://api.x.ai/v1/realtime?model=grok-voice-3.0" {
		t.Errorf("endpoint URL = %s", got)
	}
	for _, law := range []media.Law{media.LawMu, media.LawAlaw} {
		in, out := profile.FormatsFor(law)
		if in != media.G711Format(law) || out != media.G711Format(law) {
			t.Errorf("%s formats = %s / %s, want a G.711 passthrough", law, in, out)
		}
	}
}

// A 101 upgrade is not proof of a valid key: the provider may accept the
// handshake and refuse the first frame. Start returns only once session.updated
// has arrived, so a refused session is an error here, which is what `aicc
// doctor` relies on.
func TestStartFailsWhenTheProviderRefusesTheSessionAfterTheUpgrade(t *testing.T) {
	f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
		if message["type"] == "session.update" {
			f.send(map[string]any{"type": "error", "error": map[string]any{
				"type": "invalid_request_error", "code": "invalid_api_key",
				"message": "Incorrect API key provided",
			}})
		}
	})
	session, _ := loggedSession(t, f, GrokProfile())
	err := session.Start(t.Context(), grokConfig())
	if err == nil || !strings.Contains(err.Error(), "Incorrect API key") {
		t.Fatalf("start = %v, want the provider's refusal", err)
	}
}

// A line asked for mid-call reaches the engine as a caller item first, then the
// override, when the engine drops the override alone once the conversation has
// a turn (IgnoresOverrideOnceTalking); where it does not, the closing line
// follows NeedsDirectedLineInConversation and any other line is the override
// alone. The inserted item is a text item: it must not become caller speech.
func TestAMidCallLineIsPutInTheConversationOnlyWhereTheEngineNeedsIt(t *testing.T) {
	for _, tc := range []struct {
		name                string
		ignoresOverride     bool
		directedWhenClosing bool
		isClosing           bool
		wantItem            bool
	}{
		{"ignores the override, phase line", true, false, false, true},
		{"ignores the override, closing line", true, false, true, true},
		{"honours the override, phase line", false, false, false, false},
		{"honours the override, closing line", false, false, true, false},
		{"directed closing only, phase line", false, true, false, false},
		{"directed closing only, closing line", false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := GrokProfile()
			profile.IgnoresOverrideOnceTalking = tc.ignoresOverride
			profile.NeedsDirectedLineInConversation = tc.directedWhenClosing
			f := newFakeProvider(t, acceptSession)
			session, _ := loggedSession(t, f, profile)
			if err := session.Start(t.Context(), grokConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)
			// The opening turn, finished.
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)

			// The fake records frames on its own read goroutine, so the
			// opening response.create may not be on record yet. Wait for it
			// (and that it is the only one) before counting what SpeakText adds.
			f.awaitMessages("response.create", 1)
			before := len(f.messages())
			if err := session.SpeakText("Thanks for calling.", tc.isClosing); err != nil {
				t.Fatalf("speak: %v", err)
			}
			wantFrames := 1
			if tc.wantItem {
				wantFrames = 2
			}
			deadline := time.Now().Add(2 * time.Second)
			for len(f.messages())-before < wantFrames && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(100 * time.Millisecond)
			sent := f.messages()[before:]
			if len(sent) != wantFrames {
				t.Fatalf("sent %v, want %d frames", typesOf(sent), wantFrames)
			}
			last := sent[len(sent)-1]
			if last["type"] != "response.create" || last["response"] == nil {
				t.Errorf("the last frame is %v, want the response.create carrying the override", last)
			}
			if tc.wantItem {
				item := sent[0]
				if item["type"] != "conversation.item.create" {
					t.Fatalf("the first frame is %v, want the caller item ahead of the request", item["type"])
				}
				if role := nested(t, item, "item", "role"); role != "user" {
					t.Errorf("item role = %v", role)
				}
			}

			// The provider echoing the text item back is not caller speech.
			f.send(map[string]any{"type": "conversation.item.added",
				"item": map[string]any{"id": "u1", "type": "message", "role": "user"}})
			f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
			for _, event := range drainUntil(t, session, EventTypeSpeechStarted) {
				if event.Type == EventTypeInputTranscript {
					t.Errorf("the inserted item became caller speech: %q", event.Text)
				}
			}
		})
	}
}

func TestGrokPutsEveryMidCallLineInTheConversation(t *testing.T) {
	if !GrokProfile().IgnoresOverrideOnceTalking {
		t.Error("grok drops a per-response override once the conversation has a turn (findings 5.2)")
	}
	for _, p := range []Profile{OpenAIProfile(), QwenProfile(), GatewayProfile(), DoubaoProfile()} {
		if p.IgnoresOverrideOnceTalking {
			t.Errorf("%s: IgnoresOverrideOnceTalking is grok only", p.Name)
		}
	}
}
