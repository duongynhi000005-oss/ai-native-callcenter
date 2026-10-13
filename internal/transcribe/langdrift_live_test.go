// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live language-drift harness (issue #127). It streams the synthetic set of
// testdata/langdrift through the real dashscope client at real time, under
// each candidate configuration, and records what came back. Real money.
//
//	python3 internal/transcribe/testdata/langdrift/generate.py
//	AICC_LIVE_PROVIDER_TEST=1 AICC_LANGDRIFT_OUT=/some/dir ALIYUN_API_KEY=... \
//	  AICC_TRANSCRIBE_ENDPOINT=wss://<workspace>.cn-beijing.maas.aliyuncs.com/api-ws/v1/inference \
//	  go test -count=1 -timeout 60m -run TestLiveLanguageDrift ./internal/transcribe/
//
// AICC_LANGDRIFT_LIMIT=n runs only the first n utterances of each language.
// Each configuration writes results-<name>.json in AICC_LANGDRIFT_OUT and the
// markdown summary of every result file present is rewritten after each one.

const (
	ldChunkMs     = 100
	ldLeadSilence = 300 * time.Millisecond
	// ldTailSilence lets server-side endpointing close the last sentence
	// while the stream is still live, as it would on a call.
	ldTailSilence = 1200 * time.Millisecond
	ldConcurrency = 8
)

type ldCase struct{ ID, Lang, Category, Say, Ref string }

func ldLoad(t *testing.T, lang string) []ldCase {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "langdrift", "scripts_"+lang+".tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []ldCase
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) != 4 {
			t.Fatalf("bad script line %q", sc.Text())
		}
		out = append(out, ldCase{ID: p[0], Lang: lang, Category: p[1], Say: p[2], Ref: p[3]})
	}
	return out
}

type ldConfig struct {
	Name   string
	Model  string
	Prompt func(lang string) string // task-input context, via the harness hook; none if nil
	Lang   bool                     // pass the call language to Start (language_hints)
}

var ldContext = map[string]string{
	"zh": "这是一通中文客服电话，客户说普通话，数字请用阿拉伯数字。常见词：NovaNet、Wi-Fi、5G、4G、PIN码、App、订单号。",
	"en": "This is an English customer service call. Write numbers as digits. Common terms: NovaNet, Wi-Fi, 5G, 4G, PIN, app, order number.",
}

const (
	ldMessage   = "qwen-audio-3.1-asr-flash-message"
	ldStreaming = "qwen-audio-3.1-asr-flash-streaming"
)

var ldConfigs = []ldConfig{
	// The configuration before #127: no language reaches the service.
	{Name: "message-bare", Model: ldMessage},
	{Name: "message-context", Model: ldMessage, Prompt: func(l string) string { return ldContext[l] }},
	{Name: "streaming-hints", Model: ldStreaming, Lang: true},
	// The default after #127, through the real Start path.
	{Name: "message-hints", Model: ldMessage, Lang: true},
}

type ldFinal struct {
	Text    string `json:"text"`
	AtMs    int    `json:"atMs"`
	IsWrong bool   `json:"wrongLanguage"`
}

type ldResult struct {
	Config        string    `json:"config"`
	ID            string    `json:"id"`
	Lang          string    `json:"lang"`
	Category      string    `json:"category"`
	Variant       string    `json:"variant"`
	Ref           string    `json:"ref"`
	Hyp           string    `json:"hyp"`
	Finals        []ldFinal `json:"finals"`
	Partials      int       `json:"partials"`
	WrongPartials int       `json:"wrongPartials"`
	WrongPartial  []string  `json:"wrongPartialSamples,omitempty"`
	WrongFinals   int       `json:"wrongFinals"`
	Rate          float64   `json:"errorRate"` // CER for zh, WER for en
	AudioMs       int       `json:"audioMs"`
	LastFinalLag  int       `json:"lastFinalLagMs"` // after the end of speech; -1 if no final
	Err           string    `json:"error,omitempty"`
}

