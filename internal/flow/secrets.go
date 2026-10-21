// SPDX-License-Identifier: Apache-2.0

package flow

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A tool's credential is never written in the flow. A flow is readable by
// everyone who holds config:read, and its revisions are kept for good, so a
// token typed into a header would be a leak with no way to take it back. The
// flow names the credential instead, as {secrets.NAME}, and the value comes
// from the environment variable AICC_TOOL_SECRET_<NAME> when the call is made
// (issue #286). The value never enters the stored flow, an API response, a
// log line, an error or what the model is told about the call.

// SecretEnvPrefix is the prefix of the environment variable a secret
// reference resolves from.
const SecretEnvPrefix = "AICC_TOOL_SECRET_"

var (
	// secretReference is a well-formed reference: {secrets.KB_KEY}.
	secretReference = regexp.MustCompile(`\{secrets\.([A-Z0-9_]+)\}`)
	// anySecretReference is anything written like one, well-formed or not, so
	// that {secrets.kb_key} is reported rather than sent as text.
	anySecretReference = regexp.MustCompile(`\{secrets\.[^{}]*\}?`)
)

// SecretEnv is the variable a secret name resolves from.
func SecretEnv(name string) string { return SecretEnvPrefix + name }

// secretNames lists the secret names a header value references, in order.
func secretNames(value string) []string {
	var names []string
	for _, m := range secretReference.FindAllStringSubmatch(value, -1) {
		names = append(names, m[1])
	}
	return names
}

// hasMalformedSecret reports text written like a secret reference that is not
// a well-formed one.
func hasMalformedSecret(value string) bool {
	return len(anySecretReference.FindAllString(value, -1)) != len(secretReference.FindAllString(value, -1))
}

// mentionsSecret reports any attempt at a secret reference, which outside a
// header value is refused.
func mentionsSecret(value string) bool {
	return strings.Contains(value, "{secrets.")
}

// credentialWords mark a header that carries a credential. Such a header must
// take its value from a secret: a literal there is a credential in plain text.
var credentialWords = []string{"auth", "cookie", "token", "key", "secret", "password"}

// isCredentialHeader reports a header whose name says it carries a credential:
// Authorization, Proxy-Authorization, Cookie, and anything with token, key,
// secret or password in its name (X-Api-Key, X-Auth-Token).
func isCredentialHeader(name string) bool {
	lower := strings.ToLower(name)
	return slices.ContainsFunc(credentialWords, func(word string) bool {
		return strings.Contains(lower, word)
	})
}

// resolveSecrets renders a header value. It returns an error naming the header
// and the variable when a variable is unset or its value cannot be sent in a
// header, and never the value itself.
func resolveSecrets(header, value string, lookup func(string) (string, bool)) (string, error) {
	var failure error
	out := secretReference.ReplaceAllStringFunc(value, func(ref string) string {
		name := secretReference.FindStringSubmatch(ref)[1]
		secret, ok := lookup(SecretEnv(name))
		switch {
		case !ok || secret == "":
			if failure == nil {
				failure = fmt.Errorf("header %s needs %s, which is not set", header, SecretEnv(name))
			}
			return ""
		case strings.ContainsAny(secret, "\r\n\x00"):
			if failure == nil {
				failure = fmt.Errorf("header %s: the value of %s contains a line break or a NUL and cannot be sent", header, SecretEnv(name))
			}
			return ""
		}
		return secret
	})
	if failure != nil {
		return "", failure
	}
	return out, nil
}

// MissingSecrets lists every secret reference in the flow whose variable is
// unset (empty counts as unset), one problem per reference, sorted.
func MissingSecrets(spec *Spec, lookup func(string) (string, bool)) []string {
	var problems []string
	for toolName, tool := range spec.Tools {
		if tool.HTTP == nil {
			continue
		}
		for header, value := range tool.HTTP.Headers {
			for _, name := range secretNames(value) {
				if v, ok := lookup(SecretEnv(name)); !ok || v == "" {
					problems = append(problems, fmt.Sprintf(
						"tool %q header %s references {secrets.%s}, but %s is not set on this deployment",
						toolName, header, name, SecretEnv(name)))
				}
			}
		}
	}
	slices.Sort(problems)
	return slices.Compact(problems)
}

// RequireToolSecrets refuses a flow that references a secret this deployment
// does not have. It is a publish rule, not a load check: a draft may be
// written before the operator sets the variable, and the store only warns
// about it then (Store.FlowDraftChecks), while publishing is what a caller
// would hear fail.
func RequireToolSecrets(lookup func(string) (string, bool)) Rule {
	return func(spec *Spec) error {
		problems := MissingSecrets(spec, lookup)
		if len(problems) == 0 {
			return nil
		}
		return &ValidationError{FlowID: spec.ID, Problems: problems}
	}
}
