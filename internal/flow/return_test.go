// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"testing"
)

// returnFlow hands the caller over the way the seed novanet_support does: a
// successful transfer_to_agent moves the call to a terminal phase that says
// the caller is being put through.
const returnFlow = `{
	"id": "return",
	"specVersion": "v2",
	"initialNode": "welcome",
	"global": {
		"persona": "You answer the phone.",
		"closingTarget": "farewell",
		"maxTurnsWithoutTool": 3,
		"alwaysAllowedTools": ["transfer_to_agent", "hangup"],
		"transitions": [
			{"on": "TOOL_RESULT", "tool": "transfer_to_agent",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "finish_transfer"},
			{"on": "TOOL_RESULT", "tool": "hangup",
			 "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "farewell"}
		]
	},
	"nodes": {
		"welcome": {
			"announce": "Thanks for calling.",
			"instruction": "Help the caller.",
			"tools": [],
			"transitions": [{"on": "NO_INPUT",
				"condition": {"slot": "noInput.count", "op": "GTE", "value": 2}, "target": "farewell"}]
		},
		"finish_transfer": {"announce": "Putting you through.",
			"instruction": "The caller is connected right after this line.", "tools": [], "isTerminal": true},
		"farewell": {"announce": "Goodbye.", "instruction": "Say goodbye.", "tools": [], "isTerminal": true}
	}
}`

// handedOver is an engine whose call was moved to finish_transfer by a
// transfer made in welcome, after a silence and a tool-less reply there.
func handedOver(t *testing.T) *Engine {
	t.Helper()
	e := testEngineFor(t, returnFlow, "en")
	silences(t, e, 1)
	reply(e)
	e.OnBotTurnDone(true, false) // the turn that called transfer_to_agent
	if moved := e.OnToolResult(ToolTransferToAgent, nil, map[string]any{"ok": "1"}); moved != "finish_transfer" {
		t.Fatalf("the transfer moved to %q, want finish_transfer", moved)
	}
	return e
}

// A transfer the switch never ran: the call goes back to the phase it was made
// from, without entering it again (no visit counted), with the runs that
// measure the conversation since its last progress started over, and with
// every rule and guard of that phase live again.
func TestReturnToResumesThePhaseAMoveLeft(t *testing.T) {
	t.Parallel()
	e := handedOver(t)

	if !e.ReturnTo("welcome") {
		t.Fatal("ReturnTo refused the phase the transfer was made from")
	}
	if e.NodeID() != "welcome" || e.IsTerminal() {
		t.Fatalf("phase = %s (terminal %v), want welcome", e.NodeID(), e.IsTerminal())
	}
	if got, _ := e.Slot("node"); got != "welcome" {
		t.Errorf("node slot = %v, want welcome", got)
	}
	if got, _ := e.Slot("welcome.visits"); got != 1 {
		t.Errorf("welcome.visits = %v, want 1: a return is not a visit", got)
	}
	if got, _ := e.Slot("finish_transfer.visits"); got != 1 {
		t.Errorf("finish_transfer.visits = %v, want 1", got)
	}
	if e.Instruction() != "Help the caller." {
		t.Errorf("instruction = %q, want welcome's", e.Instruction())
	}
	if got, _ := e.Slot("noInput.count"); got != 0 {
		t.Errorf("noInput.count = %v, want 0", got)
	}
	if got, _ := e.Slot("turnsWithoutTool"); got != 0 {
		t.Errorf("turnsWithoutTool = %v, want 0", got)
	}
	// The transfer's turn left a tool reply owed; the next reply answers the
	// caller and counts.
	if reply(e); e.turnsWithoutTool != 1 {
		t.Errorf("turnsWithoutTool = %d after a reply to the caller, want 1", e.turnsWithoutTool)
	}
	// welcome's own silence rule fires again: two silences from here, not one.
	silences(t, e, 1)
	if moved := e.OnNoInput(); moved != "farewell" {
		t.Errorf("the second silence moved to %q, want farewell", moved)
	}
}

func TestAReturnedPhaseHasItsRulesAndGuardsBack(t *testing.T) {
	t.Parallel()
	e := handedOver(t)
	e.ReturnTo("welcome")
	if moved := e.OnToolResult(ToolHangup, nil, map[string]any{"ok": "1"}); moved != "farewell" {
		t.Errorf("hangup moved to %q, want farewell", moved)
	}

	e = handedOver(t)
	if moved := e.CloseAtSessionLimit(); moved != "" {
		t.Fatalf("the time limit moved a terminal phase, to %q", moved)
	}
	e.ReturnTo("welcome")
	if moved := e.CloseAtSessionLimit(); moved != "farewell" {
		t.Errorf("the time limit moved to %q, want the closing target", moved)
	}

	e = handedOver(t)
	e.ReturnTo("welcome")
	for range 4 {
		reply(e)
	}
	if moved := e.CloseAtTurnsWithoutToolWall(); moved != "farewell" {
		t.Errorf("the turns-without-a-tool wall moved to %q, want the closing target", moved)
	}
}

// A return to a terminal phase or one the flow does not have would leave the
// call where it is; it is refused and nothing changes.
func TestReturnToRefusesATerminalOrUnknownPhase(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"farewell", "nowhere", ""} {
		e := handedOver(t)
		if e.ReturnTo(phase) {
			t.Errorf("ReturnTo(%q) succeeded", phase)
		}
		if e.NodeID() != "finish_transfer" {
			t.Errorf("ReturnTo(%q) moved the call to %s", phase, e.NodeID())
		}
	}
}

// The runtime tells the transfer which phase it was asked for in, before the
// result moves the call.
func TestATransferRequestCarriesThePhaseItWasMadeIn(t *testing.T) {
	t.Parallel()
	e := testEngineFor(t, returnFlow, "en")
	actions := &fakeActions{}
	r := NewRuntime(e, actions, NewBackend(""), []string{"support"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, moved := r.Dispatch(t.Context(), ToolTransferToAgent, `{"queue":"support"}`); moved != "finish_transfer" {
		t.Fatalf("moved to %q, want finish_transfer", moved)
	}
	if len(actions.transfers) != 1 || actions.transfers[0].FromPhase != "welcome" {
		t.Errorf("transfers = %+v, want one from welcome", actions.transfers)
	}
}
