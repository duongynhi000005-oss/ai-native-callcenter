// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

const (
	seedFlows     = "../seed/flows"
	seedScenarios = "../seed/scenarios"
)

// Every seed flow has scenarios, every scenario passes on the real engine and
// the real HTTP tools, and together they reach every phase of the flow. A new
// phase therefore arrives with a scenario that walks it.
func TestSeedFlowsPassTheirScenarios(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(seedFlows, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no seed flows found: %v", err)
	}
	backend, stop := StartMockBackend()
	defer stop()

	for _, flowFile := range files {
		stem := strings.TrimSuffix(filepath.Base(flowFile), ".json")
		t.Run(stem, func(t *testing.T) {
			data, err := os.ReadFile(flowFile)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := flow.Load(data)
			if err != nil {
				t.Fatal(err)
			}
			scenarios, err := LoadScenarios(filepath.Join(seedScenarios, stem+".json"))
			if err != nil {
				t.Fatalf("every seed flow needs a scenario file: %v", err)
			}

			reports := Run(context.Background(), spec, backend, scenarios.Scenarios)
			for _, r := range reports {
				if !r.Passed() {
					t.Errorf("scenario %q: %s", r.Scenario, r.Failure)
				}
			}
			coverage := Measure(spec, reports)
			if len(coverage.UnreachedPhases) > 0 {
				t.Errorf("no scenario reaches phase(s): %s", strings.Join(coverage.UnreachedPhases, ", "))
			}
			if len(coverage.UnwalkedTransitions) > 0 {
				t.Logf("transitions no scenario walks: %s", strings.Join(coverage.UnwalkedTransitions, "; "))
			}
		})
	}
}