func ldPCM(t *testing.T, lang, variant, id string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "langdrift", "cache", lang, variant, id+".pcm"))
	if err != nil {
		t.Skipf("run testdata/langdrift/generate.py first: %v", err)
	}
	return b
}

// ldStream sends pcm in real time through a new session and returns what the
// recogniser said. A hang-up is audio ending, then Close.
func ldStream(ctx context.Context, cfg ldConfig, endpoint, key string, c ldCase, variant string, pcm []byte) ldResult {
	res := ldResult{Config: cfg.Name, ID: c.ID, Lang: c.Lang, Category: c.Category, Variant: variant,
		Ref: c.Ref, LastFinalLag: -1}
	p, err := ProfileFor("qwen", Override{Endpoint: endpoint, Model: cfg.Model})
	if err != nil {
		res.Err = err.Error()
		return res
	}
	s := newDashscope(p, key, nopLogger{})
	tcfg := Config{}
	if cfg.Lang {
		tcfg.Language = c.Lang
	}
	if cfg.Prompt != nil {
		s.extraInput = map[string]any{"context": []map[string]any{{
			"role":    "user",
			"content": []map[string]any{{"type": "input_text", "text": cfg.Prompt(c.Lang)}},
		}}}
	}
	if err := s.Start(ctx, tcfg); err != nil {
		res.Err = err.Error()
		return res
	}
	begin := time.Now()
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range s.Events() {
			at := int(time.Since(begin) / time.Millisecond)
			mu.Lock()
			switch ev.Type {
			case EventPartial:
				res.Partials++
				if wrongLanguage(c.Lang, ev.Text) {
					res.WrongPartials++
					if len(res.WrongPartial) < 3 {
						res.WrongPartial = append(res.WrongPartial, ev.Text)
					}
				}
			case EventFinal:
				w := wrongLanguage(c.Lang, ev.Text)
				res.Finals = append(res.Finals, ldFinal{Text: ev.Text, AtMs: at, IsWrong: w})
				if w {
					res.WrongFinals++
				}
			case EventError:
				res.Err = ev.Err.Error()
			}
			mu.Unlock()
		}
	}()

	chunk := 16000 * 2 * ldChunkMs / 1000
	silence := make([]byte, chunk)
	tick := time.NewTicker(ldChunkMs * time.Millisecond)
	defer tick.Stop()
	send := func(b []byte) bool {
		select {
		case <-tick.C:
		case <-ctx.Done():
			return false
		}
		return s.SendAudio(b) == nil
	}
	ok := true
	for i := 0; i < int(ldLeadSilence/time.Millisecond)/ldChunkMs && ok; i++ {
		ok = send(silence)
	}
	speechStart := time.Since(begin)
	for off := 0; off < len(pcm) && ok; off += chunk {
		end := min(off+chunk, len(pcm))
		b := pcm[off:end]
		if len(b) < chunk {
			b = append(append([]byte{}, b...), make([]byte, chunk-len(b))...)
		}
		ok = send(b)
	}
	speechEnd := speechStart + time.Duration(len(pcm)/32)*time.Millisecond
	for i := 0; i < int(ldTailSilence/time.Millisecond)/ldChunkMs && ok; i++ {
		ok = send(silence)
	}
	_ = s.Close(ctx)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}

	mu.Lock()
	defer mu.Unlock()
	res.AudioMs = len(pcm) / 32
	sep := " "
	if c.Lang == "zh" {
		sep = ""
	}
	var texts []string
	for _, f := range res.Finals {
		texts = append(texts, f.Text)
	}
	res.Hyp = strings.Join(texts, sep)
	res.Rate = errorRate(c.Lang, c.Ref, res.Hyp)
	if n := len(res.Finals); n > 0 {
		res.LastFinalLag = res.Finals[n-1].AtMs - int(speechEnd/time.Millisecond)
	}
	return res
}

