// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// What an HTTP tool's block may say, checked at load, and how its path and
// query are rendered on a call.
//
// The path and the query take values the model supplies. A value is therefore
// never trusted to keep to its place: each one is escaped into the single path
// segment or the single query value it stands for, the URL is assembled from
// url.URL fields rather than pasted together as text, and the host comes only
// from the base URL. A value such as "@evil.com/x" can then neither change the
// host nor add a segment.

// pathLiteral is what the literal part of a path may contain: the characters
// RFC 3986 allows in a path, and percent escapes. No "?" or "#": a query goes
// in http.query, and nothing in a backend call has a fragment.
var pathLiteral = regexp.MustCompile(`^(?:[A-Za-z0-9\-._~!$&'()*+,;=:@/]|%[0-9A-Fa-f]{2})*$`)

// headerName is an HTTP field name (RFC 9110 token).
var headerName = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")

// reservedHeaders are set by the HTTP client from the request itself; a flow
// that set them would either be ignored or break the framing.
var reservedHeaders = []string{"host", "content-length", "transfer-encoding", "connection", "te", "upgrade", "trailer", "keep-alive"}

// validateHTTPCall checks one HTTP tool's block beyond the presence of a path.
func validateHTTPCall(d declaredSlots, name string, tool Tool, report func(string, ...any)) {
	call := tool.HTTP
	declared := toolArgNames(tool)
	required := toolRequiredArgs(tool)

	if call.BaseURL != "" {
		if problem := baseURLProblem(call.BaseURL); problem != "" {
			report("tool %q baseUrl %s", name, problem)
		}
	}
	if t := call.TimeoutMs; t != nil && (*t < MinToolTimeoutMs || *t > MaxToolTimeoutMs) {
		report("tool %q timeoutMs is %d; it is a number of milliseconds from %d to %d",
			name, *t, MinToolTimeoutMs, MaxToolTimeoutMs)
	}

	// The path.
	if call.Path != "" {
		if !strings.HasPrefix(call.Path, "/") {
			report("tool %q path must start with a literal \"/\"", name)
		}
		if mentionsSecret(call.Path) {
			report("tool %q path references a secret; {secrets.NAME} may appear only in a header value", name)
		}
		literal := templateReference.ReplaceAllString(call.Path, "")
		if strings.ContainsAny(literal, "?#") {
			report("tool %q path contains \"?\" or \"#\"; put query parameters in http.query", name)
		} else if !mentionsSecret(literal) && !pathLiteral.MatchString(literal) {
			report("tool %q path has characters a URL path cannot carry as written (a space, a brace, a bad %% escape); escape them, or reference a value with {args.x} or {slots.y}", name)
		}
		for _, m := range templateReference.FindAllStringSubmatch(call.Path, -1) {
			switch {
			case m[1] == "args" && !slices.Contains(declared, m[2]):
				report("tool %q path references {args.%s}, which is not a declared parameter of the tool", name, m[2])
			case m[1] == "args" && !slices.Contains(required, m[2]):
				report("tool %q path references {args.%s}, which the tool's parameters do not require; a path segment cannot be left out, so list it in required or move it to http.query", name, m[2])
			case m[1] == "slots" && !d.has(m[2]):
				report("tool %q path references {slots.%s}, which nothing in this flow writes", name, m[2])
			}
		}
	}

	// The query.
	for _, param := range sortedKeys(call.Query) {
		value := call.Query[param]
		if strings.TrimSpace(param) == "" {
			report("tool %q has a query parameter with no name", name)
		}
		if mentionsSecret(value) {
			report("tool %q query parameter %q references a secret; {secrets.NAME} may appear only in a header value", name, param)
		}
		for _, m := range templateReference.FindAllStringSubmatch(value, -1) {
			switch {
			case m[1] == "args" && !slices.Contains(declared, m[2]):
				report("tool %q query parameter %q references {args.%s}, which is not a declared parameter of the tool", name, param, m[2])
			case m[1] == "slots" && !d.has(m[2]):
				report("tool %q query parameter %q references {slots.%s}, which nothing in this flow writes", name, param, m[2])
			}
		}
	}

	// The headers.
	seen := map[string]string{}
	for _, header := range sortedKeys(call.Headers) {
		value := call.Headers[header]
		lower := strings.ToLower(header)
		switch {
		case !headerName.MatchString(header):
			report("tool %q header %q is not a valid header name", name, header)
		case slices.Contains(reservedHeaders, lower):
			report("tool %q header %s is set by the HTTP client and cannot be given", name, header)
		}
		if other, dup := seen[lower]; dup {
			report("tool %q declares header %s twice (as %s and %s)", name, header, other, header)
		}
		seen[lower] = header
		if strings.ContainsAny(value, "\r\n\x00") {
			report("tool %q header %s has a line break or a NUL in its value", name, header)
		}
		if templateReference.MatchString(value) {
			report("tool %q header %s references {args.x} or {slots.y}; a header takes a literal or {secrets.NAME} only", name, header)
		}
		if hasMalformedSecret(value) {
			report("tool %q header %s has a malformed secret reference; write {secrets.NAME} with NAME in capital letters, digits and underscores", name, header)
		}
		if isCredentialHeader(header) && len(secretNames(value)) == 0 {
			report("tool %q header %s carries a credential as a literal; a flow is readable by everyone with config:read, so reference it as {secrets.NAME} and set %sNAME on the deployment", name, header, SecretEnvPrefix)
		}
	}

	if bodyMentionsSecret(call.Body) {
		report("tool %q body references a secret; {secrets.NAME} may appear only in a header value", name)
	}
	if tool.Collect != nil && mentionsSecret(tool.Collect.Value) {
		report("tool %q collect value references a secret; {secrets.NAME} may appear only in a header value", name)
	}
}

