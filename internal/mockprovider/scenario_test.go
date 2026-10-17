// SPDX-License-Identifier: Apache-2.0

package mockprovider_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/mockprovider"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// Scenario mode is tested the way the default mode is: with the application's
// own client, which is the only judge of whether the dialect is right.

const highLeadScript = `{"scripts":[{"name":"high","steps":[
  {"op":"SPEAK","durationSec":0.1},
  {"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH","notes":"wants a callback"}},
  {"op":"AWAIT_TOOL_RESULT"},
  {"op":"END_SESSION"}]}]}`

func scenarioServer(t *testing.T, document string) (*mockprovider.Server, string) {
	t.Helper()
	scenario, err := mockprovider.ParseScenario([]byte(document))
	if err != nil {
		t.Fatalf("parse scenario: %v", err)
	}
	server := mockprovider.New(mockprovider.Config{
		FirstAudio: 10 * time.Millisecond,
		DeltaAudio: 20 * time.Millisecond,
		Scenario:   scenario,
		Log:        quietLog(),
	})
	http := httptest.NewServer(server)
	t.Cleanup(http.Close)
	return server, http.URL
}

// toolCall is a tool request as the application's client reported it.
type toolCall struct{ id, name, args string }

// play runs one session to its end. It answers every tool call when answer is
// true, and returns what it saw: the tool calls, and whether the session was
// closed and whether it ended in a fatal error.
func play(t *testing.T, url, instructions string, answer bool) (calls []toolCall, isClosed, isFatal bool) {
	t.Helper()
	session, events := dialWith(t, url, instructions)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return calls, isClosed, isFatal
			}
			switch event.Type {
			case provider.EventTypeToolCall:
				calls = append(calls, toolCall{event.ToolCallID, event.ToolName, event.ToolArgs})
				if answer {
					if err := session.SendToolResult(event.ToolCallID, `{"ok":true}`); err != nil {
						t.Fatalf("send tool result: %v", err)
					}
				}
			case provider.EventTypeError:
				if event.IsFatal {
					isFatal = true
				}
			case provider.EventTypeClosed:
				isClosed = true
			}
		case <-deadline:
			t.Fatalf("session did not end; saw %d tool calls", len(calls))
		}
	}
}

func TestAScriptedRecordLeadArrivesIsAnsweredAndTheSessionEnds(t *testing.T) {
	server, url := scenarioServer(t, highLeadScript)

	calls, isClosed, isFatal := play(t, url, "any instructions", true)

	if len(calls) != 1 {
		t.Fatalf("saw %d tool calls, want exactly 1", len(calls))
	}
	if calls[0].name != "record_lead" {
		t.Errorf("tool name %q, want record_lead", calls[0].name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].args), &args); err != nil {
		t.Fatalf("arguments %q are not a JSON object: %v", calls[0].args, err)
	}
	if len(args) != 2 || args["intent"] != "HIGH" || args["notes"] != "wants a callback" {
		t.Errorf("arguments %v, want exactly intent=HIGH and the notes", args)
	}
	if calls[0].id == "" {
		t.Error("the tool call carries no call id to answer")
	}
	if !isClosed || isFatal {
		t.Errorf("closed=%v fatal=%v, want a normal close", isClosed, isFatal)
	}

	waitFor(t, func() bool { return server.Stats().ScriptsDone == 1 }, "the script to finish")
	stats := server.Stats()
	if stats.ToolCalls != 1 || stats.ToolResults != 1 || stats.ScriptsDone != 1 ||
		stats.ScriptsFailed != 0 || stats.Unmatched != 0 {
		t.Errorf("stats %+v, want one call, one result, one finished script", stats)
	}
	if stats.Turns != 1 {
		t.Errorf("%d spoken turns, want 1", stats.Turns)
	}
}

func TestEachSessionPlaysTheScriptItsInstructionsSelect(t *testing.T) {
	server, url := scenarioServer(t, `{"scripts":[
	  {"name":"sales","instructionsContain":"SALES-PERSONA","steps":[
	    {"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH"}},
	    {"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}]},
	  {"name":"default","steps":[
	    {"op":"CALL_TOOL","toolName":"end_call"},
	    {"op":"AWAIT_TOOL_RESULT"},{"op":"END_SESSION"}]}]}`)

	sales, _, _ := play(t, url, "You are SALES-PERSONA, be brief.", true)
	other, _, _ := play(t, url, "You are someone else.", true)

	if len(sales) != 1 || sales[0].name != "record_lead" {
		t.Errorf("sales session saw %+v, want exactly one record_lead", sales)
	}
	if len(other) != 1 || other[0].name != "end_call" || other[0].args != "{}" {
		t.Errorf("default session saw %+v, want exactly one end_call with {} arguments", other)
	}
	waitFor(t, func() bool { return server.Stats().ScriptsDone == 2 }, "both scripts to finish")
	if stats := server.Stats(); stats.ToolResults != 2 || stats.Unmatched != 0 {
		t.Errorf("stats %+v, want two results and no unmatched session", stats)
	}
}

