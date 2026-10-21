// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/egress"
)

// recorded is one request a test backend received, as it arrived on the wire.
type recorded struct {
	method        string
	requestURI    string
	rawQuery      string
	query         url.Values
	header        http.Header
	body          []byte
	contentLength int64
}

// testBackend is an httptest server that records requests and counts
// connections. Counting is what proves a refusal opened none.
type testBackend struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recorded
	conns    atomic.Int32
	probes   int32
	respond  func(w http.ResponseWriter, r *http.Request)
}

func newTestBackend(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *testBackend {
	t.Helper()
	b := &testBackend{respond: respond}
	b.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__probe" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.requests = append(b.requests, recorded{
			method: r.Method, requestURI: r.RequestURI, rawQuery: r.URL.RawQuery,
			query: r.URL.Query(), header: r.Header.Clone(), body: body, contentLength: r.ContentLength,
		})
		b.mu.Unlock()
		if b.respond != nil {
			b.respond(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"retCode": "000000", "data": map[string]any{"answer": "yes"}})
	}))
	// The server's accept loop marks a connection new before it accepts the
	// next one, so once a later request has been served every earlier
	// connection has been counted.
	b.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			b.conns.Add(1)
		}
	}
	b.Start()
	t.Cleanup(b.Close)
	return b
}

// connections returns how many connections the tool calls so far opened, made
// exact by one probe connection of the test's own.
func (b *testBackend) connections(t *testing.T) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(b.URL + "/__probe")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	response.Body.Close()
	b.probes++
	return int(b.conns.Load()) - int(b.probes)
}

func (b *testBackend) received() []recorded {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recorded(nil), b.requests...)
}

