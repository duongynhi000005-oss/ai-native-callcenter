// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"strings"
	"testing"
)

// withToolWaitLine puts a global.toolWaitLine into testFlow.
func withToolWaitLine(value string) string {
	return strings.Replace(testFlow, `"maxTurns": 6,`,
		`"maxTurns": 6, "toolWaitLine": `+value+`,`, 1)
}

// global.toolWaitLine is checked like any line the caller hears: its slot
// references must name something the flow writes. It must also be a statement,
// in either language.
func TestTheToolWaitLineIsValidatedAtLoad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		value   string
		problem string
	}{
		{"one wording", `"Bear with me a second."`, ""},
		{"per language", `{"en": "Bear with me a second.", "zh": "稍等片刻，马上就好。"}`, ""},
		{"one language only", `{"zh": "稍等片刻。"}`, ""},
		{"a slot the flow writes", `"Hold on, {slots.caller}, I am looking."`, ""},
		{"empty is the default", `""`, ""},
		{"a slot nothing writes", `"Hold on, {slots.nobody}."`,
			"global.toolWaitLine references {slots.nobody}"},
		{"an English question", `{"en": "Can you hold on a moment?", "zh": "请稍等。"}`,
			"global.toolWaitLine \"Can you hold on a moment?\" is a question"},
		{"a Chinese question", `{"en": "One moment.", "zh": "您能稍等一下吗？"}`,
			"global.toolWaitLine \"您能稍等一下吗？\" is a question"},
		{"a question mid-line", `"Still there? One moment."`, "is a question"},
		{"not text at all", `42`, "cannot parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Load([]byte(withToolWaitLine(tc.value)))
			if tc.problem == "" {
				if err != nil {
					t.Fatalf("load: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.problem) {
				t.Fatalf("load error = %v, want it to contain %q", err, tc.problem)
			}
		})
	}
}

// The wait line is the flow's wording in the call's language, with slots
// rendered, or that language's default where the flow has none; never the
// other language's words.
func TestTheToolWaitLineIsTheFlowsOrTheDefault(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		flow  string
		lang  string
		wants string
	}{
		{"default en", testFlow, LangEN, "One moment, let me check that."},
		{"default zh", testFlow, LangZH, "请稍等，我查一下。"},
		{"flow's en", withToolWaitLine(`{"en": "Bear with me, {slots.caller}.", "zh": "稍等片刻。"}`),
			LangEN, "Bear with me, 13800138000."},
		{"flow's zh", withToolWaitLine(`{"en": "Bear with me.", "zh": "稍等片刻。"}`),
			LangZH, "稍等片刻。"},
		{"zh only, an en call", withToolWaitLine(`{"zh": "稍等片刻。"}`),
			LangEN, "One moment, let me check that."},
		{"one wording, a zh call", withToolWaitLine(`"One second."`), LangZH, "One second."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := testEngineFor(t, tc.flow, tc.lang).ToolWaitLine(); got != tc.wants {
				t.Errorf("ToolWaitLine() = %q, want %q", got, tc.wants)
			}
		})
	}
}

// Only a flow-declared HTTP tool the phase allows waits on a backend; every
// other tool answers at once and has no wait line.
func TestOnlyAnAllowedHTTPToolWaitsOnABackend(t *testing.T) {
	t.Parallel()
	spec, err := Load([]byte(testFlow))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	runtime := NewRuntime(NewEngine(spec, LangEN, nil, nil), nil, nil, nil, nil)
	allowed := runtime.Engine().AllowedTools()
	for _, name := range allowed {
		tool, isDeclared := spec.Tools[name]
		want := isDeclared && tool.Kind != ToolKindSlot
		if got := runtime.IsHTTPTool(name); got != want {
			t.Errorf("IsHTTPTool(%q) = %v, want %v", name, got, want)
		}
	}
	for _, name := range append(BuiltinToolNames, "no_such_tool") {
		if runtime.IsHTTPTool(name) {
			t.Errorf("IsHTTPTool(%q) = true, want false", name)
		}
	}
}
