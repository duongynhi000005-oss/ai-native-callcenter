// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/base64"
	"testing"
)

// itemIDsOf lists the item ids of the messages given.
func itemIDsOf(messages []map[string]any) []string {
	ids := make([]string, 0, len(messages))
	for _, m := range messages {
		id, _ := m["item_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// The #286 live call 01a16256-ddd5-…, replayed on the wire: the wait line was
// said, the tool's result arrived and its answer was asked for as the caller
// started to speak over the line's end, and qwen's turn detection cancelled the
// answer's response with its transcript written and none of its audio sent. The
// caller heard nothing of the answer, so it comes out of the conversation before
// anything asks for another turn, and the line, which was heard, stays.
func TestAnAnswerCutShortBeforeAnyAudioIsTakenOutOfTheConversation(t *testing.T) {
	for _, profile := range []Profile{QwenProfile(), OpenAIProfile(), GrokProfile(), GatewayProfile()} {
		t.Run(profile.Name, func(t *testing.T) {
			f, session := startWaitLineCall(t, profile)
			sayWaitLine(t, f, session)

			// The line: said in full.
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.output_item.added",
				"item": map[string]any{"id": "item_line", "type": "message"}})
			f.send(map[string]any{"type": "response.audio.delta",
				"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
			awaitEvent(t, session, EventTypeAudioDelta)
			f.send(map[string]any{"type": "response.done", "response": map[string]any{
				"status": "completed",
				"output": []any{map[string]any{"id": "item_line", "type": "message"}},
			}})
			awaitEvent(t, session, EventTypeResponseDone)

			// The result, and its answer's turn.
			if err := session.SendToolResult("fc_1", `{"answer":"200 yuan"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			f.awaitMessages("response.create", 3)
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.output_item.added",
				"item": map[string]any{"id": "item_answer", "type": "message"}})
			f.send(map[string]any{"type": "input_audio_buffer.speech_started"})
			f.send(map[string]any{"type": "response.audio_transcript.done",
				"item_id": "item_answer", "transcript": "The early termination fee is 200 yuan, and"})
			f.send(map[string]any{"type": "response.done", "response": map[string]any{
				"status": "cancelled",
				"output": []any{map[string]any{"id": "item_answer", "type": "message"}},
			}})
			awaitEvent(t, session, EventTypeInterrupted)

			deletes := f.awaitMessages("conversation.item.delete", 1)
			if deletes[0]["item_id"] != "item_answer" {
				t.Errorf("deleted %v, want the unheard answer item_answer", deletes[0]["item_id"])
			}
			f.refuteMessage("conversation.item.truncate")
		})
	}
}

// Only the output list names the item (a dialect that skips output_item.added):
// it is just as unheard.
func TestAnUnheardItemNamedOnlyInTheOutputIsTakenOut(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SendUserText("(The caller pressed 1.)"); err != nil {
		t.Fatalf("cue: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.done", "response": map[string]any{
		"status": "cancelled",
		"output": []any{map[string]any{"id": "item_reply", "type": "message"}},
	}})
	awaitEvent(t, session, EventTypeInterrupted)
	deletes := f.awaitMessages("conversation.item.delete", 1)
	if deletes[0]["item_id"] != "item_reply" {
		t.Errorf("deleted %v, want item_reply", deletes[0]["item_id"])
	}
}

// The unheard answer is taken out before the turn owed to a later result is
// asked for, so that turn is made from a history without it.
func TestTheUnheardTurnIsTakenOutBeforeTheNextTurnIsAskedFor(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": "item_answer", "type": "message"}})
	// A cue while the answer is open owes a turn past it.
	if err := session.SendUserText("(The caller pressed 2.)"); err != nil {
		t.Fatalf("cue: %v", err)
	}
	f.send(map[string]any{"type": "response.done", "response": map[string]any{
		"status": "cancelled"}})
	awaitEvent(t, session, EventTypeInterrupted)
	f.awaitMessages("conversation.item.delete", 1)
	f.awaitMessages("response.create", 3)
	types := typesOf(f.messages())
	if deleteAt, createAt := indexOf(types, "conversation.item.delete"),
		len(types)-1-indexOf(reversed(types), "response.create"); deleteAt > createAt {
		t.Errorf("the owed turn was asked for before the unheard answer was taken out: %v", types)
	}
}

// A turn some audio of which reached the consumer is the consumer's to account
// for (Interrupt with the time played); one that ran to its end was heard.
func TestATurnWithAudioOrCompletedIsNotTakenOutHere(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		hasAudio bool
	}{
		{"cancelled with audio", statusCancelled, true},
		{"completed without audio", statusCompleted, false},
		{"completed with audio", statusCompleted, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, session := startWaitLineCall(t, QwenProfile())
			if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
			f.awaitMessages("response.create", 2)
			f.send(map[string]any{"type": "response.created"})
			awaitEvent(t, session, EventTypeResponseStarted)
			f.send(map[string]any{"type": "response.output_item.added",
				"item": map[string]any{"id": "item_answer", "type": "message"}})
			if tc.hasAudio {
				f.send(map[string]any{"type": "response.audio.delta",
					"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
				awaitEvent(t, session, EventTypeAudioDelta)
			}
			f.send(map[string]any{"type": "response.done", "response": map[string]any{
				"status": tc.status,
				"output": []any{map[string]any{"id": "item_answer", "type": "message"}},
			}})
			if tc.status == statusCancelled {
				awaitEvent(t, session, EventTypeInterrupted)
			} else {
				awaitEvent(t, session, EventTypeResponseDone)
			}
			f.refuteMessage("conversation.item.delete")
		})
	}
}

// A turn the consumer reports the caller heard none of (0 ms: its audio was
// queued behind an earlier turn's tail and flushed) is truncated, and deleted as
// well: qwen ignores the truncate. While its response is still open the delete
// waits for that response to end; the provider is still writing the item.
func TestATurnReportedUnheardIsDeleted(t *testing.T) {
	t.Run("response open", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		f.awaitMessages("response.create", 2)
		f.send(map[string]any{"type": "response.created"})
		awaitEvent(t, session, EventTypeResponseStarted)
		f.send(map[string]any{"type": "response.output_item.added",
			"item": map[string]any{"id": "item_answer", "type": "message"}})
		f.send(map[string]any{"type": "response.audio.delta",
			"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
		awaitEvent(t, session, EventTypeAudioDelta)

		if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("conversation.item.truncate", 1)
		f.awaitMessages("conversation.item.delete", 0)

		f.send(map[string]any{"type": "response.done", "response": map[string]any{
			"status": "cancelled",
			"output": []any{map[string]any{"id": "item_answer", "type": "message"}},
		}})
		awaitEvent(t, session, EventTypeInterrupted)
		deletes := f.awaitMessages("conversation.item.delete", 1)
		if deletes[0]["item_id"] != "item_answer" {
			t.Errorf("deleted %v, want item_answer", deletes[0]["item_id"])
		}
		types := typesOf(f.messages())
		if indexOf(types, "conversation.item.truncate") > indexOf(types, "conversation.item.delete") {
			t.Errorf("deleted before truncating: %v", types)
		}
	})

	t.Run("response ended", func(t *testing.T) {
		f, session := startWaitLineCall(t, OpenAIProfile())
		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		f.awaitMessages("response.create", 2)
		f.send(map[string]any{"type": "response.created"})
		awaitEvent(t, session, EventTypeResponseStarted)
		f.send(map[string]any{"type": "response.output_item.added",
			"item": map[string]any{"id": "item_answer", "type": "message"}})
		f.send(map[string]any{"type": "response.audio.delta",
			"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
		awaitEvent(t, session, EventTypeAudioDelta)
		f.send(map[string]any{"type": "response.done", "response": map[string]any{
			"status": "completed"}})
		awaitEvent(t, session, EventTypeResponseDone)

		if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("conversation.item.truncate", 1)
		deletes := f.awaitMessages("conversation.item.delete", 1)
		if ids := itemIDsOf(deletes); ids[0] != "item_answer" {
			t.Errorf("deleted %v, want item_answer", ids)
		}
	})

	t.Run("partly heard", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		f.awaitMessages("response.create", 2)
		f.send(map[string]any{"type": "response.created"})
		awaitEvent(t, session, EventTypeResponseStarted)
		f.send(map[string]any{"type": "response.output_item.added",
			"item": map[string]any{"id": "item_answer", "type": "message"}})
		f.send(map[string]any{"type": "response.audio.delta",
			"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
		awaitEvent(t, session, EventTypeAudioDelta)
		f.send(map[string]any{"type": "response.done", "response": map[string]any{
			"status": "completed"}})
		awaitEvent(t, session, EventTypeResponseDone)

		if err := session.Interrupt(InterruptReasonSpeech, 300); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("conversation.item.truncate", 1)
		f.refuteMessage("conversation.item.delete")
	})
}