// toolFlow is a one-phase flow with one HTTP tool called "lookup".
func toolFlow(t *testing.T, parameters, httpBlock string) *Spec {
	t.Helper()
	spec, err := Load([]byte(toolFlowJSON(parameters, httpBlock)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return spec
}

func toolFlowJSON(parameters, httpBlock string) string {
	if parameters == "" {
		parameters = `{"type":"object","properties":{}}`
	}
	return `{
	  "id": "kb", "specVersion": "v2", "initialNode": "main",
	  "global": {"persona": "You answer questions."},
	  "nodes": {"main": {"instruction": "Help the caller.", "tools": ["*"]}},
	  "tools": {"lookup": {
	    "description": "Look something up.",
	    "parameters": ` + parameters + `,
	    "http": ` + httpBlock + `
	  }}
	}`
}

// logCapture is a logger whose output a test can read back.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// runtimeFor runs spec against backend with the given logger.
func runtimeFor(spec *Spec, backend *Backend, log *slog.Logger) *Runtime {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return NewRuntime(NewEngine(spec, "en", map[string]any{"caller": "13800138000"}, log),
		&fakeActions{}, backend, nil, log)
}

func secretsFrom(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

const idParams = `{"type":"object","properties":{"id":{"type":"string"},"note":{"type":"string"}},"required":["id"]}`

// A tool's own baseUrl wins over the deployment's, and its path is appended to
// the baseUrl's own path.
func TestAToolsBaseURLWinsOverTheDeploymentBase(t *testing.T) {
	t.Parallel()
	deployment := newTestBackend(t, nil)
	own := newTestBackend(t, nil)
	spec := toolFlow(t, idParams, `{"baseUrl": "`+own.URL+`/v1/", "path": "/items/{args.id}", "method": "GET"}`)
	r := runtimeFor(spec, NewBackend(deployment.URL), nil)

	output, _ := r.Dispatch(t.Context(), "lookup", `{"id":"A1"}`)
	if got := decodeOutput(t, output); got["ok"] != "1" {
		t.Fatalf("output = %v, want ok", got)
	}
	if got := own.received(); len(got) != 1 || got[0].requestURI != "/v1/items/A1" {
		t.Errorf("the tool's own backend received %+v, want one GET /v1/items/A1", got)
	}
	if got := deployment.received(); len(got) != 0 {
		t.Errorf("the deployment backend received %d requests, want none", len(got))
	}

	// Without a baseUrl the deployment's base is used.
	spec = toolFlow(t, idParams, `{"path": "/items/{args.id}", "method": "GET"}`)
	r = runtimeFor(spec, NewBackend(deployment.URL), nil)
	r.Dispatch(t.Context(), "lookup", `{"id":"B2"}`)
	if got := deployment.received(); len(got) != 1 || got[0].requestURI != "/items/B2" {
		t.Errorf("the deployment backend received %+v, want one GET /items/B2", got)
	}

	// The simulator pins the base, so even a tool with its own goes to it.
	spec = toolFlow(t, idParams, `{"baseUrl": "`+own.URL+`", "path": "/items/{args.id}", "method": "GET"}`)
	r = runtimeFor(spec, NewBackend(deployment.URL, WithPinnedBase()), nil)
	r.Dispatch(t.Context(), "lookup", `{"id":"C3"}`)
	if got := deployment.received(); len(got) != 2 || got[1].requestURI != "/items/C3" {
		t.Errorf("a pinned backend did not keep the call: %+v", got)
	}
}

// The deployment's own base may carry a user and password, as it could before
// tools had headers; it is sent as basic auth and kept out of the log.
func TestTheDeploymentBaseKeepsItsCredentials(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	base := strings.Replace(server.URL, "http://", "http://ops:base-pass-31415@", 1)
	logs := &logCapture{}
	r := runtimeFor(toolFlow(t, idParams, `{"path": "/x", "method": "GET"}`), NewBackend(base), logs.logger())
	r.Dispatch(t.Context(), "lookup", `{"id":"1"}`)
	got := server.received()
	if len(got) != 1 || !strings.HasPrefix(got[0].header.Get("Authorization"), "Basic ") {
		t.Fatalf("received %+v, want basic auth from the base", got)
	}
	if strings.Contains(logs.String(), "base-pass-31415") {
		t.Errorf("the log carries the base's password:\n%s", logs.String())
	}
}

// A secret header reaches the backend, alone and inside other text, and the
// value appears nowhere else: not in the log, not in what the model is told,
// not in the slots, not in an error.
func TestASecretHeaderReachesTheBackendAndNothingElse(t *testing.T) {
	t.Parallel()
	const bearer, apiKey = "kb-bearer-9f8e7d6c5b4a", "api-key-0a1b2c3d4e5f"
	server := newTestBackend(t, nil)
	spec := toolFlow(t, idParams, `{
	  "path": "/search", "method": "POST",
	  "headers": {
	    "Authorization": "Bearer {secrets.KB_KEY}",
	    "X-Api-Key": "{secrets.KB_API_KEY}",
	    "X-Client": "aicc"
	  },
	  "body": {"q": "{args.id}"},
	  "result": {"answer": "data.answer"}
	}`)
	logs := &logCapture{}
	backend := NewBackend(server.URL, WithSecrets(secretsFrom(map[string]string{
		"AICC_TOOL_SECRET_KB_KEY": bearer, "AICC_TOOL_SECRET_KB_API_KEY": apiKey,
	})))
	r := runtimeFor(spec, backend, logs.logger())

	output, _ := r.Dispatch(t.Context(), "lookup", `{"id":"what are the fees"}`)
	got := server.received()
	if len(got) != 1 {
		t.Fatalf("backend received %d requests, want 1", len(got))
	}
	if h := got[0].header; h.Get("Authorization") != "Bearer "+bearer || h.Get("X-Api-Key") != apiKey || h.Get("X-Client") != "aicc" {
		t.Errorf("headers = %v, want the resolved secrets and the literal", h)
	}

	// A failing backend: the error the model reads and the log both name the
	// path, never a header.
	failing := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Bearer "+bearer, http.StatusUnauthorized)
	})
	failingSpec := toolFlow(t, idParams, strings.Replace(`{"baseUrl": "BASE", "path": "/search",
	  "headers": {"Authorization": "Bearer {secrets.KB_KEY}"}}`, "BASE", failing.URL, 1))
	r2 := runtimeFor(failingSpec, backend, logs.logger())
	failedOutput, _ := r2.Dispatch(t.Context(), "lookup", `{"id":"x"}`)
	if decodeOutput(t, failedOutput)["ok"] != "0" {
		t.Fatalf("a 401 was not a failure: %s", failedOutput)
	}

	for where, text := range map[string]string{
		"the log":                 logs.String(),
		"the tool result":         output,
		"the failed tool result":  failedOutput,
		"the slots":               fmt.Sprint(r.engine.Slots()),
		"the failed call's slots": fmt.Sprint(r2.engine.Slots()),
	} {
		for _, secret := range []string{bearer, apiKey} {
			if strings.Contains(text, secret) {
				t.Errorf("%s contains a secret value:\n%s", where, text)
			}
		}
	}
}

// A secret that is unset when the call is made fails the tool with a message
// naming the header and the variable; one with a line break is refused too,
// and its value is not in the message.
func TestAnUnusableSecretFailsTheToolByName(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t, idParams, `{"path": "/search",
	  "headers": {"Authorization": "Bearer {secrets.KB_KEY}"}}`)

	for _, tc := range []struct {
		name    string
		secrets map[string]string
		want    string
	}{
		{"unset", map[string]string{}, "header Authorization needs AICC_TOOL_SECRET_KB_KEY, which is not set"},
		{"empty", map[string]string{"AICC_TOOL_SECRET_KB_KEY": ""}, "AICC_TOOL_SECRET_KB_KEY, which is not set"},
		{"line break", map[string]string{"AICC_TOOL_SECRET_KB_KEY": "abc\r\nX-Evil: 1"}, "contains a line break"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewBackend(server.URL, WithSecrets(secretsFrom(tc.secrets))).
				Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "x"}, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "X-Evil") {
				t.Errorf("the error carries the value: %v", err)
			}
		})
	}
	if n := server.connections(t); n != 0 {
		t.Errorf("%d connections, want none", n)
	}
}

