// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// Every load-time refusal of an HTTP tool's block, one malformed field at a
// time, each naming the tool and what to do about it.
func TestTheLoaderRefusesAnUnsafeOrUnusableHTTPBlock(t *testing.T) {
	t.Parallel()
	const params = `{"type":"object","properties":{"id":{"type":"string"},"note":{"type":"string"}},"required":["id"]}`
	for _, tc := range []struct {
		name, http, want string
	}{
		// Credentials written as literals.
		{"literal Authorization", `{"path": "/x", "headers": {"Authorization": "Bearer abc123"}}`, "header Authorization carries a credential as a literal"},
		{"literal lower-case authorization", `{"path": "/x", "headers": {"authorization": "abc123"}}`, "carries a credential as a literal"},
		{"literal X-Api-Key", `{"path": "/x", "headers": {"X-Api-Key": "abc123"}}`, "header X-Api-Key carries a credential as a literal"},
		{"literal Cookie", `{"path": "/x", "headers": {"Cookie": "session=abc"}}`, "header Cookie carries a credential as a literal"},
		{"literal X-Auth-Token", `{"path": "/x", "headers": {"X-Auth-Token": "abc"}}`, "carries a credential as a literal"},
		{"literal X-Client-Secret", `{"path": "/x", "headers": {"X-Client-Secret": "abc"}}`, "carries a credential as a literal"},
		{"malformed secret in a credential header", `{"path": "/x", "headers": {"Authorization": "Bearer {secrets.kb_key}"}}`, "malformed secret reference"},
		{"unterminated secret", `{"path": "/x", "headers": {"X-Trace": "{secrets.KB"}}`, "malformed secret reference"},

		// Timeouts.
		{"timeout 499", `{"path": "/x", "timeoutMs": 499}`, "timeoutMs is 499"},
		{"timeout 10001", `{"path": "/x", "timeoutMs": 10001}`, "timeoutMs is 10001"},
		{"timeout 0", `{"path": "/x", "timeoutMs": 0}`, "timeoutMs is 0"},

		// Base URLs.
		{"relative baseUrl", `{"baseUrl": "/kb", "path": "/x"}`, "must be an absolute http:// or https:// URL"},
		{"baseUrl with no scheme", `{"baseUrl": "kb.example.com", "path": "/x"}`, "must be an absolute http:// or https:// URL"},
		{"ftp baseUrl", `{"baseUrl": "ftp://kb.example.com", "path": "/x"}`, "must be an absolute http:// or https:// URL"},
		{"baseUrl with credentials", `{"baseUrl": "http://user:pw@kb.example.com", "path": "/x"}`, "carries a user name or password"},
		{"baseUrl with a query", `{"baseUrl": "http://kb.example.com/?tenant=1", "path": "/x"}`, "has a query"},
		{"baseUrl with a fragment", `{"baseUrl": "http://kb.example.com/#a", "path": "/x"}`, "has a fragment"},
		{"baseUrl with no host", `{"baseUrl": "http:///x", "path": "/x"}`, "names no host"},
		{"templated baseUrl", `{"baseUrl": "http://{args.id}.example.com", "path": "/x"}`, "is a template"},

		// Secrets anywhere but a header.
		{"secret in the body", `{"path": "/x", "body": {"key": "{secrets.KB_KEY}"}}`, "body references a secret"},
		{"secret nested in the body", `{"path": "/x", "body": {"auth": {"list": ["{secrets.KB_KEY}"]}}}`, "body references a secret"},
		{"secret in the path", `{"path": "/x/{secrets.KB_KEY}"}`, "path references a secret"},
		{"secret in the query", `{"path": "/x", "query": {"key": "{secrets.KB_KEY}"}}`, "query parameter \"key\" references a secret"},

		// Paths.
		{"optional argument in the path", `{"path": "/x/{args.note}"}`, "references {args.note}, which the tool's parameters do not require"},
		{"undeclared argument in the path", `{"path": "/x/{args.order}"}`, "references {args.order}, which is not a declared parameter"},
		{"undeclared slot in the path", `{"path": "/x/{slots.nothing}"}`, "references {slots.nothing}, which nothing in this flow writes"},
		{"path starting with a reference", `{"path": "{args.id}/x"}`, "path must start with a literal \"/\""},
		{"relative path", `{"path": "api/x"}`, "path must start with a literal \"/\""},
		{"query in the path", `{"path": "/x?debug=1"}`, "put query parameters in http.query"},
		{"fragment in the path", `{"path": "/x#top"}`, "put query parameters in http.query"},
		{"space in the path", `{"path": "/my items"}`, "characters a URL path cannot carry"},
		{"stray brace in the path", `{"path": "/x/{id}"}`, "characters a URL path cannot carry"},
		{"bad escape in the path", `{"path": "/x/%zz"}`, "characters a URL path cannot carry"},

		// Queries.
		{"undeclared argument in the query", `{"path": "/x", "query": {"o": "{args.order}"}}`, "query parameter \"o\" references {args.order}"},
		{"undeclared slot in the query", `{"path": "/x", "query": {"s": "{slots.nothing}"}}`, "references {slots.nothing}"},
		{"query parameter with no name", `{"path": "/x", "query": {" ": "a"}}`, "query parameter with no name"},

		// Headers.
		{"model value in a header", `{"path": "/x", "headers": {"X-Order": "{args.id}"}}`, "a header takes a literal or {secrets.NAME} only"},
		{"reserved header", `{"path": "/x", "headers": {"Host": "evil.example.com"}}`, "header Host is set by the HTTP client"},
		{"invalid header name", `{"path": "/x", "headers": {"X Bad": "1"}}`, "is not a valid header name"},
		{"header twice", `{"path": "/x", "headers": {"X-Trace": "1", "x-trace": "2"}}`, "declares header"},
		{"line break in a header", `{"path": "/x", "headers": {"X-Trace": "1\r\nX-Evil: 2"}}`, "line break"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(toolFlowJSON(params, tc.http)))
			var invalid *ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("err = %v, want a ValidationError", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `tool "lookup"`) {
				t.Errorf("problems = %v, want one containing %q", invalid.Problems, tc.want)
			}
		})
	}

	// A secret reference in a collect value is refused like one in a body.
	collect := strings.Replace(toolFlowJSON(params, `{"path": "/x"}`), `"http":`,
		`"collect": {"slot": "found", "value": "{secrets.KB_KEY}"}, "http":`, 1)
	if _, err := Load([]byte(collect)); err == nil || !strings.Contains(err.Error(), "collect value references a secret") {
		t.Errorf("a secret in a collect value: %v", err)
	}
}

