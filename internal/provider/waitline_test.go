// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
	"time"
)

// A wait line (#286) is said while a slow tool runs, with the tool's call still
// unanswered, and the tool's result can arrive at any point of the line: before
// the provider has created the line's response, while it is being made, or
// after the caller has talked over it. Whenever it arrives, the line is not
// cut, the result is not lost, and the model's answer is a turn of its own
// after the line.

const waitLine = "One moment, let me check that."

// startWaitLineCall starts a session, plays the greeting out and has the model
// call a tool in a turn of its own that has ended, as a slow lookup leaves it.
func startWaitLineCall(t *testing.T, profile Profile) (*fakeProvider, *Realtime) {
	t.Helper()
	f := newFakeProvider(t, acceptSession)
	session := testSession(t, f, profile)
	if err := session.Start(t.Context(), basicConfig()); err != nil {
		t.Fatalf("start: %v", err)
	}
	awaitEvent(t, session, EventTypeSessionReady)
	finishTheOpeningTurn(t, f, session)

	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.function_call_arguments.done",
		"call_id": "fc_1", "name": "transfer_to_agent", "arguments": `{}`})
	awaitEvent(t, session, EventTypeToolCall)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "completed"}})
	awaitEvent(t, session, EventTypeResponseDone)
	return f, session
}

// sayWaitLine asks for the line and waits for its request, the call's second.
func sayWaitLine(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	if err := session.SpeakText(waitLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	requests := f.awaitMessages("response.create", 2)
	if direction, _ := nested(t, requests[1], "response", "instructions").(string); !strings.Contains(direction, waitLine) {
		t.Fatalf("the second request is not the line: %v", requests[1])
	}
}

// assertAnsweredAfter checks that the third request, the result's turn, went
// out after the result it answers and carries no line of its own.
func assertAnsweredAfter(t *testing.T, f *fakeProvider) {
	t.Helper()
	requests := f.awaitMessages("response.create", 3)
	if _, ok := requests[2]["response"]; ok {
		t.Errorf("the result's turn carries an override: %v", requests[2])
	}
	types := typesOf(f.messages())
	lastRequest := len(types) - 1 - indexOf(reversed(types), "response.create")
	lastItem := len(types) - 1 - indexOf(reversed(types), "conversation.item.create")
	if lastRequest < lastItem {
		t.Errorf("the result's turn was asked for ahead of the result: %v", types)
	}
}

// The result arrives after the line was asked for and before the provider
// created it. The line's request does not cover it, as a tool turn's would: the
// line's turn says the line and answers nothing.
func TestAResultArrivingBeforeTheWaitLineStartsIsAnsweredAfterIt(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session := startWaitLineCall(t, profile)
			sayWaitLine(t, f, session)

			if err := session.SendToolResult("fc_1", `{"ok":"1","answer":"Your order shipped."}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			awaitOutputs(t, f, 1)
			// No request on top of the line's: it would be refused.
			f.awaitMessages("response.create", 2)

			f.send(map[string]any{"type": "response.created"})
			if started := awaitEvent(t, session, EventTypeResponseStarted); !started.IsSpokenLine {
				t.Error("the line's response is not reported as a spoken line")
			}
			// The line's turn existing is not the answer's.
			f.awaitMessages("response.create", 2)
			f.awaitMessages("response.cancel", 0)

			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)
			assertAnsweredAfter(t, f)

			f.send(map[string]any{"type": "response.created"})
			if started := awaitEvent(t, session, EventTypeResponseStarted); started.IsSpokenLine {
				t.Error("the answer's response is reported as a spoken line")
			}
		})
	}
}

// The result arrives while the line is being said: it waits for the line's
// response to end and is not allowed to cancel it.
func TestAResultArrivingMidLineIsAnsweredAfterTheLine(t *testing.T) {
	for _, profile := range []Profile{OpenAIProfile(), QwenProfile(), GrokProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session := startWaitLineCall(t, profile)
			if err := session.SpeakText(waitLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
			f.awaitMessages("response.create", 2)
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
			awaitEvent(t, session, EventTypeAudioDelta)

			if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			f.awaitMessages("response.create", 2)
			f.awaitMessages("response.cancel", 0)

			f.send(map[string]any{"type": "response.done",
				"response": map[string]any{"status": "completed"}})
			awaitEvent(t, session, EventTypeResponseDone)
			assertAnsweredAfter(t, f)
		})
	}
}

// A caller who talks over the line, and stops, still gets the result: the
// cancelled line's end asks for the result's turn.
func TestACallerWhoTalksOverTheWaitLineStillGetsTheResult(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SpeakText(waitLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_line", "type": "message"}})
	f.send(map[string]any{"type": "response.audio.delta", "delta": "AAAA"})
	awaitEvent(t, session, EventTypeAudioDelta)

	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
	awaitEvent(t, session, EventTypeSpeechStarted)
	if err := session.Interrupt(InterruptReasonSpeech, 400); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	f.send(map[string]any{"type": "input_audio_buffer.speech_stopped"})
	awaitEvent(t, session, EventTypeSpeechStopped)
	f.send(map[string]any{"type": "response.done",
		"response": map[string]any{"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)

	assertAnsweredAfter(t, f)
}

// A line whose request the provider dropped is asked for once more, and a
// result that arrived while it was on its way is still owed past the retry:
// the retried line is the same line, and answers no result either.
func TestAResultOwedPastADroppedWaitLineSurvivesItsRetry(t *testing.T) {
	f, session, _ := startUnansweredCall(t, QwenProfile(), basicConfig(), createOn(3, 4))
	if err := session.SpeakText(waitLine, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.create", 2)
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}

	// The line's request is never created; its retry is.
	requests := f.awaitMessages("response.create", 3)
	if direction, _ := nested(t, requests[2], "response", "instructions").(string); !strings.Contains(direction, waitLine) {
		t.Fatalf("the retry is not the line: %v", requests[2])
	}
	if _, _, isOwed := floorState(session); !isOwed {
		t.Fatal("the retried line discharged the result's turn")
	}
	endTurn(t, f, session)

	requests = f.awaitMessages("response.create", 4)
	if _, ok := requests[3]["response"]; ok {
		t.Errorf("the result's turn carries an override: %v", requests[3])
	}
	endTurn(t, f, session)
}

// A line refused outright leaves no line's turn to wait for: a result owed past
// it is asked for at once rather than stranded.
func TestAResultOwedPastARefusedWaitLineIsAskedForAtOnce(t *testing.T) {
	f, session := startWaitLineCall(t, OpenAIProfile())
	sayWaitLine(t, f, session)
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)

	f.send(map[string]any{"type": "error", "error": map[string]any{
		"type": "invalid_request_error", "code": "invalid_value",
		"message": "The response could not be created."}})
	awaitEvent(t, session, EventTypeError)
	assertAnsweredAfter(t, f)
}

// awaitOutputs waits for n tool results to be in the conversation.
func awaitOutputs(t *testing.T, f *fakeProvider, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(functionCallOutputs(f)) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(functionCallOutputs(f)); got != n {
		t.Fatalf("%d tool results in the conversation, want %d", got, n)
	}
}
