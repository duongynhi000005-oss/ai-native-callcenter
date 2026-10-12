// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rasonyang/ai-native-callcenter/internal/config"
	"github.com/rasonyang/ai-native-callcenter/internal/postcall"
	"github.com/rasonyang/ai-native-callcenter/internal/recording"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
	"github.com/rasonyang/ai-native-callcenter/internal/transcribe"
)

// newRecordingStorage opens the configured recording backend, or returns nil
// when none is configured. It does not touch the network: the server verifies
// the bucket itself, a reader has no need to.
//
// The filesystem backend needs the directory the switch records into; the S3
// backend works with the endpoint alone when the switch uploads directly
// (mod_http_cache), the spool directory being optional.
func newRecordingStorage(cfg config.Config) (recording.Storage, error) {
	if cfg.RecordingDir == "" && !(cfg.RecordingBackend == "S3" && cfg.S3Endpoint != "") {
		return nil, nil
	}
	rec, err := recording.New(recording.Config{
		Backend: cfg.RecordingBackend, Dir: cfg.RecordingDir,
		S3Endpoint: cfg.S3Endpoint, S3AccessKey: cfg.S3AccessKey,
		S3SecretKey: cfg.S3SecretKey, S3Bucket: cfg.S3Bucket, S3IsSSL: cfg.S3IsSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("recording storage: %w", err)
	}
	return rec, nil
}

// newPostCall builds the post-call transcription service, or nil when the
// feature is off. Enabled without a key or without recordings is a startup
// error: the alternative is a queue that fills and never drains.
func newPostCall(cfg config.Config, ledger *store.LedgerStore, recordings recording.Storage) (*postcall.Service, error) {
	if !cfg.IsPostCallTranscribeEnabled {
		return nil, nil
	}
	if recordings == nil {
		return nil, errors.New("post-call transcription: AICC_POSTCALL_TRANSCRIBE_ENABLED needs recording storage " +
			"(AICC_RECORDING_DIR or the S3 backend); there is nothing to transcribe without it")
	}
	key := os.Getenv("ALIYUN_API_KEY")
	if key == "" {
		return nil, errors.New("post-call transcription: ALIYUN_API_KEY is required")
	}
	rec, err := transcribe.NewDashScopeBatch(cfg.PostCallTranscribeEndpoint, key, cfg.PostCallTranscribeModel, nil)
	if err != nil {
		return nil, fmt.Errorf("post-call transcription: %w", err)
	}
	svc := postcall.New(postcall.Config{
		MinDurationSec:   cfg.PostCallTranscribeMinSec,
		MaxDurationSec:   cfg.PostCallTranscribeMaxSec,
		DailyMaxAudioSec: cfg.PostCallTranscribeDailyMaxMin * 60,
		Concurrency:      cfg.PostCallTranscribeConcurrency,
		MaxAttempts:      cfg.PostCallTranscribeMaxAttempts,
	}, ledger, recordings, rec, slog.Default())
	slog.Info("post-call transcription enabled", "model", rec.Model(),
		"concurrency", cfg.PostCallTranscribeConcurrency, "dailyMaxMin", cfg.PostCallTranscribeDailyMaxMin)
	return svc, nil
}

// runTranscribeRecordings implements `aicc transcribe-recordings`: the
// backfill. It plans which existing recordings the automatic policy would
// transcribe, prints the count and the audio minutes, and with -run does it,
// through the same workers the server uses. Without -run nothing is spent.
func runTranscribeRecordings(args []string) error {
	fs := flag.NewFlagSet("transcribe-recordings", flag.ExitOnError)
	since := fs.Duration("since", 7*24*time.Hour, "only recordings booked within this long ago")
	limit := fs.Int("limit", 10, "at most this many recordings")
	calls := fs.String("calls", "", "comma-separated call ids to transcribe regardless of policy (ignores -since and -limit)")
	run := fs.Bool("run", false, "do it; without this flag the command only prints the plan")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !cfg.IsPostCallTranscribeEnabled {
		return errors.New("set AICC_POSTCALL_TRANSCRIBE_ENABLED=true (and its endpoint) to plan or run a backfill")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, cfg.DatabaseURL, 4)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	recordings, err := newRecordingStorage(cfg)
	if err != nil {
		return err
	}
	svc, err := newPostCall(cfg, st.Ledger(), recordings)
	if err != nil {
		return err
	}

	var cands []postcall.Candidate
	if *calls != "" {
		for _, id := range strings.Split(*calls, ",") {
			c, err := svc.CandidateFor(ctx, strings.TrimSpace(id))
			if err != nil {
				return err
			}
			cands = append(cands, c)
		}
	} else if cands, err = svc.Plan(ctx, time.Now().Add(-*since), *limit); err != nil {
		return err
	}

	total := 0
	for _, c := range cands {
		total += c.DurationSec
		fmt.Printf("%s  %4d s\n", c.CallID, c.DurationSec)
	}
	fmt.Printf("%d recordings, %.1f audio minutes (each is sent as two mono channels)\n", len(cands), float64(total)/60)
	if !*run {
		fmt.Println("dry run: nothing was sent; add -run to transcribe")
		return nil
	}
	if _, err := svc.Enqueue(ctx, cands); err != nil {
		return err
	}
	if err := svc.Drain(ctx); err != nil {
		return err
	}
	for _, c := range cands {
		t, err := st.Ledger().GetTranscription(ctx, c.CallID)
		if err != nil {
			return err
		}
		fmt.Printf("%s  %s  attempts=%d %s\n", c.CallID, t.Status, t.Attempts, t.Error)
	}
	return nil
}