// baseURLProblem says what is wrong with a tool's baseUrl, or "" when nothing.
func baseURLProblem(raw string) string {
	if mentionsSecret(raw) || templateReference.MatchString(raw) {
		return "is a template; it must be a literal URL"
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "is not a URL"
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Sprintf("%q must be an absolute http:// or https:// URL", raw)
	case u.Host == "" || u.Hostname() == "":
		return fmt.Sprintf("%q names no host", raw)
	case u.User != nil:
		return "carries a user name or password; put a credential in a header as {secrets.NAME}"
	case u.RawQuery != "" || u.ForceQuery:
		return fmt.Sprintf("%q has a query; put query parameters in http.query", raw)
	case u.Fragment != "":
		return fmt.Sprintf("%q has a fragment", raw)
	}
	return ""
}

func bodyMentionsSecret(v any) bool {
	switch x := v.(type) {
	case string:
		return mentionsSecret(x)
	case map[string]any:
		for key, item := range x {
			if mentionsSecret(key) || bodyMentionsSecret(item) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(x, bodyMentionsSecret)
	}
	return false
}

// toolRequiredArgs lists the arguments a tool's schema requires.
func toolRequiredArgs(tool Tool) []string {
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(tool.ModelParameters(), &schema); err != nil {
		return nil
	}
	return schema.Required
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// renderPath substitutes the path's references, each escaped into one path
// segment, and returns the escaped path. A value that would not be a segment
// at all (empty, "." or "..") or is not a single value (an object, a list) is
// refused before anything is sent; the refusal names the reference, never the
// value.
func renderPath(template string, args, slots map[string]any) (string, error) {
	var refusal error
	out := templateReference.ReplaceAllStringFunc(template, func(ref string) string {
		m := templateReference.FindStringSubmatch(ref)
		value := resolveReference(m[1], m[2], args, slots)
		text, problem := pathValue(value)
		if problem != "" && refusal == nil {
			refusal = fmt.Errorf("the value for {%s.%s} in the path %s", m[1], m[2], problem)
		}
		return url.PathEscape(text)
	})
	if refusal != nil {
		return "", refusal
	}
	return out, nil
}

func pathValue(value any) (string, string) {
	switch value.(type) {
	case map[string]any, []any:
		return "", "is an object or a list, not a single value"
	}
	text := asString(value)
	switch text {
	case "":
		return "", "is empty"
	case ".", "..":
		return "", "is \".\" or \"..\", which is not a path segment"
	}
	return text, ""
}

// renderQuery renders the query parameters. A parameter whose template
// references an argument the model did not pass is left out; a value that is
// an object or a list is refused.
func renderQuery(query map[string]string, args, slots map[string]any) (url.Values, error) {
	values := url.Values{}
	for _, param := range sortedKeys(query) {
		template := query[param]
		isAbsent := false
		for _, m := range templateReference.FindAllStringSubmatch(template, -1) {
			value := resolveReference(m[1], m[2], args, slots)
			if m[1] == "args" && value == nil {
				isAbsent = true
			}
			switch value.(type) {
			case map[string]any, []any:
				return nil, fmt.Errorf("the value for {%s.%s} in query parameter %s is an object or a list, not a single value", m[1], m[2], param)
			}
		}
		if isAbsent {
			continue
		}
		values.Set(param, asString(renderString(template, args, slots)))
	}
	return values, nil
}

// buildURL puts the base, the rendered path and the query together as URL
// fields. The base's scheme, user and host are taken as they are (only the
// deployment's own base may carry a user); the path is appended to the base's
// own path.
func buildURL(base *url.URL, escapedPath string, query url.Values) (*url.URL, error) {
	u := *base
	u.Fragment, u.RawFragment = "", ""
	raw := strings.TrimSuffix(base.EscapedPath(), "/") + escapedPath
	decoded, err := url.PathUnescape(raw)
	if err != nil {
		return nil, fmt.Errorf("the path is not a valid escaped URL path")
	}
	u.Path, u.RawPath = decoded, raw
	u.RawQuery = query.Encode()
	u.ForceQuery = false
	return &u, nil
}
