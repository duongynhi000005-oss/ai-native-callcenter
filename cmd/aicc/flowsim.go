// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/flowsim"
)

// runFlowSim implements `aicc flowsim`: replay a scenario file against a flow
// file on the real engine and runtime, with the flow's HTTP tools served by the
// in-process mock backend (or the backend named by -backend). It needs neither
// the database nor the switch nor a model, and it exits non-zero when a
// scenario fails.
func runFlowSim(args []string) error {
	fs := flag.NewFlagSet("flowsim", flag.ExitOnError)
	flowFile := fs.String("flow", "", "flow JSON file (required)")
	scenarioFile := fs.String("scenario", "", "scenario JSON file (required)")
	only := fs.String("only", "", "run only the scenario with this name")
	backendURL := fs.String("backend", "", "base URL of the backend the flow's tools call (default: the in-process mock backend)")
	verbose := fs.Bool("v", false, "print every step")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: aicc flowsim -flow <flow.json> -scenario <scenarios.json> [-only name] [-backend url] [-v]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if *flowFile == "" || *scenarioFile == "" {
		fs.Usage()
		return fmt.Errorf("-flow and -scenario are required")
	}

	data, err := os.ReadFile(*flowFile)
	if err != nil {
		return err
	}
	spec, err := flow.Load(data)
	if err != nil {
		return fmt.Errorf("%s: %w", *flowFile, err)
	}
	file, err := flowsim.LoadScenarios(*scenarioFile)
	if err != nil {
		return err
	}
	scenarios := file.Scenarios
	if *only != "" {
		scenarios = nil
		for _, s := range file.Scenarios {
			if s.Name == *only {
				scenarios = append(scenarios, s)
			}
		}
		if len(scenarios) == 0 {
			return fmt.Errorf("no scenario is called %q", *only)
		}
	}

	var backend *flow.Backend
	if *backendURL != "" {
		backend = flow.NewBackend(*backendURL)
	} else {
		var stop func()
		backend, stop = flowsim.StartMockBackend()
		defer stop()
	}

	reports := flowsim.Run(context.Background(), spec, backend, scenarios)
	coverage := flowsim.Measure(spec, reports)
	if failed := flowsim.Write(os.Stdout, spec.ID, reports, coverage, *verbose); failed > 0 {
		return fmt.Errorf("%d of %d scenarios failed", failed, len(reports))
	}
	return nil
}
