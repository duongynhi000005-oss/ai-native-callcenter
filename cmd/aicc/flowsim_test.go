// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFlowSimRunsASeedFlow(t *testing.T) {
	err := runFlowSim([]string{
		"-flow", "../../internal/seed/flows/early_collections.json",
		"-scenario", "../../internal/seed/scenarios/early_collections.json",
	})
	if err != nil {
		t.Fatalf("the seed scenarios must pass: %v", err)
	}
}

func TestFlowSimFailsWhenAScenarioFails(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "s.json")
	body := `{"scenarios":[{"name":"wrong","caller":"92223333","steps":[{"call":"bill_lookup","expect":{"phase":"promised"}}]}]}`
	if err := os.WriteFile(scenario, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runFlowSim([]string{"-flow", "../../internal/seed/flows/early_collections.json", "-scenario", scenario})
	if err == nil || !strings.Contains(err.Error(), "1 of 1 scenarios failed") {
		t.Fatalf("error = %v, want a failed-scenarios error", err)
	}
}
