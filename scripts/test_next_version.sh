#!/usr/bin/env bash
# fixtures for scripts/next-version.sh. each block creates a scratch git
# repo, lays down a base tag, adds commits, sources the script's logic
# (without re-implementing it), and asserts the printed tag matches.
#
# ponytail: 6 fixtures, no test framework. exit non-zero on the first miss.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# a function that mirrors the body of next-version.sh, parameterized by
# RANGE so the test can drive it against a synthetic commit log.
compute_next() {
  local dir="$1" range="$2"
  local bump_major=0 bump_minor=0 bump_patch=0

  (
    cd "$dir"
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
    done < <(git log --no-merges --pretty=%s "$range")

    if git log --no-merges --pretty=%B "$range" | grep -qi '^BREAKING CHANGE'; then
      bump_major=1
    fi

    if (( bump_major )); then
      echo major
    elif (( bump_minor )); then
      echo minor
    elif (( bump_patch )); then
      echo patch
    else
      echo none
    fi
  )
}

# build a scratch repo with: a base tag vX.Y.Z, then a series of commits
# described by the remaining args. each arg is a commit message.
# prints the range (BASE..HEAD) on stdout.
make_repo() {
  local base="$1"; shift
  local dir="$WORK/repo-$$-$RANDOM"
  mkdir -p "$dir"
  (
    cd "$dir"
    git init -q -b main
    git config user.email "t@t"
    git config user.name "t"
    echo a > f && git add f && git commit -q -m "chore: seed"
    git tag "$base"
    for msg in "$@"; do
      echo x >> f && git add f && git commit -q -m "$msg"
    done
    echo "${base}..HEAD"
    echo "$dir" >"$WORK/lastdir"
  )
}

assert_level() {
  local label="$1" expected="$2" range="$3"
  local dir actual
  dir="$(cat "$WORK/lastdir")"
  actual="$(compute_next "$dir" "$range")"
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL: $label — expected $expected, got $actual"
    exit 1
  fi
  echo "  ok: $label → $actual"
}

echo "running fixtures:"

# fixture 1: feat only → minor
r="$(make_repo v1.2.3 "feat: add a thing")"
assert_level "feat only" minor "$r"

# fixture 2: fix only → patch
r="$(make_repo v1.2.3 "fix: address bug")"
assert_level "fix only" patch "$r"

# fixture 3: bang in subject → major
r="$(make_repo v1.2.3 "feat!: rework the api")"
assert_level "feat! bang" major "$r"

# fixture 4: footer breaking change → major
r="$(make_repo v1.2.3 "$(printf 'refactor: reshape config\n\nBREAKING CHANGE: config file format changed')")"
assert_level "BREAKING CHANGE footer" major "$r"

# fixture 5: chore only → none
r="$(make_repo v1.2.3 "chore(release): bump" "docs: typo")"
assert_level "chore only" none "$r"

# fixture 6: mixed → max is major
r="$(make_repo v1.2.3 "fix: tiny" "feat: small" "chore: tidy" "feat(api)!: drop legacy")"
assert_level "mixed, max is major" major "$r"

echo "all fixtures passed."
