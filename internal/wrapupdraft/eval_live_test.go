// SPDX-License-Identifier: Apache-2.0

package wrapupdraft

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/textllm"
)

// TestEvalLive measures the real job's prompt and validation against a real
// model. It costs money, so it needs AICC_LIVE_PROVIDER_TEST=1, and it needs
// -count=1 or Go serves a cached result:
//
//	AICC_LIVE_PROVIDER_TEST=1 AICC_TEXT_LLM_MODEL=qwen-plus ALIYUN_API_KEY=... \
//	  go test -count=1 -run TestEvalLive -v ./internal/wrapupdraft/
//
// Endpoint, model and key come from AICC_TEXT_LLM_ENDPOINT (default DashScope's
// compatible mode), AICC_TEXT_LLM_MODEL (default qwen-plus) and TEXT_LLM_API_KEY
// (falling back to ALIYUN_API_KEY). It is a measurement: disagreement with the
// expected answers never fails it, only a harness error does.
func TestEvalLive(t *testing.T) {
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") != "1" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run the live evaluation")
	}
	cfg := textllm.Config{
		Endpoint: envOr("AICC_TEXT_LLM_ENDPOINT", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
		Model:    envOr("AICC_TEXT_LLM_MODEL", "qwen-plus"),
		APIKey:   envOr("TEXT_LLM_API_KEY", os.Getenv("ALIYUN_API_KEY")),
	}
	client, err := textllm.New(cfg, nil)
	if err != nil {
		t.Fatalf("harness: %v", err)
	}

	cases := loadEvalCases(t)
	results := make([]evalResult, len(cases))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, c := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runEvalCase(t, client, c)
		}()
	}
	wg.Wait()

	var report strings.Builder
	fmt.Fprintf(&report, "\nmodel %s @ %s, %d cases\n", cfg.Model, cfg.Endpoint, len(cases))
	fmt.Fprintf(&report, "%-42s %-4s %-17s %-19s %-6s %-7s %-6s %s\n", "case", "lang", "type", "expected", "got", "agree", "facts", "notes")
	var agree, valid, factsHit, factsAll, forbidden, errs, invalid int
	byType := map[string][2]int{}
	for i, r := range results {
		c := cases[i]
		status := "AGREE"
		switch {
		case r.err != nil:
			status = "ERROR"
			errs++
		case r.invalid != "":
			status = "INVALID"
			invalid++
		case !r.agree:
			status = "DIFF"
		}
		if r.err == nil && r.invalid == "" {
			valid++
			factsHit += r.hits
			factsAll += len(c.Must)
			forbidden += len(r.forbidden)
			if r.agree {
				agree++
			}
			s := byType[c.Type]
			s[1]++
			if r.agree {
				s[0]++
			}
			byType[c.Type] = s
		}
		extra := ""
		switch {
		case r.err != nil:
			extra = "error: " + r.err.Error()
		case r.invalid != "":
			extra = "invalid: " + r.invalid
		default:
			if len(r.missed) > 0 {
				extra += fmt.Sprintf("missed=%v ", r.missed)
			}
			if len(r.forbidden) > 0 {
				extra += fmt.Sprintf("FORBIDDEN=%v ", r.forbidden)
			}
		}
		fmt.Fprintf(&report, "%-42s %-4s %-17s %-19s %-6s %-7s %d/%-4d %s\n", c.ID, c.Lang, c.Type,
			codeName(c.ExpectCode), codeName(r.got), status, r.hits, len(c.Must), extra)
		if r.note != "" {
			fmt.Fprintf(&report, "    note: %s\n", strings.ReplaceAll(r.note, "\n", " "))
		}
	}
	fmt.Fprintf(&report, "\ndisposition agreement: %d/%d valid answers (%.0f%%)\n", agree, valid, pct(agree, valid))
	fmt.Fprintf(&report, "checklist facts present: %d/%d (%.0f%%)\n", factsHit, factsAll, pct(factsHit, factsAll))
	fmt.Fprintf(&report, "forbidden facts present: %d\n", forbidden)
	fmt.Fprintf(&report, "invalid answers: %d, harness errors: %d\n", invalid, errs)
	for _, ty := range sortedKeys(byType) {
		s := byType[ty]
		fmt.Fprintf(&report, "  %-17s %d/%d agree\n", ty, s[0], s[1])
	}
	t.Log(report.String())
	if errs > 0 {
		t.Errorf("%d cases hit a harness error (see the table)", errs)
	}
}

type evalResult struct {
	got       *string
	note      string
	agree     bool
	hits      int
	missed    [][]string
	forbidden []string
	invalid   string // parseAnswer's rejection, which would mean no draft in production
	err       error  // render, the transport or the model service
}

func runEvalCase(t *testing.T, client textllm.Client, c evalCase) evalResult {
	lines, agent := c.transcript(t)
	prompt, ok := render(split(lines, agent), seededDispositions, DefaultBudgetChars)
	if !ok {
		return evalResult{err: fmt.Errorf("the transcript does not render")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout*3)
	defer cancel()
	var answer string
	var err error
	for attempt := 0; attempt < 2; attempt++ { // one retry for a transient service error
		answer, err = client.Complete(ctx, textllm.Request{System: systemPrompt, User: prompt, JSON: true})
		if err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return evalResult{err: err}
	}
	code, note, err := parseAnswer(answer, seededDispositions)
	if err != nil {
		return evalResult{invalid: err.Error(), note: answer}
	}
	r := evalResult{got: code, note: note}
	r.agree = sameCode(code, c.ExpectCode) || (c.AcceptNull && code == nil) ||
		(c.AcceptCode != "" && code != nil && *code == c.AcceptCode)
	low := strings.ToLower(note)
	for _, group := range c.Must {
		if containsAny(low, group) {
			r.hits++
		} else {
			r.missed = append(r.missed, group)
		}
	}
	for _, group := range c.MustNot {
		for _, alt := range group {
			if strings.Contains(low, strings.ToLower(alt)) {
				r.forbidden = append(r.forbidden, alt)
			}
		}
	}
	return r
}

func containsAny(lowText string, alts []string) bool {
	for _, a := range alts {
		if strings.Contains(lowText, strings.ToLower(a)) {
			return true
		}
	}
	return false
}

func sameCode(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func codeName(c *string) string {
	if c == nil {
		return "null"
	}
	return *c
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func sortedKeys(m map[string][2]int) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	for i := range ks {
		for j := i + 1; j < len(ks); j++ {
			if ks[j] < ks[i] {
				ks[i], ks[j] = ks[j], ks[i]
			}
		}
	}
	return ks
}
