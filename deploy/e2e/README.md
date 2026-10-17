# End-to-end environment

An isolated stack for automated outbound-call tests: PostgreSQL, the switch
image, the application built from this checkout, `aicc-mockprovider` (the voice
provider) and `aicc-calleesim` (the called party). The test drivers
(`cmd/aicc-e2e` for the smoke, a Go test binary for the campaign tests) run
inside the same network and talk to the application only through the public
API, with a scoped API key.

```sh
make e2e-up        # fresh database, application built from the working tree
make e2e-smoke     # one AI outbound call; checks the callee's record and the CDR
make e2e-campaign  # two campaigns (US, CN): unanswered outcomes and SIP probes
make e2e-down      # collect logs, remove containers, volumes and the network
```

Needs Docker with Compose v2.24 or later and Go. Nothing else on the host:
the harness binaries are cross-compiled statically for the Docker daemon's
architecture and run in an image with no shell and no libraries.

## Why it cannot dial a real number

The isolation is a property of the network, not of configuration.

- Every service is on one compose network with `internal: true`
  (`docker-compose.yml`, bottom). A container on it has no route out: no
  container can reach a trunk, a provider, a registry or a name server.
- No service publishes a port, uses host networking or joins another network.
  The `deploy/compose.linux.yml` and `deploy/compose.macos.yml` overlays are not
  used.
- The switch has one extra gateway, `e2e_calleesim`, whose only peer is the
  simulator's fixed address (`calleesim-gateway.xml.in`, rendered into the run
  directory and mounted as that one file; the directory is never mounted, so
  `aicc_bot.xml` stays). Nothing from `deploy/dev/freeswitch`, the development
  machine's trunk, is mounted.
- `AICC_OUTBOUND_ENDPOINT` is `sofia/gateway/e2e_calleesim/%s`. The provider is
  the mock, reached by endpoint, with the placeholder key `not-a-key`; no real
  key is passed.
- The base file's `deploy/.env` is not read (`env_file: !reset []`, and
  `e2e.sh` runs Compose with a scrubbed environment and its own
  `--env-file deploy/e2e/e2e.env`).

On top of that the harness refuses a run (`internal/e2e/guard.go`):

- before anything starts, `e2e.sh` resolves the configuration
  (`docker compose config`) and `aicc-e2e guard` rejects it unless the network
  is internal and alone, no service publishes a port, `AICC_OUTBOUND_ENDPOINT`
  is exactly the simulator's gateway and the provider is the mock;
- the smoke repeats that check from inside, then asks the switch for
  `sofia status` and rejects it unless its only gateways are `aicc_bot` and
  `e2e_calleesim`.

`E2E_OUTBOUND_ENDPOINT=sofia/gateway/other/%s make e2e-up` is the way to see the
refusal.

## Several worktrees at once

The project name is derived from the checkout's path
(`aice2e-<number>`), and so is the `/24` the network uses (`10.64-127.x.y`), so
two worktrees get two environments that share nothing. Volumes, containers and
the network carry the project name; `e2e-down` removes only those and never
touches the `aicc` project of the demo stack. Pin either one yourself with
`COMPOSE_PROJECT_NAME` and `E2E_NET_PREFIX` (for example `10.77.3`).

Addresses on the network, from the prefix: `.10` application, `.11` switch,
`.12` mock provider, `.13` callee simulator.

## What the smoke checks

1. The driver signs in as the administrator the environment creates, mints an
   API key with only the scopes it needs, and uses that key for everything
   after.
2. It publishes a one-phase flow (`flow.json`) and creates a DID that allows
   outbound calls, then places `POST /calls` with `kind: AI_OUTBOUND` to
   `13800000001`, the number `scenario.json` answers with 200.
3. It asserts: exactly one new dialog in the simulator's record, for that
   number, answered with 200 and acknowledged, with downlink audio above
   silence; exactly one CDR for the DID, and exactly one more row in the ledger,
   for that call, typed `OUTBOUND` and with an answer time.

Every equality is paired with a count, so an empty result fails. Latency
(`firstAudioAfterAnswer`, `inviteToAnswer`, `placeToInvite`) is written to the
summary and never asserted. The smoke provisions its own flow and DID with a
random number, so it can be repeated against a running environment; `make
e2e-up` is what gives a fresh database.

## Campaign tests

