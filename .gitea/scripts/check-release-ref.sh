#!/usr/bin/env bash
set -euo pipefail

main_ref=${1:-refs/remotes/origin/main}
if ! tag=$(git describe --exact-match --tags HEAD 2>/dev/null); then
  echo 'release requires an exact version tag at HEAD' >&2
  exit 1
fi
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo 'release requires a vMAJOR.MINOR.PATCH version tag' >&2
  exit 1
fi
if ! git show-ref --verify --quiet "$main_ref"; then
  echo "release requires the fetched main ref: $main_ref" >&2
  exit 1
fi
if ! git merge-base --is-ancestor HEAD "$main_ref"; then
  echo 'release tag must point to a commit on main' >&2
  exit 1
fi
