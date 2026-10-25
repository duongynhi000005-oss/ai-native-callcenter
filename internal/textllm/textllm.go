// SPDX-License-Identifier: Apache-2.0

// Package textllm is the platform's text model: one OpenAI-compatible Chat
// Completions call, configured apart from the voice provider.
//
// The voice engine (AICC_PROVIDER, decision A1) speaks a realtime wire
// protocol and is chosen once per deployment. Work that is plain text in, text
// out (a wrap-up draft, a conversation summary) has nothing to do with it, so
// it gets its own endpoint, model and key, and this narrow interface that a
// test can fake.
package textllm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Request is one single-turn completion.
type Request struct {
	System string
	User   string
	// JSON asks for a JSON object as the answer (response_format json_object).
	// Most compatible servers also insist the prompt itself mentions JSON.
	JSON bool
}

// Client makes one completion and returns the model's text. Implementations
// honour ctx for cancellation and deadline; the caller owns the timeout.
type Client interface {
	Complete(ctx context.Context, req Request) (string, error)
}

// Config selects the endpoint, model and credential of the HTTP client.
type Config struct {
	// Endpoint is the API base, without /chat/completions, for example
	// https://dashscope.aliyuncs.com/compatible-mode/v1.
	Endpoint string
	Model    string
	APIKey   string
}

// Enabled says the text model is configured enough to be called.
func (c Config) Enabled() bool {
	return c.Endpoint != "" && c.Model != "" && c.APIKey != ""
}

// maxResponseBytes bounds what is read back; an answer worth keeping is small.
const maxResponseBytes = 1 << 20

// HTTP is the Chat Completions client.
type HTTP struct {
	cfg    Config
	client *http.Client
}

// New builds the HTTP client. It does not dial.
func New(cfg Config, hc *http.Client) (*HTTP, error) {
	if !cfg.Enabled() {
		return nil, errors.New("textllm: endpoint, model and key are all required")
	}
	if !strings.HasPrefix(cfg.Endpoint, "http://") && !strings.HasPrefix(cfg.Endpoint, "https://") {
		return nil, fmt.Errorf("textllm: endpoint must be an http(s) URL, got %q", cfg.Endpoint)
	}
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	return &HTTP{cfg: cfg, client: hc}, nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Temperature    float64           `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements Client.
func (h *HTTP) Complete(ctx context.Context, req Request) (string, error) {
	body := chatRequest{Model: h.cfg.Model, Temperature: 0}
	if req.System != "" {
		body.Messages = append(body.Messages, chatMessage{Role: "system", Content: req.System})
	}
	body.Messages = append(body.Messages, chatMessage{Role: "user", Content: req.User})
	if req.JSON {
		body.ResponseFormat = map[string]string{"type": "json_object"}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("textllm: encode request: %w", err)
	}
	url := strings.TrimRight(h.cfg.Endpoint, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("textllm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+h.cfg.APIKey)

	resp, err := h.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("textllm: %w", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("textllm: read response: %w", err)
	}
	var out chatResponse
	// A non-JSON error page is reported by status below, not as a decode error.
	_ = json.Unmarshal(payload, &out)
	if resp.StatusCode/100 != 2 {
		msg := ""
		if out.Error != nil {
			msg = out.Error.Message
		}
		return "", fmt.Errorf("textllm: status %d: %s", resp.StatusCode, truncate(msg, 200))
	}
	if len(out.Choices) == 0 {
		return "", errors.New("textllm: response has no choices")
	}
	return out.Choices[0].Message.Content, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
