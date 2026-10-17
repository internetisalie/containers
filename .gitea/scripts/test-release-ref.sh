#!/usr/bin/env bash
set -euo pipefail

check=$(cd -- "$(dirname -- "$0")" && pwd)/check-release-ref.sh
makefile=$(cd -- "$(dirname -- "$0")/../.." && pwd)/Makefile
fixture=$(mktemp -d "${TMPDIR:-/tmp}/containers-release-ref.XXXXXX")
trap 'rm -rf -- "$fixture"' EXIT
cd -- "$fixture"
mkdir -p .gitea/scripts
cp -- "$check" .gitea/scripts/check-release-ref.sh
cp -- "$makefile" Makefile
git init -q
git config user.name 'Release test'
git config user.email 'release-test@example.invalid'
git commit --allow-empty -qm 'fixture main'
git update-ref refs/remotes/origin/main HEAD
git tag v1.2.3
# Drive the real Make target with a harmless publisher, proving the guard is
# wired into publishing rather than only testing its implementation directly.
make --no-print-directory release GORELEASER=true

expect_failure() {
  if make --no-print-directory release GORELEASER=true "RELEASE_MAIN_REF=${2:-refs/remotes/origin/main}" >failure.log 2>&1; then
    echo "release guard accepted: $1" >&2
    exit 1
  fi
  grep -F -- "$1" failure.log >/dev/null
}

expect_failure 'release requires the fetched main ref' refs/remotes/missing/main
git checkout -qb side
git commit --allow-empty -qm 'fixture side branch'
expect_failure 'release requires an exact version tag at HEAD'
git tag experiment
expect_failure 'release requires a vMAJOR.MINOR.PATCH version tag'
git tag -d experiment >/dev/null
git tag v1.2.4
expect_failure 'release tag must point to a commit on main'
git tag -d v1.2.4 >/dev/null
git tag v1.2.4-rc.1
make --no-print-directory release GORELEASER=true
expect_failure 'release requires the fetched main ref' refs/remotes/missing/main
echo 'release ref guard: main stable and branch prerelease accepted; missing ref, untagged, malformed and off-main stable tags rejected'