`make e2e-campaign` runs the tests of `internal/e2e/campaign*.go` (issue
#245). They are Go tests under the build tag `e2e`: `e2e.sh up` compiles them
into a static test binary (`go test -c -tags e2e`) that goes into the harness
image and runs with `docker compose run`, inside the internal network, like the
smoke driver. They need an environment that is up; like the smoke, they can be
repeated against a running one, and `make e2e-up` is what gives a fresh one.

Two campaigns, one per country, are created and started together, each with
its own DID (the caller id in `From`) and every case of the table below:

- US: `+1 202 555 01xx`, the fictional range;
- CN: `+86 139 8888 66xx`, 11 national digits, a range reserved for this
  harness.

Numbers are never mixed within a campaign, and no number of either range may
be replaced by one a real trunk rings.

| case | callee (`scenario.json`) | asserted |
| --- | --- | --- |
| C03 | 180, then 486 | `BUSY`, cause `USER_BUSY` |
| C05 | 180, then 603 | `REJECTED`, cause `CALL_REJECTED` |
| C06 | 180, then 404 | `INVALID_NUMBER`, cause `UNALLOCATED_NUMBER`; contact `INVALID_NUMBER`; no further INVITE |
| C07 | 180, never answered | `NO_ANSWER`; CANCEL at the 5 s ring timeout ±1 s; no provider session |
| C08 | 183 with early media, never answered | `NO_ANSWER`; CANCEL; no provider session |
| C09 | number on the do-not-call list | `DNC_SKIPPED`, cause `NUMBER_ON_DNC`; contact `DNC`; zero INVITEs |
| P1–P5 | 180, then 480, 408, 503, 484, 410 | recorded, and held to design 10 §7.2's cause table |

C07's bound is read from the attempt's `startedAt`, which the dialer writes
just before it sends the originate whose timeout the ring timeout is. The
switch checks that timeout in whole seconds, so a 5 s ring timeout rings for
more than 4 s and at most 5 s; measured from the INVITE, a ring that began
late in a second can read just under 4 s although nothing is wrong. Both
times are in the report (`cancelAfterStartMs`, `cancelAfterInviteMs`).

The policy is the validated minimum: ring timeout 5 s, every interval and the
transient backoff 60 s, `maxAttempts` and `maxSystemRetries` 2, calling window
00:00–24:00 every day. So a retried outcome is dialled exactly twice and ends
`EXHAUSTED`, whichever budget it spends, and a terminal one is dialled once. A
run lasts a little over a minute, most of it the wait for the retries.

For every case and both countries the run asserts the attempt's outcome and
hangup cause; the contact's status, `attempts`, `systemFailures` and
`nextAttemptAt` right after the first attempt (end + 60 s, or none) and at the
end; that the retry did not start early; one CDR per dialled attempt with the
same cause, the dialled string and no answer; and the simulator's INVITEs for
the number. Over the run: every dialog belongs to one contact and came from its
own campaign's caller id, no INVITE of one campaign carries the other country's
number, the simulator saw nothing its scenario does not know, and the mock
provider opened no session at all (`GET /_stats` on it, before and after). The
two countries must come out identical on every column of the measured table;
a difference fails the run. Every equality is paired with a count. Latency is
reported, never asserted.

The deployment's `AICC_OUTBOUND_DIAL_FORM` is `E164` (the default, spelt out in
the compose overlay): the switch is handed `+12025550103` and `+8613988886603`,
which is the Request-URI user part the simulator matches. The driver reads the
dial form from the resolved configuration and refuses to start when
`scenario.json` does not hold every case's number in that form with the case's
responses.

Checks that hold for any run, rather than for one case, plug in as
`AfterRunChecks` (`internal/e2e/campaign_checks.go`); issue #246 adds its
invariants there.

A run writes `campaign-<time>/report.json` (every assertion, per region, case
and attempt, with the simulator's dialog and the CDRs) and `probes.md` (the
measured table: SIP response → `hangup_cause` → outcome → retry interval and
budget). On a failure, `bundles/<region>-<case>-<n>/` holds that attempt, its
dialog, and the lines of the application log and the switch log that name its
call id, leg id or SIP Call-ID, sliced by `e2e.sh` from `logs/aicc.log` and
`logs/freeswitch-file.log` (the switch's own log file, which prefixes a call's
lines with its channel uuid, the leg id).

## Reading a run

`bin/e2e/<project>/run/` (git-ignored) holds, after a run:

| Path | Content |
| --- | --- |
| `smoke-<time>/smoke.json` | every assertion made, failures, latencies, condensed dialog and CDR |
| `smoke-<time>/calleesim-report.json` | the simulator's full record: INVITE headers, responses, energy per 100 ms |
| `smoke-<time>/cdr.json` | the CDR list for the DID |
| `campaign-<time>/report.json` | the campaign run: every assertion, per region, case and attempt |
| `campaign-<time>/probes.md` | the measured table, one block per country |
| `campaign-<time>/bundles/` | per failed attempt: attempt, dialog, CDRs and the sliced logs |
| `logs/<service>.log` | each container's output, written on a failed up, smoke or campaign run and by `e2e-down` |
| `logs/freeswitch-file.log` | the switch's log file, each call's lines prefixed with its leg id |
| `logs/sofia-status.txt` | the switch's gateways |
| `compose.json` | the resolved configuration the guard checked |
| `e2e_calleesim.xml` | the rendered gateway file |
| `calleesim-final.json` | the simulator's record at shutdown |

CI (`.github/workflows/e2e.yml`) uploads this directory as an artifact when the
job fails.

## Notes

- The switch image is the one `deploy/docker-compose.yml` pins (`AICC_FS_IMAGE`
  overrides it); it is pulled, never built. The working tree's
  `freeswitch/scripts` is mounted over the image's, so a Lua change is tested
  without rebuilding. Changes to `freeswitch/conf` need a new image.
- The application image `aicc-e2e:<project>` and the harness image
  `aicc-e2e-tools:<project>` stay after `e2e-down` so the next build is warm.
  Remove them with `docker rmi` when done.
