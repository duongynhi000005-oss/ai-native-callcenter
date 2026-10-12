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

// Tier 2 spends money, so without the switch it refuses before any request.
func TestFlowSimTier2NeedsTheLiveSwitch(t *testing.T) {
	t.Setenv("AICC_LIVE_FLOWSIM", "")
	t.Setenv("ALIYUN_API_KEY", "k")
	err := runFlowSim([]string{
		"-flow", "../../internal/seed/flows/early_collections.json",
		"-scenario", "../../internal/seed/scenarios/early_collections.json",
		"-tier", "2",
	})
	if err == nil || !strings.Contains(err.Error(), "AICC_LIVE_FLOWSIM") {
		t.Fatalf("error = %v, want a refusal naming AICC_LIVE_FLOWSIM", err)
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
