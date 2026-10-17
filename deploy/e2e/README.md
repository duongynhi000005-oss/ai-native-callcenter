# End-to-end environment

An isolated stack for automated outbound-call tests: PostgreSQL, the switch
image, the application built from this checkout, `aicc-mockprovider` (the voice
provider) and `aicc-calleesim` (the called party). A test driver
(`cmd/aicc-e2e`) runs inside the same network and talks to the application only
through the public API.

```sh
make e2e-up       # fresh database, application built from the working tree
make e2e-smoke    # one AI outbound call; checks the callee's record and the CDR
make e2e-down     # collect logs, remove containers, volumes and the network
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

## Reading a run

`bin/e2e/<project>/run/` (git-ignored) holds, after a run:

| Path | Content |
| --- | --- |
| `smoke-<time>/smoke.json` | every assertion made, failures, latencies, condensed dialog and CDR |
| `smoke-<time>/calleesim-report.json` | the simulator's full record: INVITE headers, responses, energy per 100 ms |
| `smoke-<time>/cdr.json` | the CDR list for the DID |
| `logs/<service>.log` | each container's output, written on a failed up or smoke and by `e2e-down` |
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
