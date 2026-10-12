// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rasonyang/ai-native-callcenter/internal/api"
	"github.com/rasonyang/ai-native-callcenter/internal/postcall"
	"github.com/rasonyang/ai-native-callcenter/internal/store"
)

// PostCallTranscriber queues post-call transcription runs. A nil one means the
// deployment has the feature off; reading what exists still works.
type PostCallTranscriber interface {
	Request(ctx context.Context, callID uuid.UUID) (store.CallTranscription, error)
}

// GetPostCallTranscription serves a call's batch transcription: the run, and
// its segments once it is DONE. It is not the live transcript and shares
// nothing with it.
func (s *Server) GetPostCallTranscription(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	if s.ledger == nil {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such call", nil)
		return
	}
	if !s.mayHearCall(w, r, callID) {
		return
	}
	run, err := s.ledger.GetTranscription(r.Context(), callID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, CodeNotFound, "this call has no post-call transcription", nil)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the transcription", nil)
		return
	}
	segs := []store.TranscriptionSegment{}
	if run.Status == store.TranscriptionDone {
		if segs, err = s.ledger.TranscriptionSegments(r.Context(), callID); err != nil {
			writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot read the transcription", nil)
			return
		}
	}
	writeJSON(w, http.StatusOK, postCallTranscriptionToAPI(run, segs))
}

// RequestPostCallTranscription queues a run over the call's recording.
func (s *Server) RequestPostCallTranscription(w http.ResponseWriter, r *http.Request, callID uuid.UUID) {
	if s.postCall == nil {
		writeError(w, http.StatusConflict, CodeTranscriptionDisabled,
			"post-call transcription is not enabled on this deployment", nil)
		return
	}
	run, err := s.postCall.Request(r.Context(), callID)
	switch {
	case errors.Is(err, postcall.ErrNoRecording):
		writeError(w, http.StatusNotFound, CodeNotFound, "this call has no recording", nil)
	case errors.Is(err, postcall.ErrTooLong):
		writeError(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"the recording is longer than the configured maximum",
			map[string]any{"maxDurationSec": s.cfg.PostCallTranscribeMaxSec})
	case errors.Is(err, postcall.ErrCapReached):
		writeError(w, http.StatusTooManyRequests, CodeRateLimited,
			"the daily audio cap for post-call transcription is reached",
			map[string]any{"dailyMaxAudioMin": s.cfg.PostCallTranscribeDailyMaxMin})
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeStorageDown, "cannot queue the transcription", nil)
	default:
		writeJSON(w, http.StatusAccepted, postCallTranscriptionToAPI(run, []store.TranscriptionSegment{}))
	}
}

func postCallTranscriptionToAPI(run store.CallTranscription, segs []store.TranscriptionSegment) api.PostCallTranscription {
	out := api.PostCallTranscription{
		CallID:      run.CallID,
		Status:      api.PostCallTranscriptionStatus(run.Status),
		Model:       run.Model,
		Attempts:    run.Attempts,
		AudioSec:    run.AudioSec,
		RequestedAt: run.RequestedAt,
		StartedAt:   run.StartedAt,
		FinishedAt:  run.FinishedAt,
		Segments:    make([]api.PostCallTranscriptionSegment, 0, len(segs)),
	}
	if run.Error != "" {
		out.Error = &run.Error
	}
	for _, sg := range segs {
		item := api.PostCallTranscriptionSegment{
			Seq: sg.Seq, Channel: api.RecordedChannel(sg.Channel),
			OffsetMs: int64(sg.OffsetMs), EndMs: int64(sg.EndMs), Text: sg.Text,
		}
		if sg.Language != "" {
			lang := sg.Language
			item.Language = &lang
		}
		out.Segments = append(out.Segments, item)
	}
	return out
}
