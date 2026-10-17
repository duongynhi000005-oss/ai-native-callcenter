// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// The application's own counters, read off its unauthenticated ops listener
// (GET /metrics on AICC_METRICS_ADDR, :9090 in the compose stack), the same
// listener the driver already polls for /readyz. The names are the contract
// of internal/obs/callmetrics.go.
const (
	// MetricProviderSessions counts the provider sessions the application
	// opened, by provider (obs.RecordProviderSessionStarted).
	MetricProviderSessions = "aicc_provider_sessions_started_total"
	// MetricBotSlots is the bot capacity in use, by state: IN_USE and
	// RESERVED (obs.BotSlotsChanged).
	MetricBotSlots = "aicc_bot_slots"
	// MetricDIDOutboundInUse is the outbound lines held on a DID, by did
	// (obs.DIDOutboundInUseChanged).
	MetricDIDOutboundInUse = "aicc_did_outbound_in_use"
	// MetricCampaignAttempts counts finished campaign attempts by outcome; a
	// revision of a finished attempt is not counted again
	// (obs.RecordCampaignAttempt).
	MetricCampaignAttempts = "aicc_campaign_attempts_total"
)

// Sample is one series of the exposition: its labels (the exporter's
// otel_scope_* labels dropped) and its value.
type Sample struct {
	Labels map[string]string `json:"labels"`
	Value  float64           `json:"value"`
}

// Metrics is a scrape of the exposition, by metric name. Only the aicc_
// families are kept.
type Metrics map[string][]Sample

// Find is every series of name whose labels include match.
func (m Metrics) Find(name string, match map[string]string) []Sample {
	var out []Sample
	for _, s := range m[name] {
		ok := true
		for k, v := range match {
			if s.Labels[k] != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// Sum adds up the series of name whose labels include match, and says how
// many there were.
func (m Metrics) Sum(name string, match map[string]string) (float64, int) {
	var sum float64
	found := m.Find(name, match)
	for _, s := range found {
		sum += s.Value
	}
	return sum, len(found)
}

// ByLabel is the series of name keyed by the value of one label.
func (m Metrics) ByLabel(name, label string) map[string]float64 {
	out := map[string]float64{}
	for _, s := range m[name] {
		out[s.Labels[label]] += s.Value
	}
	return out
}

// ParseMetrics reads the Prometheus text exposition. Comment lines are
// skipped, and so is every family that is not the application's (aicc_).
func ParseMetrics(text string) (Metrics, error) {
	out := Metrics{}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "aicc_") {
			continue
		}
		name, labels, rest, err := splitSeries(line)
		if err != nil {
			return nil, fmt.Errorf("metrics line %d: %w", n+1, err)
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			return nil, fmt.Errorf("metrics line %d: no value", n+1)
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return nil, fmt.Errorf("metrics line %d: value %q: %w", n+1, fields[0], err)
		}
		for k := range labels {
			if strings.HasPrefix(k, "otel_scope_") {
				delete(labels, k)
			}
		}
		out[name] = append(out[name], Sample{Labels: labels, Value: v})
	}
	for _, list := range out {
		sort.SliceStable(list, func(i, j int) bool { return labelKey(list[i].Labels) < labelKey(list[j].Labels) })
	}
	return out, nil
}

// splitSeries cuts `name{a="x",b="y"} rest` into its parts. A label value may
// hold an escaped quote, backslash or newline.
func splitSeries(line string) (name string, labels map[string]string, rest string, err error) {
	labels = map[string]string{}
	i := strings.IndexAny(line, "{ \t")
	if i < 0 {
		return "", nil, "", fmt.Errorf("no value in %q", line)
	}
	name = line[:i]
	if line[i] != '{' {
		return name, labels, line[i:], nil
	}
	pos := i + 1
	for {
		for pos < len(line) && (line[pos] == ',' || line[pos] == ' ') {
			pos++
		}
		if pos >= len(line) {
			return "", nil, "", fmt.Errorf("unterminated labels in %q", line)
		}
		if line[pos] == '}' {
			return name, labels, line[pos+1:], nil
		}
		eq := strings.IndexByte(line[pos:], '=')
		if eq < 0 || pos+eq+1 >= len(line) || line[pos+eq+1] != '"' {
			return "", nil, "", fmt.Errorf("malformed label in %q", line)
		}
		key := line[pos : pos+eq]
		pos += eq + 2
		var b strings.Builder
		for {
			if pos >= len(line) {
				return "", nil, "", fmt.Errorf("unterminated label value in %q", line)
			}
			c := line[pos]
			if c == '\\' && pos+1 < len(line) {
				switch line[pos+1] {
				case 'n':
					b.WriteByte('\n')
				default:
					b.WriteByte(line[pos+1])
				}
				pos += 2
				continue
			}
			pos++
			if c == '"' {
				break
			}
			b.WriteByte(c)
		}
		labels[key] = b.String()
	}
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + l[k] + ",")
	}
	return b.String()
}

// scrapeMetrics reads the application's exposition.
func scrapeMetrics(ctx context.Context, client *http.Client, url string) (Metrics, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return ParseMetrics(string(body))
}