func ldEnv(t *testing.T) (endpoint, key, out string) {
	t.Helper()
	if os.Getenv("AICC_LIVE_PROVIDER_TEST") == "" {
		t.Skip("set AICC_LIVE_PROVIDER_TEST=1 to run against the live service (real money)")
	}
	endpoint, key = os.Getenv("AICC_TRANSCRIBE_ENDPOINT"), os.Getenv("ALIYUN_API_KEY")
	if endpoint == "" || key == "" {
		t.Skip("AICC_TRANSCRIBE_ENDPOINT and ALIYUN_API_KEY are required")
	}
	out = os.Getenv("AICC_LANGDRIFT_OUT")
	if out == "" {
		out = t.TempDir()
	}
	return
}

func TestLiveLanguageDrift(t *testing.T) {
	endpoint, key, out := ldEnv(t)
	limit := 0
	if v := os.Getenv("AICC_LANGDRIFT_LIMIT"); v != "" {
		fmt.Sscan(v, &limit)
	}
	var cases []ldCase
	for _, lang := range []string{"zh", "en"} {
		cs := ldLoad(t, lang)
		if limit > 0 && len(cs) > limit {
			cs = cs[:limit]
		}
		cases = append(cases, cs...)
	}
	for _, cfg := range ldConfigs {
		t.Run(cfg.Name, func(t *testing.T) {
			type job struct {
				c       ldCase
				variant string
			}
			var jobs []job
			for _, c := range cases {
				for _, v := range []string{"clean", "noise", "crosstalk"} {
					jobs = append(jobs, job{c, v})
				}
			}
			results := make([]ldResult, len(jobs))
			ch := make(chan int)
			var wg sync.WaitGroup
			for w := 0; w < ldConcurrency; w++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range ch {
						j := jobs[i]
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						results[i] = ldStream(ctx, cfg, endpoint, key, j.c, j.variant, ldPCM(t, j.c.Lang, j.variant, j.c.ID))
						cancel()
					}
				}()
			}
			for i := range jobs {
				ch <- i
			}
			close(ch)
			wg.Wait()

			b, _ := json.MarshalIndent(results, "", " ")
			if err := os.WriteFile(filepath.Join(out, "results-"+cfg.Name+".json"), b, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ldWriteSummary(out); err != nil {
				t.Fatal(err)
			}
			errs := 0
			for _, r := range results {
				if r.Err != "" {
					errs++
				}
			}
			t.Logf("%s: %d runs, %d with an error", cfg.Name, len(results), errs)
		})
	}
}

// A caller who hangs up mid-sentence still gets the words so far as a final,
// on the live service (the non-live counterpart pins the client's side).
func TestLiveHangupMidSentenceDeliversFinal(t *testing.T) {
	endpoint, key, _ := ldEnv(t)
	for _, c := range []ldCase{ldLoad(t, "zh")[40], ldLoad(t, "en")[40]} {
		pcm := ldPCM(t, c.Lang, "clean", c.ID)
		pcm = pcm[:len(pcm)/2] // cut in the middle of the sentence, no trailing silence
		cfg := ldConfig{Name: "hangup", Model: ldMessage}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r := ldStreamNoTail(ctx, cfg, endpoint, key, c, pcm)
		cancel()
		if r.Err != "" {
			t.Fatalf("%s: %s", c.ID, r.Err)
		}
		if len(r.Finals) == 0 || strings.TrimSpace(r.Hyp) == "" {
			t.Errorf("%s: the hang-up lost the open sentence (partials=%d)", c.ID, r.Partials)
			continue
		}
		t.Logf("%s: hang-up final %q", c.ID, r.Hyp)
	}
}