// A backend that answers in 600 ms fails a tool bounded at 500 ms and passes
// one bounded at 1000 ms.
func TestATimeoutIsPerTool(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(600 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	backend := NewBackend(server.URL)
	for _, tc := range []struct {
		timeoutMs int
		isOK      bool
	}{{500, false}, {1000, true}} {
		spec := toolFlow(t, idParams, fmt.Sprintf(`{"path": "/slow", "timeoutMs": %d}`, tc.timeoutMs))
		_, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "x"}, nil)
		if (err == nil) != tc.isOK {
			t.Errorf("timeoutMs %d: err = %v, want ok=%v", tc.timeoutMs, err, tc.isOK)
		}
		if err != nil && !strings.Contains(err.Error(), fmt.Sprintf("did not answer within %d ms", tc.timeoutMs)) {
			t.Errorf("timeoutMs %d: err = %v, want it to say the bound", tc.timeoutMs, err)
		}
	}
	if spec := toolFlow(t, idParams, `{"path": "/x"}`); spec.Tools["lookup"].HTTP.Timeout() != 5*time.Second {
		t.Errorf("default timeout = %s, want 5s", spec.Tools["lookup"].HTTP.Timeout())
	}
}

// A path value is escaped into exactly one segment: it cannot add a segment,
// start a query, end in a fragment or change the host.
func TestAPathValueStaysOneSegment(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t, `{"type":"object","properties":{"id":{}},"required":["id"]}`,
		`{"path": "/api/items/{args.id}/detail", "method": "GET"}`)
	backend := NewBackend(server.URL)

	for _, tc := range []struct {
		value any
		want  string
	}{
		{"@evil.com/x", "/api/items/@evil.com%2Fx/detail"},
		{"a/b", "/api/items/a%2Fb/detail"},
		{"x?debug=1", "/api/items/x%3Fdebug=1/detail"},
		{"a#b", "/api/items/a%23b/detail"},
		{"%2e%2e", "/api/items/%252e%252e/detail"},
		{"中文", "/api/items/%E4%B8%AD%E6%96%87/detail"},
		{float64(42), "/api/items/42/detail"},
		{"order 12 34", "/api/items/order%2012%2034/detail"},
	} {
		result, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": tc.value}, nil)
		if err != nil || !result.IsOK {
			t.Errorf("%v: result %+v, err %v", tc.value, result, err)
			continue
		}
		got := server.received()
		last := got[len(got)-1]
		if last.requestURI != tc.want || last.rawQuery != "" {
			t.Errorf("%v: request URI %q, want %q", tc.value, last.requestURI, tc.want)
		}
	}
}

