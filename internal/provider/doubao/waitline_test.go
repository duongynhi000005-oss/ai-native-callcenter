// SPDX-License-Identifier: Apache-2.0

package doubao

import (
	"slices"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// A wait line (#286) is committed while a slow tool's call is still owed, and
// the result can complete the set at any point of it. A turn the engine starts
// pre-empts the one in flight, so the result goes out only once the line's turn
// has ended, however it ended: the line is heard whole, the answer follows it,
// and a caller who talks over the line still gets the answer.

const waitLineZH = "请稍等，我查一下。"

// startWithAToolOwed starts a session in which the model has called one tool
// and is waiting for its result.
func startWithAToolOwed(t *testing.T, f *fakeDoubao, lineStartDeadline time.Duration) *Session {
	t.Helper()
	session := testSession(t, f)
	if lineStartDeadline > 0 {
		session.lineStartDeadline = lineStartDeadline
	}
	start(t, session, testConfig())
	f.send(map[string]any{
		"type": "response.function_call_arguments.done",
		"items": []map[string]any{
			{"call_id": "call-a", "name": "search_kb", "arguments": `{"q":"refund"}`},
		}})
	expectEvents(t, session, provider.EventTypeResponseStarted,
		provider.EventTypeToolCall, provider.EventTypeResponseDone)
	return session
}

// sayTheWaitLine commits the line and checks it went out.
func sayTheWaitLine(t *testing.T, f *fakeDoubao, session *Session) {
	t.Helper()
	if err := session.SpeakText(waitLineZH, false); err != nil {
		t.Fatalf("speak: %v", err)
	}
	f.awaitMessages("speech_text_buffer.commit", 1)
}

// startTheLine has the engine begin speaking the committed line.
func startTheLine(t *testing.T, f *fakeDoubao, session *Session) {
	t.Helper()
	f.send(audioStarted(ttsTypeSpokenLine))
	events := expectEvents(t, session,
		provider.EventTypeResponseStarted, provider.EventTypeOutputTranscript)
	if !events[0].IsSpokenLine {
		t.Error("the line's turn is not reported as a spoken line")
	}
	f.send(audioDelta("AAAA"))
	expectEvents(t, session, provider.EventTypeAudioDelta)
}

// answer sends the owed call's result.
func answer(t *testing.T, session *Session) {
	t.Helper()
	if err := session.SendToolResult("call-a", `{"ok":"1","answer":"七天内可退款"}`); err != nil {
		t.Fatalf("send tool result: %v", err)
	}
}

// assertResultAfterTheLine checks the result went out once, after the line.
func assertResultAfterTheLine(t *testing.T, f *fakeDoubao) {
	t.Helper()
	f.awaitMessages("conversation.item.create", 1)
	types := f.frameTypes()
	if commit, item := slices.Index(types, "speech_text_buffer.commit"),
		slices.Index(types, "conversation.item.create"); item < commit {
		t.Errorf("the result went out ahead of the line: %v", types)
	}
}

// The result completes the set before the engine has started the line: it
// waits for the line to start and then to finish.
func TestAResultBeforeTheWaitLineStartsWaitsForTheLineToEnd(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startWithAToolOwed(t, f, 0)
	sayTheWaitLine(t, f, session)

	answer(t, session)
	f.awaitMessages("conversation.item.create", 0)

	startTheLine(t, f, session)
	f.awaitMessages("conversation.item.create", 0)

	f.send(audioDone())
	expectEvents(t, session, provider.EventTypeResponseDone)
	assertResultAfterTheLine(t, f)
}

// The result completes the set while the line is being said: the same.
func TestAResultMidWaitLineIsSentWhenTheLineEnds(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startWithAToolOwed(t, f, 0)
	sayTheWaitLine(t, f, session)
	startTheLine(t, f, session)

	answer(t, session)
	f.awaitMessages("conversation.item.create", 0)

	f.send(audioDone())
	expectEvents(t, session, provider.EventTypeResponseDone)
	assertResultAfterTheLine(t, f)
}

// A caller who talks over the line ends it (a bare response.done with the turn
// open), and the result goes out then: the engine answers the caller with the
// result in hand.
func TestACallerWhoTalksOverTheWaitLineStillGetsTheResult(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startWithAToolOwed(t, f, 0)
	sayTheWaitLine(t, f, session)
	startTheLine(t, f, session)
	answer(t, session)

	f.send(transcriptionStarted("item-1"))
	expectEvents(t, session, provider.EventTypeSpeechStarted)
	f.send(wireResponseDone())
	event := awaitEvent(t, session, provider.EventTypeInterrupted)
	if event.InterruptedBy != provider.InterruptReasonSpeech {
		t.Errorf("interruptedBy = %q, want the caller", event.InterruptedBy)
	}
	assertResultAfterTheLine(t, f)
}

// A line the engine never starts does not hold the result for ever: after
// lineStartDeadline it goes out without the line.
func TestAWaitLineThatNeverStartsReleasesTheResult(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startWithAToolOwed(t, f, 200*time.Millisecond)
	sayTheWaitLine(t, f, session)

	answer(t, session)
	f.awaitMessages("conversation.item.create", 1)
}

// With no line in the way the set goes out the moment it is complete, as it
// always has.
func TestAResultWithNoLineInTheWayGoesOutAtOnce(t *testing.T) {
	f := newFakeDoubao(t, acceptSession)
	session := startWithAToolOwed(t, f, 0)
	answer(t, session)
	f.awaitMessages("conversation.item.create", 1)
}
