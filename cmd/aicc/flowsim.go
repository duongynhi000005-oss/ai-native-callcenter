// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/rasonyang/ai-native-callcenter/internal/egress"
	"github.com/rasonyang/ai-native-callcenter/internal/flow"
	"github.com/rasonyang/ai-native-callcenter/internal/flowsim"
)

// runFlowSim implements `aicc flowsim`: replay a scenario file against a flow
// file on the real engine and runtime, with the flow's HTTP tools served by the
// in-process mock backend (or the backend named by -backend). It needs neither
// the database nor the switch nor a model, and it exits non-zero when a
// scenario fails.
//
// With -tier 2 a text model plays the bot and another plays the caller
// (scenarios with a callerPersona), judged as a pass rate over -runs runs. That
// spends money on the Qwen chat API and needs AICC_LIVE_FLOWSIM=1 and
// ALIYUN_API_KEY; see flowsim.ResolveLive.
func runFlowSim(args []string) error {
	fs := flag.NewFlagSet("flowsim", flag.ExitOnError)
	flowFile := fs.String("flow", "", "flow JSON file (required)")
	scenarioFile := fs.String("scenario", "", "scenario JSON file (required)")
	only := fs.String("only", "", "run only the scenario with this name")
	backendURL := fs.String("backend", "", "base URL of the backend the flow's tools call (default: the in-process mock backend)")
	verbose := fs.Bool("v", false, "print every step (tier 2: every transcript)")
	tier := fs.Int("tier", 1, "1 replays scripted steps; 2 runs model-driven scenarios (spends money)")
	runs := fs.Int("runs", flowsim.DefaultModelRuns, "tier 2: runs per scenario")
	botModel := fs.String("bot-model", "", "tier 2: bot model (default "+flowsim.DefaultBotModel+", or "+flowsim.EnvBotModel+")")
	callerModel := fs.String("caller-model", "", "tier 2: caller model (default "+flowsim.DefaultCallerModel+", or "+flowsim.EnvCallerModel+")")
	endpoint := fs.String("endpoint", "", "tier 2: chat API base URL (default: derived from the qwen profile, or "+flowsim.EnvEndpoint+")")
	maxTurns := fs.Int("max-turns", flowsim.DefaultCallerTurns, "tier 2: cap on caller turns per run")
	maxCalls := fs.Int("max-calls", 300, "tier 2: cap on model requests for the whole invocation")
	maxTokens := fs.Int("max-tokens", 1_000_000, "tier 2: cap on tokens (prompt + completion) for the whole invocation")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: aicc flowsim -flow <flow.json> -scenario <scenarios.json> [-only name] [-backend url] [-v] [-tier 2 -runs N ...]")
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

	if *tier != 1 && *tier != 2 {
		return fmt.Errorf("-tier is 1 or 2")
	}
	var live flowsim.Live
	if *tier == 2 {
		// Checked before anything else runs: no model request without the
		// owner's switch and a key.
		live, err = flowsim.ResolveLive(os.Getenv, *endpoint, *botModel, *callerModel)
		if err != nil {
			return err
		}
	}

	var backend flowsim.BackendSource
	if *backendURL != "" {
		// A real backend is reached under the same outbound host policy as a
		// call, and its tools' secrets come from the environment as on a call.
		allowed, err := egress.ParseAllowList(os.Getenv("AICC_TOOL_ALLOWED_HOSTS"))
		if err != nil {
			return fmt.Errorf("AICC_TOOL_ALLOWED_HOSTS: %w", err)
		}
		backend = flowsim.SharedBackend(flow.NewBackend(*backendURL, flow.WithAllowedHosts(allowed)))
		if (*tier == 2 && *runs > 1) || (*tier == 1 && len(scenarios) > 1) {
			fmt.Fprintf(os.Stderr, "warning: %s is an external backend the simulator cannot reset; "+
				"state one run writes (a booking, a record) is visible to the next\n", *backendURL)
		}
	} else {
		// A fresh mock for every scenario (tier 1) and every run (tier 2).
		backend = flowsim.MockBackends()
	}

	if *tier == 2 {
		budget, cfg := live.Chats(*maxCalls, *maxTokens)
		cfg.Runs, cfg.MaxCallerTurns, cfg.Log = *runs, *maxTurns, os.Stderr
		fmt.Fprintf(os.Stderr, "tier 2: bot %s, caller %s, %s; at most %d requests, %d tokens\n",
			live.BotModel, live.CallerModel, live.BaseURL, *maxCalls, *maxTokens)
		modelReports := flowsim.RunModel(context.Background(), spec, backend, cfg, scenarios)
		failed := flowsim.WriteModel(os.Stdout, spec.ID, modelReports, *verbose)
		calls, tokens := budget.Usage()
		fmt.Printf("  spent: %d model requests, %d tokens\n", calls, tokens)
		if len(modelReports) == 0 {
			return fmt.Errorf("no scenario in %s has a callerPersona", *scenarioFile)
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d scenarios missed their pass rate", failed, len(modelReports))
		}
		return nil
	}

	reports := flowsim.Run(context.Background(), spec, backend, scenarios)
	coverage := flowsim.Measure(spec, reports)
	if failed := flowsim.Write(os.Stdout, spec.ID, reports, coverage, *verbose); failed > 0 {
		return fmt.Errorf("%d of %d scenarios failed", failed, len(reports))
	}
	return nil
}