// Values that are not one path segment are refused before anything is sent,
// as a refusal the model can correct, naming the reference and not the value.
func TestAPathValueThatIsNotASegmentIsRefusedWithoutAConnection(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t, `{"type":"object","properties":{"id":{}},"required":["id"]}`,
		`{"path": "/api/items/{args.id}", "method": "GET"}`)
	logs := &logCapture{}
	r := runtimeFor(spec, NewBackend(server.URL), logs.logger())

	for _, args := range []string{`{"id":""}`, `{"id":"."}`, `{"id":".."}`, `{"id":{"a":"secret-object"}}`, `{"id":["l1","l2"]}`, `{}`} {
		output, _ := r.Dispatch(t.Context(), "lookup", args)
		got := decodeOutput(t, output)
		if got["ok"] != "0" || !strings.Contains(fmt.Sprint(got["error"]), "{args.id}") {
			t.Errorf("%s: output %v, want a refusal naming {args.id}", args, got)
		}
		if _, hasHint := got["hint"]; hasHint {
			t.Errorf("%s: a correctable refusal carries the failure hint: %v", args, got)
		}
	}
	if strings.Contains(logs.String(), "secret-object") {
		t.Errorf("the log carries a rendered value:\n%s", logs.String())
	}
	if n := server.connections(t); n != 0 {
		t.Errorf("%d connections, want none", n)
	}
}

// Query values round-trip intact, encoded as form values, and a parameter whose
// optional argument is absent is left out.
func TestQueryValuesRoundTripAndAbsentOnesAreLeftOut(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t,
		`{"type":"object","properties":{"q":{},"page":{},"lang":{}},"required":["q"]}`,
		`{"path": "/search", "method": "GET",
		  "query": {"q": "{args.q}", "page": "{args.page}", "caller": "{slots.caller}", "fixed": "v=1&w"}}`)
	backend := NewBackend(server.URL)

	for _, q := range []string{"a&b=c", "1+1", "hello world", "中文 问题", "100%"} {
		if _, err := backend.Call(t.Context(), nil, spec.Tools["lookup"],
			map[string]any{"q": q}, map[string]any{"caller": "+86 139"}); err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		got := server.received()
		last := got[len(got)-1]
		if last.query.Get("q") != q || last.query.Get("caller") != "+86 139" || last.query.Get("fixed") != "v=1&w" {
			t.Errorf("%q: backend read %v from %q", q, last.query, last.rawQuery)
		}
		if _, ok := last.query["page"]; ok {
			t.Errorf("%q: the absent optional page was sent: %q", q, last.rawQuery)
		}
		if strings.Count(last.rawQuery, "&") != 2 {
			t.Errorf("%q: raw query %q has an injected parameter", q, last.rawQuery)
		}
	}

	if _, err := backend.Call(t.Context(), nil, spec.Tools["lookup"],
		map[string]any{"q": "x", "page": float64(2)}, nil); err != nil {
		t.Fatal(err)
	}
	got := server.received()
	if last := got[len(got)-1]; last.query.Get("page") != "2" {
		t.Errorf("page = %q, want 2", last.query.Get("page"))
	}

	result, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"q": map[string]any{"a": 1}}, nil)
	if err != nil || result.IsOK || !strings.Contains(result.Error, "query parameter q") {
		t.Errorf("an object in the query: result %+v, err %v, want a refusal", result, err)
	}
}

