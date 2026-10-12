// SPDX-License-Identifier: Apache-2.0

package flowsim

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rasonyang/ai-native-callcenter/internal/flow"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveLiveRefusesWithoutTheSwitchOrAKey(t *testing.T) {
	if _, err := ResolveLive(env(map[string]string{"ALIYUN_API_KEY": "k"}), "", "", ""); err == nil || !strings.Contains(err.Error(), EnvLive) {
		t.Errorf("without %s: err = %v", EnvLive, err)
	}
	if _, err := ResolveLive(env(map[string]string{EnvLive: "1"}), "", "", ""); err == nil || !strings.Contains(err.Error(), "ALIYUN_API_KEY") {
		t.Errorf("without a key: err = %v", err)
	}
	if _, err := ResolveLive(env(map[string]string{EnvLive: "true", "ALIYUN_API_KEY": "k"}), "", "", ""); err == nil {
		t.Error("only 1 switches it on")
	}
}

func TestResolveLiveEndpointAndModels(t *testing.T) {
	base := map[string]string{EnvLive: "1", "ALIYUN_API_KEY": "k"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	tests := []struct {
		name     string
		env      map[string]string
		flag     string
		wantBase string
	}{
		{"qwen profile host", base, "", "https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"provider endpoint on dashscope", with(map[string]string{"AICC_PROVIDER_ENDPOINT": "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime"}), "",
			"https://dashscope-intl.aliyuncs.com/compatible-mode/v1"},
		{"provider endpoint elsewhere is ignored", with(map[string]string{"AICC_PROVIDER_ENDPOINT": "wss://api.openai.com/v1/realtime"}), "",
			"https://dashscope.aliyuncs.com/compatible-mode/v1"},
		{"env override", with(map[string]string{EnvEndpoint: "https://example.test/x/v1"}), "", "https://example.test/x/v1"},
		{"flag beats env", with(map[string]string{EnvEndpoint: "https://example.test/x/v1"}), "http://127.0.0.1:9/v1", "http://127.0.0.1:9/v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveLive(env(tt.env), tt.flag, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if got.BaseURL != tt.wantBase {
				t.Errorf("base = %q, want %q", got.BaseURL, tt.wantBase)
			}
			if got.BotModel != DefaultBotModel || got.CallerModel != DefaultCallerModel || got.APIKey != "k" {
				t.Errorf("defaults = %+v", got)
			}
		})
	}

	got, err := ResolveLive(env(with(map[string]string{EnvBotModel: "bot-env", EnvCallerModel: "caller-env"})), "", "bot-flag", "")
	if err != nil || got.BotModel != "bot-flag" || got.CallerModel != "caller-env" {
		t.Errorf("models = %+v, %v", got, err)
	}
}

// Every seed flow has a tier 2 scenario, so a sweep covers all of them.
func TestEverySeedFlowHasAModelScenario(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(seedScenarios, "*.json"))
	if len(files) == 0 {
		t.Fatal("no scenario files")
	}
	for _, f := range files {
		file, err := LoadScenarios(f)
		if err != nil {
			t.Fatal(err)
		}
		var n int
		for _, s := range file.Scenarios {
			if s.CallerPersona != nil {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s has no scenario with a callerPersona", filepath.Base(f))
		}
	}
}

// TestLiveSeedScenarios is the opt-in real run, like the live provider tests:
// it spends money on the Qwen chat API and runs only with AICC_LIVE_FLOWSIM=1
// and ALIYUN_API_KEY, and needs -count=1 so Go does not serve a cached pass.
//
//	AICC_LIVE_FLOWSIM=1 go test -count=1 -run Live -v ./internal/flowsim/
func TestLiveSeedScenarios(t *testing.T) {
	if os.Getenv(EnvLive) != "1" {
		t.Skipf("set %s=1 to run model-driven scenarios (spends money)", EnvLive)
	}
	live, err := ResolveLive(os.Getenv, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	backend := MockBackends()
	budget, cfg := live.Chats(300, 1_000_000)
	cfg.Runs, cfg.MaxCallerTurns = 1, DefaultCallerTurns

	files, _ := filepath.Glob(filepath.Join(seedFlows, "*.json"))
	for _, flowFile := range files {
		stem := strings.TrimSuffix(filepath.Base(flowFile), ".json")
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
			t.Fatal(err)
		}
		for _, r := range RunModel(context.Background(), spec, backend, cfg, scenarios.Scenarios) {
			var out strings.Builder
			WriteModel(&out, stem, []ModelReport{r}, false)
			t.Log("\n" + out.String())
			if !r.Succeeded() {
				t.Errorf("%s / %s: %d of %d runs passed", stem, r.Scenario, r.Passed, len(r.Runs))
			}
		}
	}
	calls, tokens := budget.Usage()
	t.Logf("spent %d model requests, %d tokens", calls, tokens)
}
