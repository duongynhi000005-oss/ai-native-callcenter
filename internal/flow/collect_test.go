// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
)

const collectFlow = `{
  "id": "lookups", "specVersion": "v2", "initialNode": "ask",
  "global": {"persona": "p"},
  "nodes": {
    "ask": {
      "instruction": "Found: [{slots.status.found}] ({slots.status.found.count})",
      "tools": ["status"],
      "transitions": [
        {"on": "TOOL_RESULT", "tool": "status", "condition": {"slot": "status.found.count", "op": "GTE", "value": 2}, "target": "many"}
      ]
    },
    "many": {"instruction": "Many: {slots.status.found}", "tools": ["status"]}
  },
  "tools": {
    "status": {
      "description": "d",
      "parameters": {"type": "object", "properties": {"rma": {"type": "string"}}},
      "http": {"path": "/s", "result": {"hit": "hit"}},
      "collect": {"slot": "status.found", "value": "{args.rma}",
        "when": {"slot": "result.hit", "op": "EQ", "value": "1"}}
    }
  }
}`

func collectEngine(t *testing.T, source string) *Engine {
	t.Helper()
	spec, err := Load([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return NewEngine(spec, "en", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func lookup(e *Engine, rma, ok, hit string) string {
	return e.OnToolResult("status", map[string]any{"rma": rma}, map[string]any{"ok": ok, "hit": hit})
}

func TestCollectKeepsTheValuesOfSuccessfulMatchingCalls(t *testing.T) {
	t.Parallel()
	e := collectEngine(t, collectFlow)
	if got := e.Instruction(); got != "Found: [] (0)" {
		t.Fatalf("before any call: %q", got)
	}

	lookup(e, "A1", "1", "1")
	lookup(e, "A9", "1", "0") // when is false
	lookup(e, "A8", "0", "1") // the call failed
	if got := e.Instruction(); got != "Found: [A1] (1)" {
		t.Fatalf("after one hit: %q", got)
	}

	// The count is already updated when the transition is evaluated.
	if moved := lookup(e, "  B2 ", "1", "1"); moved != "many" {
		t.Fatalf("moved to %q, want many", moved)
	}
	lookup(e, "A1", "1", "1") // duplicate, order kept
	if got := e.Instruction(); got != "Many: A1, B2" {
		t.Fatalf("instruction %q", got)
	}
	if v, _ := e.Slot("status.found.count"); v != 2 {
		t.Fatalf("count %v", v)
	}
}

func TestCollectSkipsAnEmptyValue(t *testing.T) {
	t.Parallel()
	e := collectEngine(t, collectFlow)
	lookup(e, "   ", "1", "1")
	e.OnToolResult("status", nil, map[string]any{"ok": "1", "hit": "1"})
	if v, _ := e.Slot("status.found"); v != "" {
		t.Fatalf("slot %q", v)
	}
}

func TestCollectIsBounded(t *testing.T) {
	t.Parallel()
	e := collectEngine(t, collectFlow)
	for i := 0; i < MaxCollectedValues+5; i++ {
		lookup(e, fmt.Sprintf("R%d", i), "1", "1")
	}
	if v, _ := e.Slot("status.found.count"); v != MaxCollectedValues {
		t.Fatalf("count %v, want %d", v, MaxCollectedValues)
	}
	list, _ := e.Slot("status.found")
	if !strings.HasPrefix(list.(string), "R0, R1, ") || strings.Contains(list.(string), "R20") {
		t.Fatalf("the first values must be kept: %q", list)
	}

	long := collectEngine(t, collectFlow)
	lookup(long, strings.Repeat("x", MaxSlotValueLen+40), "1", "1")
	if v, _ := long.Slot("status.found"); len([]rune(v.(string))) != MaxSlotValueLen {
		t.Fatalf("a long value must be cut to %d characters, got %d", MaxSlotValueLen, len([]rune(v.(string))))
	}
}

func TestCollectWithoutWhenCollectsEveryCall(t *testing.T) {
	t.Parallel()
	source := strings.Replace(collectFlow, `,
        "when": {"slot": "result.hit", "op": "EQ", "value": "1"}`, "", 1)
	e := collectEngine(t, source)
	lookup(e, "A", "1", "0")
	lookup(e, "B", "1", "0")
	if v, _ := e.Slot("status.found"); v != "A, B" {
		t.Fatalf("slot %q", v)
	}
}

func TestLoadRejectsBrokenCollects(t *testing.T) {
	t.Parallel()
	collect := func(body string) string {
		return strings.Replace(collectFlow,
			`"collect": {"slot": "status.found", "value": "{args.rma}",
        "when": {"slot": "result.hit", "op": "EQ", "value": "1"}}`, body, 1)
	}
	cases := []struct{ name, source, want string }{
		{"no slot", collect(`"collect": {"value": "{args.rma}"}`), "collect names no slot"},
		{"bad slot name", collect(`"collect": {"slot": "a b", "value": "{args.rma}"}`), "not a name of letters"},
		{"no value", collect(`"collect": {"slot": "found"}`), "collect has no value"},
		{"undeclared argument", collect(`"collect": {"slot": "found", "value": "{args.nope}"}`), "{args.nope}"},
		{"undeclared slot reference", collect(`"collect": {"slot": "found", "value": "{slots.nope}"}`), "{slots.nope}"},
		{"unknown operator", collect(`"collect": {"slot": "found", "value": "{args.rma}", "when": {"slot": "result.hit", "op": "NOPE"}}`), "operator"},
		{"when on an undeclared slot", collect(`"collect": {"slot": "found", "value": "{args.rma}", "when": {"slot": "result.zzz", "op": "EQ", "value": "1"}}`), `slot "result.zzz"`},
		{"engine slot", collect(`"collect": {"slot": "turns", "value": "{args.rma}"}`), "written by the engine"},
		{"result namespace", collect(`"collect": {"slot": "result.x", "value": "{args.rma}"}`), "namespace of result"},
		{"userData namespace", collect(`"collect": {"slot": "userData.x", "value": "{args.rma}"}`), "namespace of userData"},
		{"tool calls counter", collect(`"collect": {"slot": "status.calls", "value": "{args.rma}"}`), "written by the engine for tool status"},
		{"tool argument", collect(`"collect": {"slot": "status.args.rma", "value": "{args.rma}"}`), "written by the engine for tool status"},
		{"tool result field", collect(`"collect": {"slot": "status.hit", "value": "{args.rma}"}`), "result field of tool status"},
		{"phase visits", collect(`"collect": {"slot": "ask.visits", "value": "{args.rma}"}`), "visit counter"},
		{"count of a result field", collect(`"collect": {"slot": "status.hit", "value": "{args.rma}"}`), "status.hit"},
		{"slot tool's slot", strings.Replace(collect(`"collect": {"slot": "note", "value": "{args.rma}"}`),
			`"tools": {`, `"tools": {"remember": {"kind": "slot", "slot": "note", "description": "d"},`, 1), "slot of slot tool remember"},
		{"on a slot tool", strings.Replace(collectFlow, `"tools": {`,
			`"tools": {"remember": {"kind": "slot", "slot": "note", "description": "d", "collect": {"slot": "n2", "value": "x"}},`, 1), "must not declare collect"},
		{"two collects share a slot", strings.Replace(collectFlow, `"tools": {`,
			`"tools": {"other": {"description": "d", "http": {"path": "/o"}, "collect": {"slot": "status.found", "value": "x"}},`, 1), "also written by the collect"},
		{"count collides with another collect", strings.Replace(collectFlow, `"tools": {`,
			`"tools": {"other": {"description": "d", "http": {"path": "/o"}, "collect": {"slot": "status.found.count", "value": "x"}},`, 1), "also written by the collect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(tc.source))
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("want a validation error, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
