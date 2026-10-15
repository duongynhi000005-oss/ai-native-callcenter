# Gemini provider — archive note (removed 2026-10-15)

The Gemini voice provider (`AICC_PROVIDER=gemini`, `internal/provider/gemini`,
Google's Live bidirectional API) was added on 2026-09-19 and **removed on
2026-10-15** by issue #224. This note replaces the full evidence record so the
removal is not silent. The code, its tests and the original findings document
were last present at commit `9f30922` on `main`; `git show 9f30922:docs/design/gemini-findings.md`
prints the full record.

## Why it was removed

Gemini was the one engine that could not apply a mid-call instruction change.
The provider layer is to assume that a mid-call instruction update is sent as a
frame and takes effect from the next reply. On 2026-10-15 that was measured live
on every other engine (openai, qwen, gateway on the Realtime client, and doubao):
the new instructions were obeyed 6 of 6 when sent idle and 6 of 6 when sent
while the model was speaking, tools survived the update 3 of 3, and a control
arm with no update showed the marker 0 of 9 times (qwen-findings W-Q7,
doubao-findings W-D9, `docs/provider-extension.md`). Gemini could not do it, so
the owner decided to delete the provider together with everything that existed
only for it (the session-lifetime failure cause, its metric, an unbounded uplink
drain policy, and the client's own energy detector).

## What had been established

- **A separate protocol.** The session was configured once, in a setup frame
  answered before anything else could be sent. Instructions, tools and voice
  could not change while the connection was open. A turn ended twice
  (`generationComplete` for the model having stopped, `turnComplete` for a
  playback it presumed), there was no cancel primitive, and errors arrived only
  as a WebSocket close code and a truncated sentence.
- **Mid-call instructions (the finding behind the removal, §2.6).** A user-role
  turn carrying replacement instructions was refused 0 of 3 (the model said it
  could not change them); a model-role turn 0 of 3. The workaround that worked
  was to prefix the new phase paragraph to the next tool result or text cue (18
  of 18, against 0 of 6 for the control), so a phase change reached the model
  with the next turn that carried something, not at once.
- **Connection lifetime.** The connection was capped at about ten minutes with
  no reconnect, by decision; the call was released with the hangup cause
  `PROVIDER_SESSION_EXPIRED` and the caller was rescued to the fallback queue.
  The `goAway` notice was never observed, even in a 17-minute session, so that
  path was exercised by synthetic frames only.
- **Uplink write stalls.** Writes to the endpoint blocked for seconds at a time
  in most probe sockets and two live calls ended on a write timeout. Attribution
  was never established: the development machine reached the endpoint through
  a local TUN proxy, which produces the same signature, and two calls on the same
  path had no stalls at all.
- **Barge-in gate (the live acceptance run of 2026-09-20, 18 calls).** The
  latency criteria were met in both rounds, but the zero-miss criterion failed in
  both (2 misses in round 1, 1 in round 2, the latter on an uplink that was
  healthy at the time), together with `interrupted` frames while the caller was
  silent. The gate was never met for this provider; the same gate was met on
  qwen.
- **Other measured behaviour.** The model never greeted unprompted; tools
  declared `BLOCKING` held the model silent while a call was pending; the
  service accepted both lowercase and protobuf-enum schema type names;
  `outputTranscription` and `sessionResumptionUpdate` arrived unsolicited.

## What stays

A6 in `docs/phase1-decisions.md` (a new protocol earns a client, a new engine on
a known protocol is a profile) is unchanged; its Gemini amendment is now a
removal record. The shared plumbing the client used (`wsconn`, `Watchdog`,
`MergeHint`, `SayExactly`, `pacer`) remains for the clients that are left.