// A tool with no body template sends what it always has: "{}" as JSON for
// POST, PUT and PATCH (a backend may read a JSON body), and nothing for GET,
// DELETE and HEAD. One with a body template sends it as JSON.
func TestNoBodyTemplateKeepsTheOldBody(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	backend := NewBackend(server.URL)
	for _, tc := range []struct {
		method, body string
	}{
		{"", "{}"}, {"POST", "{}"}, {"put", "{}"}, {"PATCH", "{}"},
		{"GET", ""}, {"DELETE", ""}, {"HEAD", ""},
	} {
		spec := toolFlow(t, idParams, `{"path": "/x", "method": "`+tc.method+`"}`)
		_, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "1"}, nil)
		if err != nil && tc.method != "HEAD" { // a HEAD answer has no object to read
			t.Fatalf("%s: %v", tc.method, err)
		}
		got := server.received()
		last := got[len(got)-1]
		wantType := ""
		if tc.body != "" {
			wantType = "application/json"
		}
		if string(last.body) != tc.body || last.header.Get("Content-Type") != wantType {
			t.Errorf("%q with no body template sent body %q as %q, want %q as %q",
				tc.method, last.body, last.header.Get("Content-Type"), tc.body, wantType)
		}
	}
	spec := toolFlow(t, idParams, `{"path": "/x", "body": {"id": "{args.id}"}}`)
	if _, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "7"}, nil); err != nil {
		t.Fatal(err)
	}
	got := server.received()
	if last := got[len(got)-1]; string(last.body) != `{"id":"7"}` || last.header.Get("Content-Type") != "application/json" {
		t.Errorf("body template sent %q as %q", last.body, last.header.Get("Content-Type"))
	}
}

// A path written before templates existed loads and goes on the wire exactly
// as the old code sent it: base and path joined as text, parsed as one URL,
// and its request URI sent. A literal query is kept, a fragment dropped, and
// spaces and non-ASCII characters escaped.
func TestALegacyPathKeepsItsWireForm(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	for _, base := range []string{server.URL, server.URL + "/v1"} {
		backend := NewBackend(base)
		for _, path := range []string{
			"/api/lookup?format=json",
			"/api/lookup?format=json&lang=zh-CN;x=1",
			"/api/lookup?q=a%20b&r=%E4%B8%AD&s=+",
			"/api/lookup#section",
			"/api/lookup?format=json#frag",
			"/api/查询/我的 items",
			"/api/a b/c%2Fd",
			"/api/{id}/x",
			"/api/x?",
		} {
			raw, _ := json.Marshal(path)
			spec := toolFlow(t, "", `{"path": `+string(raw)+`, "method": "GET"}`)
			if _, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], nil, nil); err != nil {
				t.Errorf("%s%s: %v", base, path, err)
				continue
			}
			old, err := url.Parse(strings.TrimSuffix(base, "/") + path)
			if err != nil {
				t.Fatalf("the old code could not have sent %s: %v", path, err)
			}
			got := server.received()
			if want := old.RequestURI(); got[len(got)-1].requestURI != want {
				t.Errorf("%s%s: sent %q, the old code sent %q", base, path, got[len(got)-1].requestURI, want)
			}
		}
	}
}

