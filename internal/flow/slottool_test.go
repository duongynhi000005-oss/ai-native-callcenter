// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// factsFlow is a small survey: two slot tools, a lookup that reads them, and a
// phase that renders them back.
const factsFlow = `{
  "id": "facts", "specVersion": "v2", "initialNode": "ask",
  "global": {"persona": "p", "apiBaseEnv": "X"},
  "nodes": {
    "ask": {
      "instruction": "Known: budget={slots.budget}; plan={slots.plan}; last={slots.search.args.q}; vip={slots.userData.tier}",
      "tools": ["note_budget", "note_plan", "search"],
      "transitions": [
        {"on": "TOOL_RESULT", "tool": "note_plan", "condition": {"slot": "result.ok", "op": "EQ", "value": "1"}, "target": "done"}
      ]
    },
    "done": {"instruction": "Plan {slots.plan}.", "tools": []}
  },
  "tools": {
    "note_budget": {"kind": "slot", "slot": "budget", "description": "record budget"},
    "note_plan": {"kind": "slot", "slot": "plan", "enum": ["basic", "pro"], "description": "record plan"},
    "search": {
      "description": "d",
      "parameters": {"type": "object", "properties": {"q": {"type": "string"}}},
      "http": {"path": "/s", "body": {"budget": "{slots.budget}", "q": "{args.q}"}}
    }
  }
}`

func factsRuntime(t *testing.T, backendURL string, userData map[string]any) *Runtime {
	t.Helper()
	spec, err := Load([]byte(factsFlow))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	engine := NewEngine(spec, "en", SeedSlots("1380", "e", "950", userData), log)
	return NewRuntime(engine, &fakeActions{}, NewBackend(backendURL), nil, log)
}

func TestASlotToolRecordsTheFactAndTheInstructionRendersIt(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", nil)
	r.Instructions()

	out, moved := r.Dispatch(t.Context(), "note_budget", `{"value":"  about 5000 yuan\n"}`)
	if got := decodeOutput(t, out)["ok"]; got != "1" || moved != "" {
		t.Fatalf("output %s moved %q", out, moved)
	}
	if v, _ := r.Engine().Slot("budget"); v != "about 5000 yuan" {
		t.Fatalf("budget = %q, want the trimmed value", v)
	}
	if !r.IsInstructionStale() || !strings.Contains(r.Engine().Instruction(), "budget=about 5000 yuan;") {
		t.Fatalf("instruction does not carry the fact: %q", r.Engine().Instruction())
	}
	if n, _ := r.Engine().Slot("note_budget.calls"); n != 1 {
		t.Fatalf("calls = %v", n)
	}

	// The latest call overwrites.
	r.Dispatch(t.Context(), "note_budget", `{"value":"8000"}`)
	if v, _ := r.Engine().Slot("budget"); v != "8000" {
		t.Fatalf("budget = %q, want the later value", v)
	}
	if n, _ := r.Engine().Slot("note_budget.calls"); n != 2 {
		t.Fatalf("calls = %v", n)
	}
}