// Everything the new fields allow, together, loads.
func TestTheLoaderAcceptsAFullHTTPBlock(t *testing.T) {
	t.Parallel()
	spec := toolFlow(t,
		`{"type":"object","properties":{"id":{"type":"string"},"page":{"type":"integer"}},"required":["id"]}`,
		`{
		  "baseUrl": "https://kb.example.com:8443/v2",
		  "path": "/items/{args.id}/by-caller/{slots.caller}/%E4%B8%AD;v=1",
		  "method": "GET",
		  "query": {"page": "{args.page}", "lang": "{slots.lang}", "fixed": "a b"},
		  "headers": {
		    "Authorization": "Bearer {secrets.KB_KEY}",
		    "X-Api-Key": "{secrets.KB_API_KEY}",
		    "Cookie": "sid={secrets.KB_SESSION}; theme=dark",
		    "Accept-Language": "zh-CN",
		    "X-Keep": "literal"
		  },
		  "timeoutMs": 10000
		}`)
	call := spec.Tools["lookup"].HTTP
	if call.Timeout().Milliseconds() != 10000 || len(call.Headers) != 5 || len(call.Query) != 3 {
		t.Errorf("loaded %+v", call)
	}
	for _, bound := range []int{MinToolTimeoutMs, MaxToolTimeoutMs} {
		toolFlow(t, `{"type":"object","properties":{}}`, `{"path": "/x", "timeoutMs": `+strconv.Itoa(bound)+`}`)
	}
}

// An unset secret is an error at publish and only a warning on a draft: the
// rule reports it, naming tool, header and variable, and loading does not.
func TestAnUnsetSecretBlocksPublishNotLoad(t *testing.T) {
	t.Parallel()
	raw := toolFlowJSON(`{"type":"object","properties":{}}`, `{"path": "/x",
	  "headers": {"Authorization": "Bearer {secrets.KB_KEY}", "X-Api-Key": "{secrets.KB_API_KEY}"}}`)
	spec, err := Load([]byte(raw))
	if err != nil {
		t.Fatalf("a draft referencing an unset secret did not load: %v", err)
	}

	rule := RequireToolSecrets(secretsFrom(map[string]string{"AICC_TOOL_SECRET_KB_API_KEY": "set", "AICC_TOOL_SECRET_KB_KEY": ""}))
	err = rule(spec)
	var invalid *ValidationError
	if !errors.As(err, &invalid) || len(invalid.Problems) != 1 {
		t.Fatalf("err = %v, want one problem", err)
	}
	for _, want := range []string{`tool "lookup"`, "header Authorization", "AICC_TOOL_SECRET_KB_KEY"} {
		if !strings.Contains(invalid.Problems[0], want) {
			t.Errorf("problem %q does not name %s", invalid.Problems[0], want)
		}
	}

	all := RequireToolSecrets(secretsFrom(map[string]string{
		"AICC_TOOL_SECRET_KB_KEY": "a", "AICC_TOOL_SECRET_KB_API_KEY": "b",
	}))
	if err := all(spec); err != nil {
		t.Errorf("every secret set: %v", err)
	}
}