// A literal query that references a value is a set of query templates: its
// values are encoded like http.query's, merged with them, and an absent
// optional argument leaves its parameter out.
func TestATemplatedQueryInThePathIsEncoded(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t,
		`{"type":"object","properties":{"q":{},"page":{},"top":{}},"required":["q"]}`,
		`{"path": "/search?q={args.q}&fmt=json&top={args.top}", "method": "GET", "query": {"page": "{args.page}"}}`)
	backend := NewBackend(server.URL)
	if _, err := backend.Call(t.Context(), nil, spec.Tools["lookup"],
		map[string]any{"q": "a&b=c 中", "page": float64(2)}, nil); err != nil {
		t.Fatal(err)
	}
	got := server.received()
	last := got[len(got)-1]
	if last.query.Get("q") != "a&b=c 中" || last.query.Get("fmt") != "json" || last.query.Get("page") != "2" {
		t.Errorf("backend read %v from %q", last.query, last.rawQuery)
	}
	if _, ok := last.query["top"]; ok || !strings.HasPrefix(last.requestURI, "/search?") {
		t.Errorf("request %q: the absent top was sent or the path is wrong", last.requestURI)
	}
}

// The log records the path template and the query parameter names, the base
// and the timeout, and never a rendered value.
func TestTheLogCarriesTemplatesNotValues(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, nil)
	spec := toolFlow(t,
		`{"type":"object","properties":{"order":{},"phone":{}},"required":["order"]}`,
		`{"path": "/orders/{args.order}", "method": "GET", "timeoutMs": 3000,
		  "query": {"phone": "{args.phone}"}}`)
	logs := &logCapture{}
	r := runtimeFor(spec, NewBackend(server.URL), logs.logger())
	r.Dispatch(t.Context(), "lookup", `{"order":"ORD-98765","phone":"13912345678"}`)

	text := logs.String()
	for _, want := range []string{`"path":"/orders/{args.order}"`, `"queryParams":["phone"]`, `"timeoutMs":3000`, `"baseUrl":"` + server.URL + `"`} {
		if !strings.Contains(text, want) {
			t.Errorf("the log lacks %s:\n%s", want, text)
		}
	}
	for _, value := range []string{"ORD-98765", "13912345678"} {
		if strings.Contains(text, value) {
			t.Errorf("the log carries the rendered value %s:\n%s", value, text)
		}
	}
}

// fakeResolver answers names from a table and routes every allowed connection
// to one test server, so public addresses can be exercised without a network.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]string
	calls   map[string]int
}

func (f *fakeResolver) resolve(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	answers := f.answers[host]
	if len(answers) == 0 {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	i := min(f.calls[host], len(answers)-1)
	f.calls[host]++
	return []netip.Addr{netip.MustParseAddr(answers[i])}, nil
}

func routeTo(server *testBackend) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, server.Listener.Addr().String())
	}
}

