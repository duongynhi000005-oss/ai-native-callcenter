// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

const secretFlow = `{
  "id": "kb", "specVersion": "v2", "initialNode": "main",
  "global": {"persona": "You answer questions."},
  "nodes": {"main": {"instruction": "Help the caller.", "tools": ["kb_search"]}},
  "tools": {"kb_search": {
    "description": "Search the knowledge base.",
    "parameters": {"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]},
    "http": {
      "baseUrl": "http://kb.internal:8000",
      "path": "/search",
      "method": "GET",
      "query": {"q": "{args.q}"},
      "headers": {"Authorization": "Bearer {secrets.KB_KEY}"},
      "timeoutMs": 8000
    }
  }}
}`

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A draft that references a secret the deployment does not have is stored
// with a warning, and publishing it is refused naming the variable. Once the
// variable is set it publishes, and what is stored is the reference, never the
// value.
//
// Not parallel: it reads the default logger, which the store writes to.
func TestAnUnsetToolSecretWarnsOnADraftAndBlocksThePublish(t *testing.T) {
	ctx := context.Background()
	dsn := scratchDB(t)
	db := openScratch(t, dsn)
	gooseFor(t)
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := Open(ctx, dsn, 4)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(st.Close)

	const value = "kb-live-7c1d2e3f4a5b"
	env := map[string]string{}
	var mu sync.Mutex
	lookup := func(name string) (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		v, ok := env[name]
		return v, ok
	}
	st.FlowPublishRules = []flow.Rule{flow.RequireToolSecrets(lookup)}
	st.FlowDraftChecks = []flow.Rule{flow.RequireToolSecrets(lookup)}
	flows := st.Flows()

	logs := &syncBuffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	id, err := flows.Create(ctx, "kb", "Knowledge", []byte(secretFlow))
	if err != nil {
		t.Fatalf("a draft with an unset secret was refused: %v", err)
	}
	if text := logs.String(); !strings.Contains(text, "will block its publish") || !strings.Contains(text, "AICC_TOOL_SECRET_KB_KEY") {
		t.Errorf("no draft warning naming the variable:\n%s", text)
	}

	err = flows.Publish(ctx, id, "")
	var invalid *flow.ValidationError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "AICC_TOOL_SECRET_KB_KEY") {
		t.Fatalf("publish error = %v, want a ValidationError naming the variable", err)
	}

	mu.Lock()
	env["AICC_TOOL_SECRET_KB_KEY"] = value
	mu.Unlock()
	if err := flows.Publish(ctx, id, ""); err != nil {
		t.Fatalf("publish with the secret set: %v", err)
	}
	detail, err := flows.Detail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detail.DraftSpec), "{secrets.KB_KEY}") || strings.Contains(string(detail.DraftSpec), value) {
		t.Errorf("stored draft = %s, want the reference and not the value", detail.DraftSpec)
	}
	published, err := flows.PublishedSpec(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got := published.Tools["kb_search"].HTTP.Headers["Authorization"]; got != "Bearer {secrets.KB_KEY}" {
		t.Errorf("published header = %q, want the reference", got)
	}
	if strings.Contains(logs.String(), value) {
		t.Errorf("the log carries the secret value:\n%s", logs.String())
	}
}
