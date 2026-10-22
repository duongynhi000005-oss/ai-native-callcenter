// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
)

// The caller speaks over the end of the wait line, as on #286 live call
// 01a16256-ddd5-73b7-9335-13322c315bfa: the tool's answer is asked for as the
// caller starts to speak, qwen's turn detection cancels that response with
// its transcript written and none of its audio sent, and the reply to the
// caller is the next thing heard. Before removeUnheardTurn that reply took the
// unheard answer as given, "在的，请问有什么可以帮您？" 3 of 3 (qwen-findings
// W-Q9); with it the reply gave the result 3 of 3. The reply must give it.
//
// Same session and clock as TestLiveQwenWaitLine (testdata/line_session_zh.json,
// q_contract, the line 1 s after the model's call, the result 3 s after it).
// The caller's words, testdata/q_are_you_there.pcm, "哎，喂，你在吗？"
// (qwen3-tts-flash, voice Cherry, 16 kHz mono PCM16, silence trimmed), start
// AICC_LIVE_SPEAK_AT after the call (default 3050ms: 50 ms after the result,
// which put speech_started 180-630 ms after the answer's response.created in
// the three runs with the fix). A run whose answer was not cut before its audio is reported and
// proves nothing either way.
//
//	AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveQwenSpeechOverTheAnswer -v ./internal/provider/
func TestLiveQwenSpeechOverTheAnswer(t *testing.T) {
	profile := QwenProfile()
	requireLive(t, profile)
	runs := 3
	if v, err := strconv.Atoi(os.Getenv("AICC_LIVE_RUNS")); err == nil && v > 0 {
		runs = v
	}
	speakAt := 3050 * time.Millisecond
	if v, err := time.ParseDuration(os.Getenv("AICC_LIVE_SPEAK_AT")); err == nil {
		speakAt = v
	}
	var fixture struct {
		Instructions string
		Announce     string
		Tools        []ToolSpec
	}
	data, err := os.ReadFile("testdata/line_session_zh.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	input, output := profile.FormatsFor(media.LawMu)
	question := readMidClip(t, input, "q_contract")
	overTheLine := readMidClip(t, input, "q_are_you_there")

	cut, given := 0, 0
	for run := 1; run <= runs; run++ {
		tap := &liveLogTap{}
		c := openLiveCall(t, profile, slog.New(tap), SessionConfig{
			Instructions: fixture.Instructions,
			OpeningText:  fixture.Announce,
			Language:     "zh",
			Turn:         DefaultTurnDetection(),
			InputFormat:  input,
			OutputFormat: output,
			Tools:        fixture.Tools,
		})
		r := c.speechOverTheAnswerRound(t, question, overTheLine, speakAt)
		taken := tap.count("took a turn the caller heard none of")
		tap.print(t)
		c.close(t)
		t.Logf("[#%d] answer cut before audio=%v (written %q) taken out=%d; reply %q",
			run, r.isAnswerCut, r.cutText, taken, r.replyText)
		if !r.isAnswerCut {
			continue
		}
		cut++
		if taken == 0 {
			t.Errorf("[#%d] the unheard answer was not taken out of the conversation", run)
		}
		if strings.Contains(r.replyText, "200") {
			given++
		} else {
			t.Errorf("[#%d] the reply did not give the result: %q", run, r.replyText)
		}
	}
	t.Logf("[qwen] RESULT answer cut before audio %d/%d, result given in the reply %d/%d",
		cut, runs, given, cut)
	if cut == 0 {
		t.Log("no run cut the answer before its audio; move AICC_LIVE_SPEAK_AT")
	}
}

type speechOverTheAnswerRun struct {
	isAnswerCut bool   // the answer's turn ended cancelled with no audio
	cutText     string // what that turn had written
	replyText   string // the next turn with audio after it
}

// speechOverTheAnswerRound plays the greeting out, asks the question, says
// the wait line over the slow tool and sends its result on the call's clock,
// and has the caller speak at speakAt after the model's call.
func (c *midCall) speechOverTheAnswerRound(t *testing.T, question, overTheLine []byte,
	speakAt time.Duration) speechOverTheAnswerRun {
	t.Helper()
	opening := c.expectReply(t, "opening")
	t.Logf("opening: %q", opening.text)
	c.say(question)

	var r speechOverTheAnswerRun
	var call Event
	var calledAt time.Time
	var lineAt, resultAt, callerAt, quietAt <-chan time.Time
	isResultSent := false
	turn := ""
	audio := 0
	text := ""
	since := func() time.Duration { return time.Since(calledAt).Round(time.Millisecond) }
	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("no end within 60 s (reply %q)", r.replyText)
		case <-quietAt:
			return r
		case <-lineAt:
			lineAt = nil
			if err := c.session.SpeakText(liveWaitLine, false); err != nil {
				t.Fatalf("speak: %v", err)
			}
		case <-callerAt:
			callerAt = nil
			t.Logf("T+%v the caller starts speaking", since())
			c.mu.Lock()
			c.queue = append(c.queue, overTheLine...)
			c.mu.Unlock()
		case <-resultAt:
			resultAt = nil
			isResultSent = true
			t.Logf("T+%v the result is sent", since())
			if err := c.session.SendToolResult(call.ToolCallID,
				`{"answer":"合约期内变更套餐需支付200元违约金，由人工客服办理。","found":"1","ok":"1"}`); err != nil {
				t.Fatalf("send tool result: %v", err)
			}
		case ev, ok := <-c.session.Events():
			if !ok {
				t.Fatal("the session ended")
			}
			switch ev.Type {
			case EventTypeToolCall:
				if call.ToolCallID == "" {
					call, calledAt = ev, time.Now()
					lineAt = time.After(time.Second)
					resultAt = time.After(3 * time.Second)
					callerAt = time.After(speakAt)
				}
			case EventTypeSpeechStarted:
				if !calledAt.IsZero() {
					t.Logf("T+%v speech started", since())
				}
			case EventTypeResponseStarted:
				quietAt = nil
				audio, text = 0, ""
				switch {
				case ev.IsSpokenLine:
					turn = "line"
				case isResultSent:
					turn = "after the result"
				default:
					turn = "before the result"
				}
				if !calledAt.IsZero() {
					t.Logf("T+%v turn started: %s", since(), turn)
				}
			case EventTypeAudioDelta:
				audio += len(ev.Audio)
			case EventTypeOutputTranscript:
				if ev.IsFinal {
					text += ev.Text
				}
			case EventTypeInterrupted, EventTypeResponseDone:
				if calledAt.IsZero() {
					break
				}
				t.Logf("T+%v turn ended: %s %s audio=%d text=%q", since(), turn, ev.Type, audio, text)
				if turn != "after the result" {
					break
				}
				switch {
				case ev.Type == EventTypeInterrupted && audio == 0 && r.replyText == "":
					if !r.isAnswerCut {
						r.isAnswerCut, r.cutText = true, text
					}
				case audio > 0 && r.replyText == "":
					r.replyText = text
				}
				quietAt = time.After(5 * time.Second)
			case EventTypeError:
				t.Logf("provider error (fatal=%v): %v", ev.IsFatal, ev.Err)
			}
		}
	}
}