// The outbound host policy on a call, end to end: every refusal is a tool
// failure that opened no connection, logs host, address and rule, and a
// listed public host is reached.
func TestTheHostPolicyAppliesToEveryCall(t *testing.T) {
	t.Parallel()
	server := newTestBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		// No pooled connection: each call resolves and connects afresh.
		w.Header().Set("Connection", "close")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	resolver := &fakeResolver{calls: map[string]int{}, answers: map[string][]string{
		"kb.example.com":    {"203.0.113.10"},
		"other.example.com": {"203.0.113.11"},
		"lan.internal":      {"10.1.2.3"},
		"rebind.internal":   {"192.168.1.20", "169.254.169.254"},
		"mapped.internal":   {"::ffff:169.254.169.254"},
	}}
	allowed, err := egress.ParseAllowList("kb.example.com, 198.51.100.7")
	if err != nil {
		t.Fatal(err)
	}
	logs := &logCapture{}
	backend := NewBackend("", WithAllowedHosts(allowed), WithLogger(logs.logger()))
	backend.dialer.Resolver = resolver.resolve
	backend.dialer.Connect = routeTo(server)

	call := func(base string) error {
		spec := toolFlow(t, idParams, `{"baseUrl": "`+base+`", "path": "/x", "method": "GET"}`)
		_, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "1"}, nil)
		return err
	}

	for _, tc := range []struct {
		base string
		rule egress.Rule
	}{
		{"http://169.254.169.254", egress.RuleLinkLocal},
		{"http://[::ffff:169.254.169.254]", egress.RuleLinkLocal},
		{"http://[fe80::1]", egress.RuleLinkLocal},
		{"http://0.0.0.0:8080", egress.RuleUnspecified},
		{"http://2130706433", egress.RuleNonCanonical},
		{"http://0x7f.1", egress.RuleNonCanonical},
		{"http://other.example.com", egress.RulePublicNotListed},
		{"http://198.51.100.8", egress.RulePublicNotListed},
		{"http://mapped.internal", egress.RuleLinkLocal},
	} {
		err := call(tc.base)
		refusal, ok := egress.IsRefusal(err)
		if !ok || refusal.Rule != tc.rule {
			t.Errorf("%s: err = %v, want a %s refusal", tc.base, err, tc.rule)
		}
	}
	if n := server.connections(t); n != 0 {
		t.Fatalf("%d connections after refusals, want none", n)
	}
	if text := logs.String(); !strings.Contains(text, `"host":"other.example.com"`) ||
		!strings.Contains(text, `"address":"203.0.113.11:80"`) || !strings.Contains(text, `"rule":"PUBLIC_NOT_LISTED"`) {
		t.Errorf("the refusal log does not name host, address and rule:\n%s", text)
	}

	// Allowed: a listed name, a listed IP literal, a private address.
	for _, base := range []string{"http://kb.example.com", "http://198.51.100.7", "http://lan.internal:8770"} {
		if err := call(base); err != nil {
			t.Errorf("%s: %v", base, err)
		}
	}
	if n := server.connections(t); n != 3 {
		t.Errorf("%d connections, want 3", n)
	}

	// A name that resolves privately once and to the metadata address next is
	// refused at the second connect.
	if err := call("http://rebind.internal"); err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	if refusal, ok := egress.IsRefusal(call("http://rebind.internal")); !ok || refusal.Address != "169.254.169.254:80" {
		t.Errorf("second resolution was not refused at connect")
	}
	if n := server.connections(t); n != 4 {
		t.Errorf("%d connections, want 4", n)
	}
}

// A redirect is not followed, wherever it points: the 3xx is a tool failure
// and only the first request is made.
func TestARedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	var target string
	server := newTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
	backend := NewBackend(server.URL)
	spec := toolFlow(t, idParams, `{"path": "/x", "method": "GET"}`)
	for _, to := range []string{"http://169.254.169.254/latest/meta-data/", server.URL + "/elsewhere"} {
		target = to
		before := len(server.received())
		_, err := backend.Call(t.Context(), nil, spec.Tools["lookup"], map[string]any{"id": "1"}, nil)
		if err == nil || !strings.Contains(err.Error(), "redirect (302)") {
			t.Errorf("redirect to %s: err = %v, want a redirect failure", to, err)
		}
		if got := len(server.received()) - before; got != 1 {
			t.Errorf("redirect to %s: %d requests, want only the first", to, got)
		}
	}
}

// Through Dispatch, a refusal by the host policy is a tool failure the model
// reads with the usual recovery hint, not a crash.
func TestAPolicyRefusalIsAToolFailure(t *testing.T) {
	t.Parallel()
	spec := toolFlow(t, idParams, `{"baseUrl": "http://169.254.169.254", "path": "/latest/meta-data", "method": "GET"}`)
	r := runtimeFor(spec, NewBackend(""), nil)
	output, moved := r.Dispatch(t.Context(), "lookup", `{"id":"1"}`)
	got := decodeOutput(t, output)
	if got["ok"] != "0" || moved != "" || !strings.Contains(fmt.Sprint(got["error"]), "outbound host policy") {
		t.Errorf("output %v, want a failure naming the policy", got)
	}
}
