#!/usr/bin/env bash
# prints the next vX.Y.Z tag based on conventional commits since the latest tag.
# prints nothing and exits 0 if no bump is warranted (no feat/fix/breaking change).
#
# bump rules:
#   feat:               minor
#   fix:                patch
#   type!   (subject)   major
#   BREAKING CHANGE:    major  (footer in body)
#   chore/docs/refactor/perf/test/build/ci   none
#   unrecognized                         none
#
# multiple commits: take the max bump.
set -euo pipefail

# ponytail: this exists. add a real version tool if bash math ever isn't enough
# (e.g. semver pre-release tags, build metadata, complex ranges).

LATEST="$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null || echo v0.0.0)"
LATEST_VER="${LATEST#v}"

# --no-merges: linear history means merge commits are noise. squash-merge
# collapses a PR to a single commit, rebase-merge keeps individual commits.
# either way we want the *content*, not the merge marker.
# keep the `v` prefix in the range: "0.1.0..HEAD" is parsed as a pathspec.
RANGE="${LATEST}..HEAD"
# ponytail: if the range is empty (e.g. the workflow fires on a tag that
# already points at HEAD), bail. don't use $(...) because command
# substitution strips trailing newlines and an all-newline body looks empty.
if [[ -z "$(git log --no-merges --pretty=%s "$RANGE" | tr -d '[:space:]')" ]]; then
  exit 0
fi

bump_major=0
bump_minor=0
bump_patch=0

while IFS= read -r line; do
  if [[ "$line" =~ ^(feat|fix|refactor|perf|chore|build|ci|docs|test)(\(.*\))?(!)?: ]]; then
    bang="${BASH_REMATCH[3]}"
    type="${BASH_REMATCH[1]}"
    if [[ -n "$bang" ]]; then
      bump_major=1
    elif [[ "$type" == "feat" ]]; then
      bump_minor=1
    elif [[ "$type" == "fix" ]]; then
      bump_patch=1
    fi
  fi
done < <(git log --no-merges --pretty=%s "$RANGE")

# footer-form breaking change: scan the full body of each commit.
# grep -i matches both `BREAKING CHANGE:` and `BREAKING-CHANGE:`.
if git log --no-merges --pretty=%B "$RANGE" | grep -qi '^BREAKING CHANGE'; then
  bump_major=1
fi

if (( bump_major )); then
  level=major
elif (( bump_minor )); then
  level=minor
elif (( bump_patch )); then
  level=patch
else
  exit 0
fi

IFS='.' read -r major minor patch <<<"$LATEST_VER"

case "$level" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac

echo "v${major}.${minor}.${patch}"