// ldStreamNoTail streams pcm and closes the instant it ends.
func ldStreamNoTail(ctx context.Context, cfg ldConfig, endpoint, key string, c ldCase, pcm []byte) ldResult {
	res := ldResult{Config: cfg.Name, ID: c.ID, Lang: c.Lang}
	p, _ := ProfileFor("qwen", Override{Endpoint: endpoint, Model: cfg.Model})
	s := newDashscope(p, key, nopLogger{})
	if err := s.Start(ctx, Config{}); err != nil {
		res.Err = err.Error()
		return res
	}
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range s.Events() {
			mu.Lock()
			switch ev.Type {
			case EventPartial:
				res.Partials++
			case EventFinal:
				res.Finals = append(res.Finals, ldFinal{Text: ev.Text})
			}
			mu.Unlock()
		}
	}()
	chunk := 16000 * 2 * ldChunkMs / 1000
	tick := time.NewTicker(ldChunkMs * time.Millisecond)
	defer tick.Stop()
	for off := 0; off+chunk <= len(pcm); off += chunk {
		<-tick.C
		_ = s.SendAudio(pcm[off : off+chunk])
	}
	_ = s.Close(ctx)
	<-done
	mu.Lock()
	defer mu.Unlock()
	for _, f := range res.Finals {
		res.Hyp += f.Text
	}
	return res
}

type ldAgg struct {
	N, Finals, WrongFinals, Partials, WrongPartials, NoFinal, Errors int
	RateSum                                                          float64
	LagSum, LagN                                                     int
}

func ldWriteSummary(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "results-*.json"))
	sort.Strings(files)
	var sb strings.Builder
	type key struct{ cfg, lang, variant string }
	for _, f := range files {
		var rs []ldResult
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &rs); err != nil {
			return err
		}
		agg := map[key]*ldAgg{}
		cats := map[string]*ldAgg{}
		for _, r := range rs {
			for _, k := range []key{{r.Config, r.Lang, r.Variant}, {r.Config, r.Lang, "all"}} {
				a := agg[k]
				if a == nil {
					a = &ldAgg{}
					agg[k] = a
				}
				a.N++
				a.Finals += len(r.Finals)
				a.WrongFinals += r.WrongFinals
				a.Partials += r.Partials
				a.WrongPartials += r.WrongPartials
				a.RateSum += r.Rate
				if len(r.Finals) == 0 {
					a.NoFinal++
				}
				if r.Err != "" {
					a.Errors++
				}
				if r.LastFinalLag >= 0 {
					a.LagSum += r.LastFinalLag
					a.LagN++
				}
			}
			ck := r.Lang + "/" + r.Category
			if cats[ck] == nil {
				cats[ck] = &ldAgg{}
			}
			cats[ck].N++
			cats[ck].RateSum += r.Rate
			cats[ck].WrongFinals += r.WrongFinals
		}
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "results-"), ".json")
		fmt.Fprintf(&sb, "### %s\n\n| lang | variant | runs | wrong finals / finals | wrong partials / partials | mean rate | no final | errors | last-final lag ms |\n|---|---|---|---|---|---|---|---|---|\n", name)
		for _, lang := range []string{"zh", "en"} {
			for _, v := range []string{"clean", "noise", "crosstalk", "all"} {
				a := agg[key{name, lang, v}]
				if a == nil {
					continue
				}
				lag := "-"
				if a.LagN > 0 {
					lag = fmt.Sprint(a.LagSum / a.LagN)
				}
				fmt.Fprintf(&sb, "| %s | %s | %d | %d / %d | %d / %d | %.3f | %d | %d | %s |\n", lang, v, a.N,
					a.WrongFinals, a.Finals, a.WrongPartials, a.Partials, a.RateSum/float64(a.N), a.NoFinal, a.Errors, lag)
			}
		}
		var ck []string
		for k := range cats {
			ck = append(ck, k)
		}
		sort.Strings(ck)
		sb.WriteString("\nBy category (mean rate / wrong finals):")
		for _, k := range ck {
			a := cats[k]
			fmt.Fprintf(&sb, " %s %.3f/%d;", k, a.RateSum/float64(a.N), a.WrongFinals)
		}
		sb.WriteString("\n\n")
	}
	return os.WriteFile(filepath.Join(dir, "summary.md"), []byte(sb.String()), 0o644)
}
