#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Stress run of the tests a change touches, timed like CI, so a test that
# assumes one goroutine finishes before another fails here instead of on a
# busy CI runner later (CONTRIBUTING.md, "Tests with goroutines, fakes,
# sockets or timers").
#
#   make test-stress PKG=./internal/calleesim RUN=TestCodecIsNegotiatedPerOffer
#   PKG='./internal/a ./internal/b' COUNT=50 scripts/test-stress.sh
#
# Settings come from the environment (the Makefile passes them through):
#
#   PKG    required: the packages to stress, space-separated; never ./...
#   RUN    optional: the -run pattern
#   COUNT  -count, default 20
#   CPU    -cpu, default 1,2,8
#   CORES  how many CPUs to run on, default 4 (CI's runner); 0 uses them all
#
# The run shares a few CPUs with a load: CI's own `go test -race -count=1
# ./...` on a loop, plus one busy loop per CPU. Load alone does not reproduce
# CI on a machine with many cores, because the scheduler always finds a free
# core for the goroutine a test is waiting on. So the script confines itself,
# and with it the load and the run: on Linux to CORES CPUs with taskset, on
# macOS to background QoS, which keeps it on the efficiency cores of Apple
# silicon (their number is fixed, so CORES only switches this on or off).
# Elsewhere it runs unconfined and says so. The run starts once the load is
# running tests.
#
# It runs in the module of the current directory. The load never sees
# AICC_TEST_DATABASE_URL, and its output and temporary files go to a directory
# that is removed afterwards. The load and the run each have their own process
# group, and both groups are stopped on success, failure, Ctrl-C and SIGTERM.
# The exit status is the run's (130 when interrupted).

set -u

die() {
    printf 'test-stress: %s\n' "$*" >&2
    exit 2
}

count=${COUNT:-20}
cpu=${CPU:-1,2,8}
run=${RUN:-}
cores=${CORES:-4}

set -f
# shellcheck disable=SC2206 # PKG is a space-separated list of packages
pkgs=(${PKG:-})
set +f
[ ${#pkgs[@]} -gt 0 ] || die "PKG is required: the packages your change touches, e.g. make test-stress PKG=./internal/calleesim RUN=TestName"
for p in "${pkgs[@]}"; do
    case $p in
        ./... | ... | all) die "PKG=$p is the whole tree; name the packages your change touches" ;;
    esac
done
case $cores in
    '' | *[!0-9]*) die "CORES=$cores: want a number of CPUs, or 0 for all of them" ;;
esac

# Confine this script, and so everything it starts, by running it again under
# the confining command. TEST_STRESS_ON says where it landed and how many busy
# loops fill that.
if [ -z "${TEST_STRESS_ON:-}" ] && [ "$cores" -gt 0 ]; then
    if command -v taskset >/dev/null 2>&1 && command -v nproc >/dev/null 2>&1; then
        if [ "$(nproc)" -le "$cores" ]; then
            : # no more CPUs than CI has: nothing to confine
        elif taskset -c "0-$((cores - 1))" true 2>/dev/null; then
            export TEST_STRESS_ON="CPUs 0-$((cores - 1))" TEST_STRESS_SPIN=$cores
            exec taskset -c "0-$((cores - 1))" bash "$0"
        else
            printf 'test-stress: taskset cannot use CPUs 0-%s here, so the run is not confined and may pass where CI fails\n' "$((cores - 1))" >&2
        fi
    elif command -v taskpolicy >/dev/null 2>&1 &&
        ecores=$(sysctl -n hw.perflevel1.logicalcpu 2>/dev/null) && [ "$ecores" -gt 0 ]; then
        export TEST_STRESS_ON="the $ecores efficiency cores" TEST_STRESS_SPIN=$ecores
        exec taskpolicy -c background bash "$0"
    else
        printf 'test-stress: cannot confine the run to %s CPUs here, so it may pass where CI fails\n' "$cores" >&2
    fi
fi
ncpu=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null) || ncpu=4
on=${TEST_STRESS_ON:-all $ncpu CPUs}
spin=${TEST_STRESS_SPIN:-$ncpu}

# The load writes its output and its temporary files (what an interrupted test
# leaves behind) under one directory, removed at the end.
work=$(mktemp -d "${TMPDIR:-/tmp}/test-stress.XXXXXX") || die "cannot create a temporary directory"
log=$work/load.log
mkdir "$work/tmp" || {
    rm -rf "${work:?}"
    die "cannot create $work/tmp"
}
load_pid=
stress_pid=

# stop_group interrupts every process in the group led by $1 (go cleans up on
# SIGINT), waits up to 10 s for the group to empty, then kills what is left.
stop_group() {
    local pg=$1 i=0
    kill -INT -- "-$pg" 2>/dev/null || return 0
    while kill -0 -- "-$pg" 2>/dev/null; do
        if [ "$i" -ge 100 ]; then
            kill -KILL -- "-$pg" 2>/dev/null
            break
        fi
        sleep 0.1
        i=$((i + 1))
    done
    wait "$pg" 2>/dev/null
}

cleanup() {
    [ -z "$stress_pid" ] || stop_group "$stress_pid"
    [ -z "$load_pid" ] || stop_group "$load_pid"
    stress_pid=
    load_pid=
    rm -rf "${work:?}"
}

interrupted() {
    trap '' INT TERM
    printf '\ntest-stress: interrupted, stopping the run and the load\n' >&2
    cleanup
    exit 130
}

trap cleanup EXIT
trap interrupted INT TERM

# Job control gives each background job its own process group, so stopping a
# group also reaches the test binaries go starts, and SIGINT is not ignored.
# The busy loops are subshells of the load, in its group; an asynchronous
# subshell starts with SIGINT ignored, hence their `trap - INT`.
set -m
(
    trap 'exit 0' INT TERM
    unset AICC_TEST_DATABASE_URL
    export TMPDIR="$work/tmp"
    i=0
    while [ "$i" -lt "$spin" ]; do
        (
            trap - INT
            while :; do :; done
        ) &
        i=$((i + 1))
    done
    while :; do go test -race -count=1 ./...; done
) </dev/null >"$log" 2>&1 &
load_pid=$!
set +m

printf 'test-stress: load: go test -race -count=1 ./... on a loop and %s busy loops, on %s; waiting until it runs tests\n' "$spin" "$on" >&2
i=0
until grep -Eq '^(ok|FAIL)' "$log"; do
    [ "$i" -lt 1200 ] || die "the load ran no test in 10 minutes; its log ends: $(tail -n 5 "$log")"
    sleep 0.5
    i=$((i + 1))
done

cmd=(go test -race "-count=$count" -cpu "$cpu")
shown="${cmd[*]}"
if [ -n "$run" ]; then
    cmd+=(-run "$run")
    shown="$shown -run '$run'"
fi
cmd+=("${pkgs[@]}")
printf 'test-stress: + %s %s\n' "$shown" "${pkgs[*]}" >&2

set -m
"${cmd[@]}" </dev/null &
stress_pid=$!
set +m

# The script exits with the run's status; the EXIT trap keeps it.
wait "$stress_pid"
