// SPDX-License-Identifier: Apache-2.0

package aicall

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/media"
	"github.com/rasonyang/ai-native-callcenter/internal/provider"
)

// slowStartModel is a model whose Start returns only when released, like a
// provider still waiting for its session.updated.
type slowStartModel struct {
	*fakeModel
	release chan struct{}
}

func (m *slowStartModel) Start(ctx context.Context, cfg provider.SessionConfig) error {
	select {
	case <-m.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return m.fakeModel.Start(ctx, cfg)
}

// Caller audio is never handed to the provider before Start returns. Grok
// leaves a session deaf if audio arrives before its configuration is
// acknowledged (docs/design/grok-findings.md section 2), and a trunk that opens
// media early puts frames on the leg while the model is still starting.
func TestNoCallerAudioReachesTheModelBeforeItsStartReturns(t *testing.T) {
	leg := newFakeLeg(media.LawMu)
	model := &slowStartModel{fakeModel: newFakeModel(), release: make(chan struct{})}
	session, err := New(leg, model, provider.OpenAIProfile(), Config{
		BargeGuard: -1, NoInput: -1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(func() { session.Close(context.Background()) })

	started := make(chan error, 1)
	go func() { started <- session.Start(t.Context()) }()

	for range 10 {
		leg.frames <- make([]byte, media.FrameSamples)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(model.receivedAudio()); got != 0 {
		t.Fatalf("%d frames reached the model while it was still starting", got)
	}

	close(model.release)
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(model.receivedAudio()) < 10 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := len(model.receivedAudio()); got != 10 {
		t.Errorf("%d frames reached the model after Start, want the 10 queued", got)
	}
}
