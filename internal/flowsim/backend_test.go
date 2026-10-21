// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

// Against the mock, a tool with its own baseUrl and secret headers still
// calls the mock, with a placeholder for each secret, so a variable unset on
// the machine running the simulator does not fail the scenario.
func TestTheMockBackendNeedsNoRealSecretOrHost(t *testing.T) {
	t.Parallel()
	spec, err := flow.Load([]byte(`{
	  "id": "kb", "specVersion": "v2", "initialNode": "main",
	  "global": {"persona": "You answer questions."},
	  "nodes": {"main": {"instruction": "Help.", "tools": ["*"]}},
	  "tools": {"kb_search": {
	    "description": "Search.",
	    "parameters": {"type": "object", "properties": {"query": {"type": "string"}}, "required": ["query"]},
	    "http": {
	      "baseUrl": "https://kb.example.com/v1",
	      "path": "/api/kb/search",
	      "headers": {"Authorization": "Bearer {secrets.KB_KEY}", "X-Api-Key": "{secrets.UNSET_ONE}"},
	      "body": {"query": "{args.query}"},
	      "result": {"found": "data.found"}
	    }
	  }}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	backend, release := MockBackends()()
	defer release()
	result, err := backend.Call(context.Background(), nil, spec.Tools["kb_search"], map[string]any{"query": "fees"}, nil)
	if err != nil {
		t.Fatalf("the mock call failed: %v", err)
	}
	if !result.IsOK {
		t.Errorf("result = %+v, want the mock's answer", result)
	}
	if v, ok := PlaceholderSecrets(flow.SecretEnv("ANY")); !ok || v != PlaceholderSecret {
		t.Errorf("placeholder lookup = %q, %v", v, ok)
	}
}