// A scenario that passes a broken flow is worth nothing, so the breakages the
// simulator exists for are made on purpose and must be caught, with a message
// that names the cause.
func TestBrokenFlowsAreCaught(t *testing.T) {
	backend, stop := StartMockBackend()
	defer stop()

	tests := []struct {
		name   string
		break_ func(doc map[string]any)
		// wantScenario is the scenario that must fail; wantFailure a fragment
		// of its message.
		wantScenario string
		wantFailure  string
	}{
		{
			name: "a transition to the wrong phase",
			break_: func(doc map[string]any) {
				rules := nodeOf(doc, "greet")["transitions"].([]any)
				rules[0].(map[string]any)["target"] = "promised"
			},
			wantScenario: "caller promises a date and hangs up",
			wantFailure:  `the call is in phase "promised", want "remind"`,
		},
		{
			name: "a tool missing from a phase's allowlist",
			break_: func(doc map[string]any) {
				nodeOf(doc, "remind")["tools"] = []any{"dispute_record"}
			},
			wantScenario: "caller promises a date and hangs up",
			wantFailure:  `the tool "promise_record" is not allowed in phase "remind"`,
		},
		{
			name: "a transition that no longer fires",
			break_: func(doc map[string]any) {
				rules := nodeOf(doc, "remind")["transitions"].([]any)
				rules[0].(map[string]any)["condition"] = map[string]any{"slot": "result.recorded", "op": "EQ", "value": "yes"}
			},
			wantScenario: "caller promises a date and hangs up",
			wantFailure:  `the call is in phase "remind", want "promised"`,
		},
		{
			name: "a slot the instruction no longer renders",
			break_: func(doc map[string]any) {
				node := nodeOf(doc, "promised")
				node["instruction"] = map[string]any{"en": "Confirm the date and hang up.", "zh": "确认日期后挂断。"}
			},
			wantScenario: "caller promises a date and hangs up",
			wantFailure:  `does not contain "this Friday"`,
		},
		{
			name: "a closing target that is not reached on silence",
			break_: func(doc map[string]any) {
				delete(doc["global"].(map[string]any), "closingTarget")
			},
			wantScenario: "three silences close the call",
			wantFailure:  `the call is in phase "greet", want "finish"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, scenarios := brokenEarlyCollections(t, tt.break_)
			reports := Run(context.Background(), spec, backend, scenarios.Scenarios)

			var found bool
			for _, r := range reports {
				if r.Scenario != tt.wantScenario {
					continue
				}
				found = true
				if r.Passed() {
					t.Fatalf("scenario %q passed on a broken flow", r.Scenario)
				}
				if !strings.Contains(r.Failure, tt.wantFailure) {
					t.Errorf("failure is\n  %s\nwant it to contain\n  %s", r.Failure, tt.wantFailure)
				}
			}
			if !found {
				t.Fatalf("no scenario called %q", tt.wantScenario)
			}
		})
	}
}

func nodeOf(doc map[string]any, id string) map[string]any {
	return doc["nodes"].(map[string]any)[id].(map[string]any)
}

func brokenEarlyCollections(t *testing.T, mutate func(map[string]any)) (*flow.Spec, *File) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(seedFlows, "early_collections.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	broken, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := flow.Load(broken)
	if err != nil {
		t.Fatalf("the broken flow no longer loads, so the load check, not the simulator, would catch it: %v", err)
	}
	scenarios, err := LoadScenarios(filepath.Join(seedScenarios, "early_collections.json"))
	if err != nil {
		t.Fatal(err)
	}
	return spec, scenarios
}

func TestScenarioFileIsStrict(t *testing.T) {
	tests := []struct {
		name, file, want string
	}{
		{"unknown field", `{"scenarios":[{"name":"a","steps":[],"expec":{}}]}`, "unknown field"},
		{"no scenarios", `{"scenarios":[]}`, "no scenarios"},
		{"unnamed", `{"scenarios":[{"steps":[]}]}`, "has no name"},
		{"duplicate names", `{"scenarios":[{"name":"a"},{"name":"a"}]}`, "two scenarios are called"},
		{"two events in a step", `{"scenarios":[{"name":"a","steps":[{"noInput":true,"call":"x"}]}]}`, "exactly one of"},
		{"no event in a step", `{"scenarios":[{"name":"a","steps":[{"expect":{"phase":"x"}}]}]}`, "exactly one of"},
		{"bad botTurn", `{"scenarios":[{"name":"a","steps":[{"botTurn":"loud"}]}]}`, "botTurn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseScenarios([]byte(tt.file))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestUnknownToolIsNamed(t *testing.T) {
	backend, stop := StartMockBackend()
	defer stop()
	spec, scenarios := brokenEarlyCollections(t, func(map[string]any) {})
	file, err := ParseScenarios([]byte(`{"scenarios":[{"name":"typo","steps":[{"call":"bill_lokup"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = scenarios
	r := Run(context.Background(), spec, backend, file.Scenarios)[0]
	if r.Passed() || !strings.Contains(r.Failure, `the flow has no tool "bill_lokup"`) {
		t.Fatalf("failure = %q", r.Failure)
	}
}

// A batch is judged in the phase the response was made in and the last move
// wins (02 §6); the simulator drives it through the same engine calls.
func TestBatchLastMoveWins(t *testing.T) {
	backend, stop := StartMockBackend()
	defer stop()
	spec, _ := brokenEarlyCollections(t, func(map[string]any) {})
	file, err := ParseScenarios([]byte(`{"scenarios":[{"name":"batch","caller":"92223333","steps":[
		{"call":"bill_lookup","expect":{"phase":"remind"}},
		{"batch":[{"call":"dispute_record","args":{"reason":"x"}},{"call":"promise_record","args":{"pay_date":"Friday"}}],
		 "expect":{"phase":"promised"}}
	]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := Run(context.Background(), spec, backend, file.Scenarios)[0]; !r.Passed() {
		t.Fatal(r.Failure)
	}
}

func TestWriteReportsFailuresAndCoverage(t *testing.T) {
	backend, stop := StartMockBackend()
	defer stop()
	spec, scenarios := brokenEarlyCollections(t, func(doc map[string]any) {
		nodeOf(doc, "remind")["tools"] = []any{"dispute_record"}
	})
	reports := Run(context.Background(), spec, backend, scenarios.Scenarios)
	coverage := Measure(spec, reports)

	var out bytes.Buffer
	if failed := Write(&out, spec.ID, reports, coverage, false); failed == 0 {
		t.Fatal("no failure reported")
	}
	for _, want := range []string{"FAIL  caller promises a date and hangs up", "failure: step 4", "coverage:", "unreached phases:"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}