func TestASlotToolRefusesWhatIsNotAFact(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ tool, args string }{
		"empty":      {"note_budget", `{"value":""}`},
		"whitespace": {"note_budget", `{"value":" \n\t "}`},
		"missing":    {"note_budget", `{}`},
		"too long":   {"note_budget", `{"value":"` + strings.Repeat("x", MaxSlotValueLen+1) + `"}`},
		"not enum":   {"note_plan", `{"value":"gold"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := factsRuntime(t, "", nil)
			out, moved := r.Dispatch(t.Context(), tc.tool, tc.args)
			decoded := decodeOutput(t, out)
			if decoded["ok"] != "0" || decoded["error"] == "" {
				t.Fatalf("want a refusal with a reason, got %s", out)
			}
			if moved != "" {
				t.Fatalf("a refused fact moved the call to %q", moved)
			}
			for _, slot := range []string{"budget", "plan"} {
				if v, ok := r.Engine().Slot(slot); ok {
					t.Fatalf("slot %s = %v after a refusal", slot, v)
				}
			}
		})
	}
}

func TestASlotToolAtTheLimitIsAccepted(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", nil)
	value := strings.Repeat("好", MaxSlotValueLen)
	out, _ := r.Dispatch(t.Context(), "note_budget", `{"value":"`+value+`"}`)
	if decodeOutput(t, out)["ok"] != "1" {
		t.Fatalf("a value of exactly %d characters was refused: %s", MaxSlotValueLen, out)
	}
}

func TestASlotCallFiresTheToolResultTransition(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", nil)
	if _, moved := r.Dispatch(t.Context(), "note_plan", `{"value":"gold"}`); moved != "" {
		t.Fatal("a refused value must not move the call")
	}
	if _, moved := r.Dispatch(t.Context(), "note_plan", `{"value":"pro"}`); moved != "done" {
		t.Fatalf("moved %q, want done", moved)
	}
	if got := r.Engine().Instruction(); got != "Plan pro." {
		t.Fatalf("instruction %q", got)
	}
}

func TestAFactSurvivesPhaseChangesAndFeedsAnHTTPTool(t *testing.T) {
	t.Parallel()
	var body map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewDecoder(req.Body).Decode(&body)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer backend.Close()
	r := factsRuntime(t, backend.URL, nil)

	r.Dispatch(t.Context(), "note_budget", `{"value":"5000"}`)
	r.Engine().OnNoInput() // other traffic in between
	r.Dispatch(t.Context(), "search", `{"q":"phones"}`)
	if body["budget"] != "5000" || body["q"] != "phones" {
		t.Fatalf("backend saw %v", body)
	}
	if v, _ := r.Engine().Slot("search.args.q"); v != "phones" {
		t.Fatalf("search.args.q = %v", v)
	}
	// An argument left out of the next call is not read from the one before.
	r.Dispatch(t.Context(), "search", `{}`)
	if v, ok := r.Engine().Slot("search.args.q"); ok {
		t.Fatalf("search.args.q = %v after a call without it", v)
	}
}

func TestToolArgumentsAreStoredAsScalarsAndCapped(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", nil)
	long := strings.Repeat("a", MaxSlotValueLen+50)
	r.Engine().OnToolResult("search", map[string]any{
		"q": long, "n": float64(3), "nested": map[string]any{"a": 1}, "list": []any{1},
	}, map[string]any{"ok": "1"})
	if v, _ := r.Engine().Slot("search.args.q"); len([]rune(v.(string))) != MaxSlotValueLen {
		t.Fatalf("argument not cut to %d", MaxSlotValueLen)
	}
	if v, _ := r.Engine().Slot("search.args.n"); v != float64(3) {
		t.Fatalf("n = %v", v)
	}
	for _, k := range []string{"search.args.nested", "search.args.list"} {
		if _, ok := r.Engine().Slot(k); ok {
			t.Fatalf("%s was stored", k)
		}
	}
}

func TestUserDataIsSeededAsReadOnlySlots(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", map[string]any{
		"tier": "gold", "order": map[string]any{"id": "A1"}, "tags": []any{"x"}, "n": 7,
	})
	for slot, want := range map[string]any{
		"caller": "1380", "did": "950", "userData.tier": "gold", "userData.order.id": "A1", "userData.n": 7,
	} {
		if v, _ := r.Engine().Slot(slot); v != want {
			t.Errorf("%s = %v, want %v", slot, v, want)
		}
	}
	if _, ok := r.Engine().Slot("userData.tags"); ok {
		t.Error("an array was seeded")
	}
	if !strings.Contains(r.Engine().Instruction(), "vip=gold") {
		t.Errorf("instruction does not render userData: %q", r.Engine().Instruction())
	}
}

func TestASlotToolIsOfferedWithAGeneratedSchema(t *testing.T) {
	t.Parallel()
	r := factsRuntime(t, "", nil)
	for _, spec := range r.Tools() {
		if spec.Name != "note_plan" {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(spec.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		if got := schema.Properties["value"].Enum; len(got) != 2 || got[0] != "basic" {
			t.Fatalf("enum = %v", got)
		}
		if len(schema.Required) != 1 || schema.Required[0] != "value" {
			t.Fatalf("required = %v", schema.Required)
		}
		return
	}
	t.Fatal("note_plan was not offered")
}

func TestLoadRejectsBadSlotDeclarations(t *testing.T) {
	t.Parallel()
	base := func(mutate func(m map[string]any)) []byte {
		var m map[string]any
		_ = json.Unmarshal([]byte(factsFlow), &m)
		mutate(m)
		out, _ := json.Marshal(m)
		return out
	}
	tools := func(m map[string]any) map[string]any { return m["tools"].(map[string]any) }
	node := func(m map[string]any) map[string]any { return m["nodes"].(map[string]any)["ask"].(map[string]any) }

	cases := map[string]struct {
		mutate func(m map[string]any)
		want   string
	}{
		"slot tool with http": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["http"] = map[string]any{"path": "/x"}
		}, "must not declare http"},
		"slot tool with parameters": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["parameters"] = map[string]any{"type": "object"}
		}, "must not declare parameters"},
		"unknown kind": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["kind"] = "script"
		}, `unknown kind "script"`},
		"no slot": {func(m map[string]any) {
			delete(tools(m)["note_budget"].(map[string]any), "slot")
		}, "names no slot"},
		"dotted slot": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["slot"] = "a.b"
		}, "not a single name"},
		"reserved slot": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["slot"] = "caller"
		}, "the engine owns"},
		"duplicate slot": {func(m map[string]any) {
			tools(m)["note_budget"].(map[string]any)["slot"] = "plan"
		}, "both write slot"},
		"duplicate enum": {func(m map[string]any) {
			tools(m)["note_plan"].(map[string]any)["enum"] = []any{"a", "a"}
		}, "twice"},
		"slot on an http tool": {func(m map[string]any) {
			tools(m)["search"].(map[string]any)["slot"] = "x"
		}, "only a slot tool has"},
		"undeclared slot in an instruction": {func(m map[string]any) {
			node(m)["instruction"] = "x {slots.bugdet}"
		}, "{slots.bugdet}"},
		"undeclared slot in an announce": {func(m map[string]any) {
			node(m)["announce"] = "x {slots.nope}"
		}, "announce references {slots.nope}"},
		"undeclared slot in a condition": {func(m map[string]any) {
			node(m)["transitions"] = []any{map[string]any{"target": "done",
				"condition": map[string]any{"any": []any{map[string]any{"slot": "plam", "op": "IS_NOT_EMPTY"}}}}}
		}, `tests slot "plam"`},
		"undeclared slot in an http body": {func(m map[string]any) {
			tools(m)["search"].(map[string]any)["http"].(map[string]any)["body"] = map[string]any{"a": []any{"{slots.zzz}"}}
		}, "{slots.zzz}"},
		"argument the tool does not declare": {func(m map[string]any) {
			node(m)["instruction"] = "x {slots.search.args.nope}"
		}, "{slots.search.args.nope}"},
		"field the tool does not map": {func(m map[string]any) {
			node(m)["instruction"] = "x {slots.search.nope}"
		}, "{slots.search.nope}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Load(base(tc.mutate))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a problem containing %q, got %v", tc.want, err)
			}
		})
	}

	if _, err := Load([]byte(factsFlow)); err != nil {
		t.Fatalf("the fixture must load: %v", err)
	}
}
