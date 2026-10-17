#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# The end-to-end environment: up, smoke, down. See deploy/e2e/README.md.
#
#   deploy/e2e/e2e.sh up      fresh database, application built from this tree
#   deploy/e2e/e2e.sh smoke   place one AI outbound call and check the result
#   deploy/e2e/e2e.sh campaign  run the campaign scenarios and probes (#245)
#   deploy/e2e/e2e.sh down    collect logs, remove containers, volumes, network
#   deploy/e2e/e2e.sh config  print the resolved compose configuration
#
# COMPOSE_PROJECT_NAME picks the project (default: derived from this checkout's
# path, so two worktrees get two environments). E2E_NET_PREFIX picks the /24
# (default: derived from the project name, 10.64-127.x.y).
set -eEuo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
E2E="$ROOT/deploy/e2e"

h="$(printf '%s' "$ROOT" | cksum | cut -d' ' -f1)"
PROJECT="${COMPOSE_PROJECT_NAME:-aice2e-$h}"
if [ -n "${COMPOSE_PROJECT_NAME:-}" ]; then h="$(printf '%s' "$PROJECT" | cksum | cut -d' ' -f1)"; fi
NET="${E2E_NET_PREFIX:-10.$((64 + h % 64)).$(((h / 64) % 256))}"

WORK="$ROOT/bin/e2e/$PROJECT"
RUN="$WORK/run"
TOOLS="$WORK/tools"
APP_IP="$NET.10"; FS_IP="$NET.11"; MOCK_IP="$NET.12"; CALLEE_IP="$NET.13"
TO_NUMBER=13800000001

# Compose runs with a scrubbed environment: nothing from the caller's shell
# (provider keys, AICC_* settings, a stray .env) can reach an interpolation.
# Only the Docker client's own variables and the harness's are passed.
compose() {
  env -i PATH="$PATH" HOME="$HOME" \
    ${DOCKER_HOST:+DOCKER_HOST="$DOCKER_HOST"} ${DOCKER_CONTEXT:+DOCKER_CONTEXT="$DOCKER_CONTEXT"} \
    ${DOCKER_CONFIG:+DOCKER_CONFIG="$DOCKER_CONFIG"} ${AICC_FS_IMAGE:+AICC_FS_IMAGE="$AICC_FS_IMAGE"} \
    E2E_PROJECT="$PROJECT" E2E_RUN_DIR="$RUN" E2E_SUBNET="$NET.0/24" \
    E2E_APP_IP="$APP_IP" E2E_FS_IP="$FS_IP" E2E_MOCK_IP="$MOCK_IP" E2E_CALLEESIM_IP="$CALLEE_IP" \
    E2E_UID="$(id -u)" E2E_GID="$(id -g)" FS_EXTERNAL_IP="$FS_IP" \
    ${E2E_OUTBOUND_ENDPOINT:+E2E_OUTBOUND_ENDPOINT="$E2E_OUTBOUND_ENDPOINT"} \
    docker compose -p "$PROJECT" --project-directory "$ROOT/deploy" \
    --env-file "$E2E/e2e.env" -f "$ROOT/deploy/docker-compose.yml" -f "$E2E/docker-compose.yml" "$@"
}

env_value() { sed -n "s/^$1=//p" "$E2E/e2e.env"; }
say() { printf '[e2e] %s\n' "$*" >&2; }

daemon_arch() {
  case "$(docker info --format '{{.Architecture}}')" in
    aarch64|arm64) echo arm64 ;;
    x86_64|amd64) echo amd64 ;;
    *) echo "unsupported daemon architecture" >&2; exit 1 ;;
  esac
}

build_tools() {
  local arch; arch="$(daemon_arch)"
  mkdir -p "$TOOLS"
  say "building the harness binaries for linux/$arch"
  for cmd in aicc-calleesim aicc-mockprovider aicc-e2e; do
    (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags '-s -w' -o "$TOOLS/$cmd" "./cmd/$cmd")
  done
  # The campaign tests are Go tests under the build tag e2e, compiled into a
  # static test binary that runs inside the network like the smoke driver.
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -c -tags e2e -trimpath -ldflags '-s -w' -o "$TOOLS/aicc-e2e-campaign.test" ./internal/e2e)
  docker build -q -t "aicc-e2e-tools:$PROJECT" -f "$E2E/Dockerfile.tools" "$TOOLS" >/dev/null
  # The guard runs on the host, before anything starts.
  (cd "$ROOT" && go build -o "$WORK/aicc-e2e-host" ./cmd/aicc-e2e)
}

# render writes the resolved configuration and the one gateway file, then
# refuses to go on when the configuration is not the isolated environment.
render() {
  mkdir -p "$RUN"
  sed "s/@CALLEESIM_IP@/$CALLEE_IP/" "$E2E/calleesim-gateway.xml.in" > "$RUN/e2e_calleesim.xml"
  compose config --format json > "$RUN/compose.json"
  "$WORK/aicc-e2e-host" guard -compose-config "$RUN/compose.json" >&2
}

