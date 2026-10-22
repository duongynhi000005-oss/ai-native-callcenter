// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/base64"
	"testing"
	"time"
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

// notesIn lists the unheard-result notes the client put in the conversation.
func notesIn(f *fakeProvider) []string {
	var notes []string
	for _, m := range f.messagesOfType("conversation.item.create") {
		item, _ := m["item"].(map[string]any)
		content, _ := item["content"].([]any)
		for _, c := range content {
			part, _ := c.(map[string]any)
			text, _ := part["text"].(string)
			if text == unheardResultNote("en") {
				notes = append(notes, text)
			}
		}
	}
	return notes
}

// awaitNotes waits until n notes were sent, then checks no more follow.
func awaitNotes(t *testing.T, f *fakeProvider, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(notesIn(f)) < n && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(notesIn(f)); got != n {
		t.Fatalf("the client sent %d unheard-result notes, want %d; it sent %v",
			got, n, typesOf(f.messages()))
	}
}

// answerTurn starts the turn that answers a tool result, with a spoken item.
func answerTurn(t *testing.T, f *fakeProvider, session *Realtime, itemID string) {
	t.Helper()
	f.send(map[string]any{"type": "response.created"})
	awaitEvent(t, session, EventTypeResponseStarted)
	f.send(map[string]any{"type": "response.output_item.added",
		"item": map[string]any{"id": itemID, "type": "message"}})
}

// sendAudio hands the client one chunk of the open turn's audio.
func sendAudio(t *testing.T, f *fakeProvider, session *Realtime) {
	t.Helper()
	f.send(map[string]any{"type": "response.audio.delta",
		"delta": base64.StdEncoding.EncodeToString(make([]byte, 320))})
	awaitEvent(t, session, EventTypeAudioDelta)
}

// finishAnswerTurn ends the open turn with a status and waits for the client to say so.
func finishAnswerTurn(t *testing.T, f *fakeProvider, session *Realtime, status string) {
	t.Helper()
	f.send(map[string]any{"type": "response.done", "response": map[string]any{"status": status}})
	if status == statusCancelled {
		awaitEvent(t, session, EventTypeInterrupted)
	} else {
		awaitEvent(t, session, EventTypeResponseDone)
	}
}

// Live call 01a1627d-7eae-77c2-98f7-e997fca088da: the answer was cut before
// its audio and taken out, and the reply to the caller still took it as given.
// The model is told, once, that the caller has not heard the result, after the
// item is gone and before anything asks for a turn; a second unheard turn (the
// provider's reply cut by the caller's next words) does not repeat it.
func TestAnUnheardAnswerIsNotedOnceForTheReply(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	answerTurn(t, f, session, "item_answer")
	// A cue while the answer is open owes a turn past it.
	if err := session.SendUserText("(The caller pressed 2.)"); err != nil {
		t.Fatalf("cue: %v", err)
	}
	finishAnswerTurn(t, f, session, statusCancelled)
	awaitNotes(t, f, 1)
	f.awaitMessages("response.create", 3)
	types := typesOf(f.messages())
	deleteAt := indexOf(types, "conversation.item.delete")
	lastCreate := len(types) - 1 - indexOf(reversed(types), "response.create")
	lastItem := len(types) - 1 - indexOf(reversed(types), "conversation.item.create")
	if deleteAt > lastItem || lastItem > lastCreate {
		t.Errorf("want the delete, then the note, then the owed turn: %v", types)
	}

	// The owed turn is cut before its audio too.
	answerTurn(t, f, session, "item_again")
	finishAnswerTurn(t, f, session, statusCancelled)
	f.awaitMessages("conversation.item.delete", 2)
	awaitNotes(t, f, 1)
}

