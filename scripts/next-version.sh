#!/usr/bin/env bash
# prints the next vX.Y.Z tag based on:
#   1. merged PR labels since the last tag (preferred)
#   2. conventional-commits scan (fallback)
# prints nothing and exits 0 if no bump is warranted.
#
# label rules (any single merged PR with one of these wins, max wins):
#   release:major   major
#   release:minor   minor
#   release:patch   patch
#
# commit-scan rules:
#   feat:               minor
#   fix:                patch
#   type!   (subject)   major
#   BREAKING CHANGE:    major  (footer in body)
#   chore/docs/refactor/perf/test/build/ci   none
#   unrecognized                         none
set -euo pipefail

# ponytail: this exists. add a real version tool if bash math ever isn't
# enough (e.g. semver pre-release tags, build metadata, complex ranges).

LATEST="$(git describe --tags --abbrev=0 HEAD^ 2>/dev/null || echo v0.0.0)"
LATEST_VER="${LATEST#v}"
RANGE="${LATEST}..HEAD"

# 1. try PR labels first. works only in a gh-authed env (CI or local
#    `gh auth login`). if `gh` isn't available or fails, fall through.
#    ponytail: tests inject a fixture via RELEASE_LABELS_FILE so the label
#    path stays runnable without mocking `gh`.
level_from_labels() {
  local labels_json

  if [[ -n "${RELEASE_LABELS_FILE:-}" ]]; then
    labels_json="$(cat "$RELEASE_LABELS_FILE" 2>/dev/null || true)"
  else
    if ! command -v gh >/dev/null 2>&1; then
      return 1
    fi
    if ! gh auth status >/dev/null 2>&1; then
      return 1
    fi
    if [[ -z "${GITHUB_REPOSITORY:-}" ]]; then
      return 1
    fi

    local since
    since="$(git log -1 --format=%cI "$LATEST" 2>/dev/null || true)"
    if [[ -z "$since" ]]; then
      return 1
    fi

    labels_json="$(gh pr list \
      --state merged \
      --search "merged:>=$since" \
      --json labels \
      --limit 200 2>/dev/null || true)"
  fi

  if [[ -z "$labels_json" ]] || [[ "$labels_json" == "[]" ]]; then
    return 1
  fi

  # extract release:* label names, dedup, pick max precedence.
  # ponytail: jq if available, else grep over the raw JSON. the JSON shape
  # is stable (gh emits compact JSON with `,"name":"release:major",`).
  local found
  if command -v jq >/dev/null 2>&1; then
    found="$(printf '%s' "$labels_json" | jq -r '.[].labels[].name' 2>/dev/null \
      | grep -E '^release:(major|minor|patch)$' || true)"
  else
    found="$(printf '%s' "$labels_json" \
      | grep -oE '"name":"release:(major|minor|patch)"' \
      | sed -E 's/.*"(release:[a-z]+)".*/\1/' || true)"
  fi
  if [[ -z "$found" ]]; then
    return 1
  fi

  if grep -qx 'release:major' <<<"$found"; then
    echo major; return 0
  fi
  if grep -qx 'release:minor' <<<"$found"; then
    echo minor; return 0
  fi
  if grep -qx 'release:patch' <<<"$found"; then
    echo patch; return 0
  fi
  return 1
}

# 2. fall back: conventional-commits scan. prints the level, or exits 0
#    if the range is empty / no bump-worthy commits.
level_from_commits() {
  # ponytail: empty range short-circuit. don't use $(...) alone because
  # command substitution strips trailing newlines and an all-newline body
  # looks empty.
  if [[ -z "$(git log --no-merges --pretty=%s "$RANGE" | tr -d '[:space:]')" ]]; then
    return 1
  fi

  local bump_major=0 bump_minor=0 bump_patch=0

  while IFS= read -r line; do
    if [[ "$line" =~ ^(feat|fix|refactor|perf|chore|build|ci|docs|test)(\(.*\))?(!)?: ]]; then
      local bang="${BASH_REMATCH[3]}"
      local type="${BASH_REMATCH[1]}"
      if [[ -n "$bang" ]]; then
        bump_major=1
      elif [[ "$type" == "feat" ]]; then
        bump_minor=1
      elif [[ "$type" == "fix" ]]; then
        bump_patch=1
      fi
    fi
  done < <(git log --no-merges --pretty=%s "$RANGE")

  if git log --no-merges --pretty=%B "$RANGE" | grep -qi '^BREAKING CHANGE'; then
    bump_major=1
  fi

  if (( bump_major )); then
    echo major
  elif (( bump_minor )); then
    echo minor
  elif (( bump_patch )); then
    echo patch
  else
    return 1
  fi
}

level="$(level_from_labels || true)"
if [[ -z "${level:-}" ]]; then
  level="$(level_from_commits || true)"
fi
if [[ -z "${level:-}" ]]; then
  exit 0
fi

IFS='.' read -r major minor patch <<<"$LATEST_VER"

case "$level" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac

echo "v${major}.${minor}.${patch}"
