// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// denyFlow lets every tool through twice over: a "*" allowlist and an
// always-allowed list that names the two built-ins a campaign call loses.
const denyFlow = `{
	"id": "deny-test",
	"specVersion": "v2",
	"initialNode": "talk",
	"global": {
		"persona": "You call people.",
		"alwaysAllowedTools": ["take_message", "transfer_to_agent"]
	},
	"nodes": {
		"talk": {"instruction": "Talk.", "tools": ["*"]}
	}
}`

func denyRuntime(t *testing.T, actions *fakeActions) *Runtime {
	t.Helper()
	spec, err := Load([]byte(denyFlow))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRuntime(NewEngine(spec, "en", nil, log), actions, NewBackend(""),
		[]string{"support-en"}, log)
}

func offered(r *Runtime) []string {
	var names []string
	for _, tool := range r.Tools() {
		names = append(names, tool.Name)
	}
	return names
}

func TestADeniedToolIsNotOfferedWhateverTheAllowlistsSay(t *testing.T) {
	t.Parallel()
	r := denyRuntime(t, &fakeActions{})
	before := offered(r)
	for _, want := range []string{"transfer_to_agent", "take_message", "hangup"} {
		if !contains(before, want) {
			t.Fatalf("setup: %q missing from %v", want, before)
		}
	}

	r.Deny("transfer_to_agent", "take_message")

	after := offered(r)
	for _, gone := range []string{"transfer_to_agent", "take_message"} {
		if contains(after, gone) {
			t.Errorf("denied tool %q still offered: %v", gone, after)
		}
	}
	if !contains(after, "hangup") || len(after) != len(before)-2 {
		t.Errorf("offered %v, want %v minus the two denied tools", after, before)
	}
}

func TestADeniedToolIsRefusedAndNeverReachesTheActions(t *testing.T) {
	t.Parallel()
	actions := &fakeActions{}
	r := denyRuntime(t, actions)
	r.Deny("transfer_to_agent", "take_message")

	for name, args := range map[string]string{
		"transfer_to_agent": `{"queue":"support-en","reason":"X","summary":"s"}`,
		"take_message":      `{"message":"call me","callbackNumber":"1"}`,
	} {
		output, moved := r.Dispatch(t.Context(), name, args)
		decoded := decodeOutput(t, output)
		if decoded["ok"] == float64(1) || !strings.Contains(output, "not available") || moved != "" {
			t.Errorf("%s: output %s moved %q, want a refusal", name, output, moved)
		}
	}
	if len(actions.transfers) != 0 || len(actions.messages) != 0 {
		t.Errorf("actions reached: transfers=%v messages=%v", actions.transfers, actions.messages)
	}

	// An ordinary runtime is unchanged.
	plain := denyRuntime(t, actions)
	plain.Dispatch(t.Context(), "take_message", `{"message":"call me","callbackNumber":"1"}`)
	if len(actions.messages) != 1 {
		t.Errorf("an undenied runtime lost take_message: %v", actions.messages)
	}
}