collect_logs() {
  mkdir -p "$RUN/logs"
  local svc
  for svc in aicc freeswitch calleesim mockprovider postgres lua-role; do
    compose logs --no-color --timestamps "$svc" > "$RUN/logs/$svc.log" 2>&1 || true
  done
  compose exec -T freeswitch fs_cli -p "$(env_value ESL_PASSWORD)" -P 18021 -x 'sofia status' > "$RUN/logs/sofia-status.txt" 2>&1 || true
  # The switch's own log file prefixes each line of a call with its channel
  # uuid (the leg id), which the console output does not: the failure bundles
  # are sliced from it.
  compose exec -T freeswitch cat /usr/local/freeswitch/log/freeswitch.log > "$RUN/logs/freeswitch-file.log" 2>/dev/null || true
}

# slice_bundles copies, into every failure bundle of a campaign run, the lines
# of the application log and the switch log that name one of the bundle's ids
# (call id, leg id, SIP Call-ID; ids.txt, written by the driver).
slice_bundles() {
  local ids dir
  for ids in "$1"/bundles/*/ids.txt; do
    [ -f "$ids" ] || continue
    dir="$(dirname "$ids")"
    grep -F -f "$ids" "$RUN/logs/aicc.log" > "$dir/aicc.log" 2>/dev/null || true
    grep -F -f "$ids" "$RUN/logs/freeswitch-file.log" > "$dir/freeswitch.log" 2>/dev/null || true
  done
}

cmd_up() {
  build_tools
  say "project $PROJECT, network $NET.0/24"
  # Refuse first, before anything of a running environment is touched.
  render
  compose down -v --remove-orphans >&2
  rm -rf "$RUN"
  render
  # A start that fails leaves its logs in the run directory.
  trap 'collect_logs' ERR
  say "building the application from this tree"
  local t0=$SECONDS
  compose build aicc >&2
  say "application build took $((SECONDS - t0)) s"
  say "starting"
  compose up -d --wait --wait-timeout 300 postgres lua-role aicc freeswitch mockprovider calleesim >&2
  compose exec -T aicc /usr/local/bin/aicc useradd -username admin -password "$(env_value E2E_PASSWORD)" -role ADMIN >&2
  say "up. Run directory: $RUN"
}

cmd_smoke() {
  [ -n "$(compose ps -q aicc 2>/dev/null)" ] || { say "the environment is not up; run: make e2e-up"; exit 1; }
  render
  local id out
  id="smoke-$(date -u +%Y%m%dT%H%M%SZ)"
  out="/run/e2e/$id"
  if compose run --rm -T --no-deps driver smoke \
      -api "http://$APP_IP:8080" -ready "http://$APP_IP:9090/readyz" \
      -calleesim "http://$CALLEE_IP:8771" -esl "$FS_IP:18021" \
      -compose-config /run/e2e/compose.json -username admin \
      -flow /etc/e2e/flow.json -to "$TO_NUMBER" -out "$out" >&2; then
    cat "$RUN/$id/smoke.json"
    say "smoke passed. Results: $RUN/$id"
  else
    local rc=$?
    collect_logs
    [ -f "$RUN/$id/smoke.json" ] && cat "$RUN/$id/smoke.json"
    say "smoke FAILED. Results and logs: $RUN"
    exit "$rc"
  fi
}

cmd_campaign() {
  [ -n "$(compose ps -q aicc 2>/dev/null)" ] || { say "the environment is not up; run: make e2e-up"; exit 1; }
  render
  local id out t0=$SECONDS
  id="campaign-$(date -u +%Y%m%dT%H%M%SZ)"
  out="/run/e2e/$id"
  if compose run --rm -T --no-deps --entrypoint /usr/local/bin/aicc-e2e-campaign.test driver \
      -test.v -test.run '^TestCampaigns$' -test.timeout 20m \
      -e2e.api "http://$APP_IP:8080" -e2e.ready "http://$APP_IP:9090/readyz" \
      -e2e.calleesim "http://$CALLEE_IP:8771" -e2e.mockprovider "http://$MOCK_IP:9099" \
      -e2e.esl "$FS_IP:18021" -e2e.compose-config /run/e2e/compose.json \
      -e2e.scenario /etc/e2e/scenario.json -e2e.flow /etc/e2e/flow.json \
      -e2e.username admin -e2e.out "$out" >&2; then
    [ -f "$RUN/$id/probes.md" ] && cat "$RUN/$id/probes.md"
    say "campaign tests passed in $((SECONDS - t0)) s. Results: $RUN/$id"
  else
    local rc=$?
    collect_logs
    slice_bundles "$RUN/$id"
    [ -f "$RUN/$id/probes.md" ] && cat "$RUN/$id/probes.md"
    say "campaign tests FAILED after $((SECONDS - t0)) s. Report, bundles and logs: $RUN"
    exit "$rc"
  fi
}

cmd_down() {
  if [ -n "$(compose ps -aq 2>/dev/null)" ]; then collect_logs; fi
  compose down -v --remove-orphans >&2
  local left
  left="$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT"; \
          docker volume ls -q --filter "label=com.docker.compose.project=$PROJECT"; \
          docker network ls -q --filter "label=com.docker.compose.project=$PROJECT")"
  [ -z "$left" ] || { say "left behind: $left"; exit 1; }
  say "down: no container, volume or network of $PROJECT remains. Logs: $RUN/logs"
}

case "${1:-}" in
  up) cmd_up ;;
  smoke) cmd_smoke ;;
  campaign) cmd_campaign ;;
  down) cmd_down ;;
  config) build_tools; render; cat "$RUN/compose.json" ;;
  *) echo "usage: $0 up|smoke|campaign|down|config" >&2; exit 2 ;;
esac
