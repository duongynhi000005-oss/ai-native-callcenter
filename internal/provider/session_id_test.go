// SPDX-License-Identifier: Apache-2.0

package provider

import (
	"strings"
	"testing"
)

// The provider's session id is the only key that matches a call to the
// provider's own session logs when several calls overlap, so it must reach the
// call's log exactly once, at Info.
func TestTheProviderSessionIDIsLoggedOncePerCall(t *testing.T) {
	cases := []struct {
		name    string
		session map[string]any
		wantID  string
	}{
		{"openai shape", map[string]any{"id": "sess_CdE9fGh2Ij4kLm6nOp8qRs", "object": "realtime.session"}, "sess_CdE9fGh2Ij4kLm6nOp8qRs"},
		{"id of another vendor's shape", map[string]any{"id": "mlx-7f3a-通话"}, "mlx-7f3a-通话"},
		{"empty id", map[string]any{"id": ""}, ""},
		{"no id", map[string]any{"model": "gpt-realtime"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The created event is sent ahead of the update's reply, as a
			// real provider does, and both ride one connection in order, so
			// SESSION_READY means the created event has been handled.
			f := newFakeProvider(t, func(f *fakeProvider, message map[string]any) {
				if message["type"] == "session.update" {
					f.send(map[string]any{"type": "session.created", "event_id": "event_1", "session": tc.session})
					f.send(map[string]any{"type": "session.updated"})
				}
			})
			session, logs := loggedSession(t, f, OpenAIProfile())
			if err := session.Start(t.Context(), basicConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}
			awaitEvent(t, session, EventTypeSessionReady)

			got := logs.String()
			const line = "msg=\"provider session created\""
			if tc.wantID == "" {
				if strings.Contains(got, line) {
					t.Fatalf("an event without an id was logged at info:\n%s", got)
				}
				if !strings.Contains(got, "provider session created without an id") {
					t.Fatalf("the missing id left no trace:\n%s", got)
				}
				return
			}
			if n := strings.Count(got, line); n != 1 {
				t.Fatalf("%d session lines, want 1:\n%s", n, got)
			}
			for _, want := range []string{"level=INFO", "providerSessionId=" + quoted(tc.wantID), "provider=openai"} {
				if !strings.Contains(got, want) {
					t.Errorf("the line lacks %s:\n%s", want, got)
				}
			}
		})
	}
}

// quoted renders a value the way the text handler does: bare unless it needs
// quotes, which a non-ASCII id does not.
func quoted(s string) string {
	if strings.ContainsAny(s, " \"=") {
		return "\"" + s + "\""
	}
	return s
}
