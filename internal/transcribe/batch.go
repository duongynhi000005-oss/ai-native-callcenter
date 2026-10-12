// SPDX-License-Identifier: Apache-2.0

package transcribe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBatchModel is the recorded-speech model post-call transcription uses
// when none is configured.
const DefaultBatchModel = "qwen-audio-3.1-asr-flash"

// BatchRequest is one mono clip for the batch recogniser.
type BatchRequest struct {
	// WAV is a complete mono 16-bit PCM WAV file.
	WAV []byte
	// SampleRate is the clip's rate in Hz, sent as a parameter.
	SampleRate int
	// LanguageHints narrows recognition (at most 4); empty lets the model decide.
	LanguageHints []string
}

// BatchSentence is one recognised sentence; times are relative to the clip.
type BatchSentence struct {
	Text    string
	StartMs int
	EndMs   int
}

// BatchResult is what the recogniser heard in one clip.
type BatchResult struct {
	// Text is the whole clip's text.
	Text string
	// Language as reported by the engine; empty when it does not say.
	Language string
	// Sentences are the timed pieces; empty when the engine returned only Text.
	Sentences []BatchSentence
}

// BatchRecognizer recognises a finished clip in one request. It is the batch
// counterpart of Session and deliberately shares nothing with it: one is a
// duplex stream with partials, this is a request that either returns the text
// or fails.
type BatchRecognizer interface {
	Model() string
	Recognize(ctx context.Context, req BatchRequest) (BatchResult, error)
}

// BatchError is a failed batch request. Retryable says whether sending the
// same clip again could succeed: throttling, timeouts and 5xx can, a rejected
// clip cannot.
type BatchError struct {
	Status    int
	Code      string
	Message   string
	Retryable bool
}

func (e *BatchError) Error() string {
	s := "batch recognition"
	if e.Status != 0 {
		s += " http " + strconv.Itoa(e.Status)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// IsRetryable reports whether err is worth retrying. Anything that is not a
// BatchError (a network failure, a deadline) is.
func IsRetryable(err error) bool {
	var be *BatchError
	if errors.As(err, &be) {
		return be.Retryable
	}
	return true
}

// DashScopeBatch is the synchronous HTTP client for the DashScope
// multimodal-generation endpoint (design 08 section 18, issue #27). One clip
// per request, at most five minutes.
type DashScopeBatch struct {
	endpoint string
	apiKey   string
	model    string
	client   *http.Client
}

// NewDashScopeBatch builds the client. endpoint is the full HTTPS URL of the
// generation endpoint on the workspace host; it is never the WebSocket one.
func NewDashScopeBatch(endpoint, apiKey, model string, client *http.Client) (*DashScopeBatch, error) {
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		return nil, fmt.Errorf("batch endpoint %q must be an http(s) URL", endpoint)
	}
	if apiKey == "" {
		return nil, errors.New("batch recognition needs an API key")
	}
	if model == "" {
		model = DefaultBatchModel
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Minute}
	}
	return &DashScopeBatch{endpoint: endpoint, apiKey: apiKey, model: model, client: client}, nil
}

// Model returns the model id every request names.
func (c *DashScopeBatch) Model() string { return c.model }

type batchWireAudio struct {
	Data string `json:"data"`
}
type batchWireContent struct {
	Type       string         `json:"type"`
	InputAudio batchWireAudio `json:"input_audio"`
}
type batchWireMessage struct {
	Role    string             `json:"role"`
	Content []batchWireContent `json:"content"`
}

// Recognize sends one clip.
func (c *DashScopeBatch) Recognize(ctx context.Context, req BatchRequest) (BatchResult, error) {
	if len(req.WAV) == 0 {
		return BatchResult{}, &BatchError{Message: "empty clip"}
	}
	params := map[string]any{
		"format":      "wav",
		"sample_rate": strconv.Itoa(req.SampleRate),
	}
	if len(req.LanguageHints) > 0 {
		hints := req.LanguageHints
		if len(hints) > 4 {
			hints = hints[:4]
		}
		params["language_hints"] = hints
	}
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"input": map[string]any{"messages": []batchWireMessage{{
			Role: "user",
			Content: []batchWireContent{{
				Type:       "input_audio",
				InputAudio: batchWireAudio{Data: "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(req.WAV)},
			}},
		}}},
		"parameters": params,
	})
	if err != nil {
		return BatchResult{}, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return BatchResult{}, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return BatchResult{}, fmt.Errorf("batch recognition: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return BatchResult{}, fmt.Errorf("batch recognition: reading the response: %w", err)
	}
	return parseBatchResponse(resp.StatusCode, raw)
}

// batchWireResponse is the part of the response this client reads. The sentences key
// is `sentence` on a plain request and `sentences` with diarization, so both
// are accepted; times are milliseconds.
type batchWireResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Output  struct {
		Text      string          `json:"text"`
		Language  string          `json:"language"`
		Sentence  json.RawMessage `json:"sentence"`
		Sentences json.RawMessage `json:"sentences"`
	} `json:"output"`
}

type batchWireSentence struct {
	Text      string   `json:"text"`
	BeginTime *float64 `json:"begin_time"`
	EndTime   *float64 `json:"end_time"`
	Language  string   `json:"language"`
}

func parseBatchResponse(status int, raw []byte) (BatchResult, error) {
	var r batchWireResponse
	_ = json.Unmarshal(raw, &r)

	if status != http.StatusOK || r.Code != "" {
		be := &BatchError{Status: status, Code: r.Code, Message: r.Message}
		if be.Message == "" {
			be.Message = strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		}
		be.Retryable = status == http.StatusTooManyRequests || status >= 500 || status == http.StatusRequestTimeout ||
			strings.Contains(r.Code, "Throttling") || strings.Contains(r.Code, "Timeout")
		return BatchResult{}, be
	}

	out := BatchResult{Text: strings.TrimSpace(r.Output.Text), Language: r.Output.Language}
	for _, rawList := range []json.RawMessage{r.Output.Sentences, r.Output.Sentence} {
		if len(out.Sentences) > 0 || len(rawList) == 0 {
			continue
		}
		var list []batchWireSentence
		if err := json.Unmarshal(rawList, &list); err != nil {
			var one batchWireSentence
			if json.Unmarshal(rawList, &one) != nil {
				continue
			}
			list = []batchWireSentence{one}
		}
		for _, s := range list {
			text := strings.TrimSpace(s.Text)
			if text == "" {
				continue
			}
			bs := BatchSentence{Text: text}
			if s.BeginTime != nil {
				bs.StartMs = int(*s.BeginTime)
			}
			if s.EndTime != nil {
				bs.EndMs = int(*s.EndTime)
			}
			if out.Language == "" {
				out.Language = s.Language
			}
			out.Sentences = append(out.Sentences, bs)
		}
	}
	return out, nil
}