// A turn that is not an answer to a result is taken out unheard with no note:
// a cue's reply with no result waiting, and the turn of a line.
func TestOnlyAnUnheardAnswerIsNoted(t *testing.T) {
	t.Run("cue reply", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		if err := session.SendUserText("(The caller pressed 1.)"); err != nil {
			t.Fatalf("cue: %v", err)
		}
		f.awaitMessages("response.create", 2)
		answerTurn(t, f, session, "item_reply")
		finishAnswerTurn(t, f, session, statusCancelled)
		f.awaitMessages("conversation.item.delete", 1)
		awaitNotes(t, f, 0)
	})
	t.Run("line", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		sayWaitLine(t, f, session)
		// The result arrives while the line's request is in flight: its turn
		// is owed past the line, and the line's turn is not its answer.
		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		answerTurn(t, f, session, "item_line")
		finishAnswerTurn(t, f, session, statusCancelled)
		f.awaitMessages("conversation.item.delete", 1)
		awaitNotes(t, f, 0)
	})
}

// An answer whose turn is stopped to make room for a line is taken out unheard
// with no note: the line is the next turn and must say the line alone.
func TestAnAnswerStoppedForALineIsNotNoted(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	answerTurn(t, f, session, "item_answer")
	if err := session.SpeakText("Let me put you through.", false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("response.cancel", 1)
	finishAnswerTurn(t, f, session, statusCancelled)
	f.awaitMessages("conversation.item.delete", 1)
	f.awaitMessages("response.create", 3)
	awaitNotes(t, f, 0)
}

// An answer that ran to its end with its audio handed out was delivered: a
// later turn cut before its audio is taken out with no note. Until a new
// result arrives; that one is noted again if its answer goes unheard.
func TestADeliveredAnswerIsNotNotedAndANewResultIs(t *testing.T) {
	f, session := startWaitLineCall(t, QwenProfile())
	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 2)
	answerTurn(t, f, session, "item_answer")
	sendAudio(t, f, session)
	finishAnswerTurn(t, f, session, statusCompleted)

	if err := session.SendUserText("(The caller pressed 1.)"); err != nil {
		t.Fatalf("cue: %v", err)
	}
	f.awaitMessages("response.create", 3)
	answerTurn(t, f, session, "item_reply")
	finishAnswerTurn(t, f, session, statusCancelled)
	f.awaitMessages("conversation.item.delete", 1)
	awaitNotes(t, f, 0)

	if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
	f.awaitMessages("response.create", 4)
	answerTurn(t, f, session, "item_second")
	finishAnswerTurn(t, f, session, statusCancelled)
	f.awaitMessages("conversation.item.delete", 2)
	awaitNotes(t, f, 1)
}

// An answer flushed before a frame of it played (Interrupt, 0 ms) is noted
// after it is deleted; a turn that is not an answer is deleted with no note.
func TestAnAnswerFlushedUnplayedIsNoted(t *testing.T) {
	t.Run("answer", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		if err := session.SendToolResult("fc_1", `{"ok":"1"}`); err != nil {
			t.Fatalf("send tool result: %v", err)
		}
		f.awaitMessages("response.create", 2)
		answerTurn(t, f, session, "item_answer")
		sendAudio(t, f, session)
		finishAnswerTurn(t, f, session, statusCompleted)
		if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("conversation.item.delete", 1)
		awaitNotes(t, f, 1)
	})
	t.Run("not an answer", func(t *testing.T) {
		f, session := startWaitLineCall(t, QwenProfile())
		if err := session.SendUserText("(The caller pressed 1.)"); err != nil {
			t.Fatalf("cue: %v", err)
		}
		f.awaitMessages("response.create", 2)
		answerTurn(t, f, session, "item_reply")
		sendAudio(t, f, session)
		finishAnswerTurn(t, f, session, statusCompleted)
		if err := session.Interrupt(InterruptReasonSpeech, 0); err != nil {
			t.Fatalf("interrupt: %v", err)
		}
		f.awaitMessages("conversation.item.delete", 1)
		awaitNotes(t, f, 0)
	})
}
