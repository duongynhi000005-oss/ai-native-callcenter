#!/bin/sh
# SPDX-License-Identifier: Apache-2.0
#
# Smoke install of a release: builds the bundle for <tag> exactly as the
# release does, runs the stamped install.sh from it on this Linux host against
# the published images, and requires `aicc doctor` to report no FAIL.
#
#   scripts/installer-smoke.sh <tag>
#
# The images rasonyang/ai-native-callcenter:<tag> and
# rasonyang/freeswitch-aicc:<tag> must already be on Docker Hub, because the
# installer pulls them. Run as a user that can sudo without a password (a
# GitHub-hosted runner). It installs into /opt/aicc with --provider none (the
# bot never answers, so no key is needed) and removes the install, its volumes
# and the directory afterwards, pass or fail. LAN agents and audio are not
# exercised here; they stay in docs/install-verification.md.

set -eu

die() {
    printf 'installer-smoke: %s\n' "$*" >&2
    exit 1
}

[ $# -eq 1 ] || die "usage: scripts/installer-smoke.sh <tag>"
tag=$1
[ "$(uname -s)" = Linux ] || die "this smoke test installs into /opt/aicc and needs Linux"

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/aicc-smoke.XXXXXX")
log=$work/install.log
installed=0

cleanup() {
    status=$?
    if [ "$status" -ne 0 ] && [ -f /opt/aicc/docker-compose.yml ]; then
        printf '\n--- container state and logs ---\n' >&2
        (cd /opt/aicc && sudo docker compose ps -a && sudo docker compose logs --tail 200) >&2 || true
    fi
    if [ "$installed" = 1 ]; then
        sudo sh "$work/assets/install.sh" --uninstall --purge --yes >&2 || true
    fi
    rm -rf "$work"
    exit "$status"
}
trap cleanup EXIT

"$root/scripts/release-bundle.sh" "$tag" "$work/assets"
(cd "$work/assets" && sha256sum -c checksums.txt)

# The bundle comes from the directory just built: the GitHub release that
# would serve it does not exist yet. pipefail is not POSIX, so the installer's
# status is carried through a file.
installed=1
{
    rc=0
    sudo env "AICC_INSTALL_BUNDLE_DIR=$work/assets" \
        sh "$work/assets/install.sh" --provider none --yes 2>&1 || rc=$?
    echo "$rc" >"$work/status"
} | tee "$log"
[ "$(cat "$work/status")" = 0 ] || die "install.sh failed"

# The installer exits non-zero when doctor does; the log is checked as well so
# that a FAIL line can never be passed over.
grep -q 'aicc doctor' "$log" || die "the installer never ran aicc doctor"
if grep -E '^FAIL ' "$log"; then
    die "a check reported FAIL"
fi
printf 'installer-smoke: %s installed and aicc doctor passed\n' "$tag"
