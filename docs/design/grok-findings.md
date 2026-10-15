# Grok voice realtime — live findings (2026-10-15, PARTIAL)

Measurement only. No Grok `Profile` exists and none was added. The test is
`internal/provider/live_grok_test.go` (gated by `AICC_LIVE_PROVIDER_TEST` and
`XAI_API_KEY`; `AICC_LIVE_PROVIDER_TEST=1 go test -count=1 -run LiveGrok -v
./internal/provider/`). It mirrors the doubao W-D9 measurement
(`doubao-findings.md`): same persona, same marker paragraph, same qwen-TTS
clips, here converted to G.711 mu-law 8 kHz and sent as `audio/pcmu`.

**This run is incomplete.** The first eight-probe pass opened sessions
normally, then the caller audio never produced a server turn (below), and after
about a dozen sessions `api.x.ai` stopped completing the TLS handshake from this
machine (`unexpected eof`, curl exit 35, also for plain `GET /v1/models` with
the same key; other hosts were fine) for more than ten minutes. The five
instruction arms (control, idle, speaking, tools-control, tools) were therefore
**NOT MEASURED**. Labels: DOC = from the vendor documentation, unverified;
MEASURED = observed on the wire on 2026-10-15.

## 1. Connection

| What was tried | Result |
|---|---|
| `wss://api.x.ai/v1/realtime?model=grok-voice-3.0`, `Authorization: Bearer` | MEASURED: accepted. The upgrade response carried no request id; only `Cf-Ray` (Cloudflare). |
| First events | MEASURED: `session.created` (`session.id`, `voice: xai_ara`, `turn_detection: {type: null}`, `modalities: ["audio"]`, `tools: []`), `conversation.created`, then a `ping` event with a `timestamp` every 10 s (unknown to the repo client; it would log it as unmapped). |

Session ids seen (`session.created.session.id`): `3a5f96c5da124ab499e6a286cf9a80a4`
(probe 1), `6b915f5745cc4da2a6a38065b688f85b` (2), `b52190e150564409b56b6fd42a938713` (3),
`4b2c5a68afa541cbbb70dd4c20abdc62` (4), `84c9c40c73354cedac1aca9d1fd124b9` (5).

## 2. Protocol probes

| # | What was tried | Result |
|---|---|---|
| 1 | `session.update` with `session.type:"realtime"` and `instructions` only | MEASURED: accepted, answered by `session.updated` that echoes the new instructions; no error. The same update without `type` is also accepted. The echo of an instructions-only update omits `audio`, `turn_detection` and `tools` keys; whether they were kept was only shown for `turn_detection`/`audio` indirectly (no error), not for `tools` (arm not run). The echo flips `keep_context` from `false` to `true` after the first update. |
| 2 | `turn_detection {type: server_vad, threshold: 0.6, silence_duration_ms: 500, prefix_padding_ms: 200}` | MEASURED: accepted without error. `session.updated` echoes `silence_duration_ms` and `prefix_padding_ms` but **not** `threshold` (silently dropped or unreported). A later update with `{type: server_vad, silence_duration_ms: 500}` echoes the same. Whether 500 ms is *honoured* was not measured. |
| 3 | `response.create` on an empty conversation, instructions asking for a greeting | MEASURED: it greets from the instructions: transcript `您好，这里是 NovaNet 客服，很高兴为您服务。`, 30641 bytes of pcmu (3.84 s of audio per `usage.output_audio_seconds`, i.e. 8 bytes/ms would give 3830 ms, so the 8 kHz byte count matches), `response.done` status `completed`. No error and no cue is needed, unlike qwen. |
| 5b | `response.cancel` with nothing active | MEASURED: `error`, `code: invalid_request_error`, message `Cancellation failed: no active response found`. Non-fatal; the session stayed open. |
| 4 | Barge-in; truncate (5); temperature (6); `grok-transcribe` events (7) | **NOT MEASURED**, see section 3 and the NOT MEASURED list. |
| 8 | G.711 pcmu 8 kHz out | Partly MEASURED via probe 3: output arrives as `audio/pcmu`, byte count consistent with the reported seconds. G.711 *in* was not shown to work (section 3). |

Event vocabulary seen on a response (probe 3), in order: `response.created`
(`status_details: "unimplemented"`, `usage: {}`), `response.output_item.added`,
`conversation.item.added`, `response.content_part.added`,
`response.output_audio.delta` and `response.output_audio_transcript.delta`
(many), `response.output_audio_transcript.done` (the whole transcript),
`response.content_part.done`, `response.output_audio.done`,
`response.output_item.done`, `response.done`. `response.done` carries `usage`
with `billable_audio_seconds`, `output_audio_seconds` and token details. Item
and response ids are UUIDs. `previous_item_id` appears on every event, usually
`null`.

## 3. Caller audio did not start a turn (unresolved)

In probes 4, 5 and (before the block) the debug run, the clips were streamed as
`input_audio_buffer.append` frames of 160 bytes of mu-law every 20 ms (silence
`0xFF` between clips) into a session whose `session.update` had set
`audio.input.format.type = audio/pcmu` and `turn_detection.type = server_vad`
and was acknowledged. For 25 to 30 s no `input_audio_buffer.speech_started`,
`speech_stopped`, `input_audio_buffer.committed` or reply arrived; only the
10 s `ping`. No error was returned for the appends. Two readings, neither
checked: Grok's VAD wants something this stream lacks (a different input
format echo: the `session.updated` shows `input_audio_format: "not specified"`
next to the nested `audio.input.format`, and `transport: "json"`), or this
clip/stream is not recognised as speech. The debug run that would have tried
`input_audio_buffer.commit` plus `response.create` after the clip was the one
that hit the network block. Until this is resolved no arm that needs a spoken
question can be run.

## 4. NOT MEASURED

- All five instruction arms: control, idle, speaking, tools-control, tools
  (does an instructions-only `session.update` change the next replies, does it
  keep declared tools, does the post-tool reply carry the marker).
- Barge-in: whether the server cancels its own response, and the
  `response.done` status of a cancelled one.
- `response.cancel` while a response is streaming.
- `conversation.item.truncate` after a completed response (acknowledged or
  error) and with an unknown item id.
- `temperature` in `session.update`.
- Input transcription with `grok-transcribe` and its event names.
- Whether `silence_duration_ms: 500` changes turn timing.
- Why server VAD saw no speech (section 3).

The test file contains probes and arms for all of these; re-run it once
`api.x.ai` is reachable again and fill the gaps.