func TestASessionNoScriptClaimsStaysSilentAndIsCounted(t *testing.T) {
	server, url := scenarioServer(t, `{"scripts":[
	  {"name":"sales","instructionsContain":"SALES-PERSONA","steps":[{"op":"END_SESSION"}]}]}`)

	session, events := dialWith(t, url, "some other flow")
	waitFor(t, func() bool { return server.Stats().Unmatched == 1 }, "the session to be counted unmatched")

	quiet := time.After(200 * time.Millisecond)
	for isQuiet := false; !isQuiet; {
		select {
		case event := <-events:
			if event.Type != provider.EventTypeSessionReady {
				t.Errorf("an unmatched session produced %s", event.Type)
			}
		case <-quiet:
			isQuiet = true
		}
	}
	session.Close(t.Context())
	if stats := server.Stats(); stats.ToolCalls != 0 || stats.ScriptsDone != 0 || stats.Unmatched != 1 {
		t.Errorf("stats %+v, want only the unmatched session counted", stats)
	}
}

func TestAToolResultThatNeverComesFailsTheScriptAndTheSession(t *testing.T) {
	server, url := scenarioServer(t, `{"scripts":[{"name":"stuck","steps":[
	  {"op":"CALL_TOOL","toolName":"record_lead","args":{"intent":"HIGH"}},
	  {"op":"AWAIT_TOOL_RESULT","timeoutSec":0.2},
	  {"op":"END_SESSION"}]}]}`)

	calls, isClosed, isFatal := play(t, url, "x", false)

	if len(calls) != 1 {
		t.Fatalf("saw %d tool calls, want exactly 1", len(calls))
	}
	if !isClosed || !isFatal {
		t.Errorf("closed=%v fatal=%v, want a fatal close the application would rescue the call from", isClosed, isFatal)
	}
	waitFor(t, func() bool { return server.Stats().ScriptsFailed == 1 }, "the script to fail")
	if stats := server.Stats(); stats.ScriptsDone != 0 || stats.ToolResults != 0 {
		t.Errorf("stats %+v, want no finished script and no result", stats)
	}
}

func TestScenarioDocumentsAreValidated(t *testing.T) {
	valid, err := mockprovider.ParseScenario([]byte(highLeadScript))
	if err != nil {
		t.Fatalf("the example does not parse: %v", err)
	}
	if len(valid.Scripts) != 1 || len(valid.Scripts[0].Steps) != 4 {
		t.Fatalf("parsed %d scripts, want 1 with 4 steps", len(valid.Scripts))
	}

	invalid := map[string]string{
		"no scripts":         `{"scripts":[]}`,
		"unknown field":      `{"scripts":[{"steps":[{"op":"SPEAK","durationSecs":1}]}]}`,
		"unknown op":         `{"scripts":[{"steps":[{"op":"HANG_UP"}]}]}`,
		"empty steps":        `{"scripts":[{"steps":[]}]}`,
		"speak without time": `{"scripts":[{"steps":[{"op":"SPEAK"}]}]}`,
		"tool without name":  `{"scripts":[{"steps":[{"op":"CALL_TOOL"}]}]}`,
		"args not object":    `{"scripts":[{"steps":[{"op":"CALL_TOOL","toolName":"t","args":[1]}]}]}`,
		"await without call": `{"scripts":[{"steps":[{"op":"AWAIT_TOOL_RESULT"}]}]}`,
		"end not last":       `{"scripts":[{"steps":[{"op":"END_SESSION"},{"op":"SPEAK","durationSec":1}]}]}`,
		"two defaults":       `{"scripts":[{"steps":[{"op":"END_SESSION"}]},{"steps":[{"op":"END_SESSION"}]}]}`,
	}
	if len(invalid) != 10 {
		t.Fatalf("%d invalid cases, the table is meant to hold 10", len(invalid))
	}
	for name, document := range invalid {
		if _, err := mockprovider.ParseScenario([]byte(document)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func dialWith(t *testing.T, url, instructions string) (*provider.Realtime, <-chan provider.Event) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "not-a-key")
	profile, err := provider.ProfileFor(provider.NameOpenAI, provider.Override{
		Endpoint: "ws" + strings.TrimPrefix(url, "http") + "/v1/realtime",
	})
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	session, err := provider.New(profile, quietLog())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if err := session.Start(t.Context(), provider.SessionConfig{
		Instructions: instructions,
		Language:     "en",
	}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	t.Cleanup(func() { session.Close(t.Context()) })
	return session, session.Events()
}
