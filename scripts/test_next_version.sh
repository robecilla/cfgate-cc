#!/usr/bin/env bash
# fixtures for scripts/next-version.sh. sources the prod script (via env
# overrides) and asserts the printed tag.
#
# ponytail: 9 fixtures, no test framework. exit non-zero on the first miss.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROD="$SCRIPT_DIR/next-version.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# run the production script with overrides:
#   - LATEST_TAG: pinned to a known prior version (so the test is hermetic
#     — doesn't depend on whatever tag exists in the working repo)
#   - RELEASE_LABELS_FILE: when set, the prod script reads this file's
#     JSON instead of calling `gh`
# strategy: wrap the prod script in a small driver that sets LATEST_TAG
# via a temporary git repo (because the script calls `git describe`).
run_prod() {
  local dir="$1"
  (
    cd "$dir"
    bash "$PROD"
  )
}

# build a scratch repo with: a base tag vX.Y.Z, then a series of commits
# described by the remaining args. prints the dir path on stdout.
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
    printf '%s\n' "$dir"
  )
}

assert_level() {
  local label="$1" expected="$2" dir="$3"
  local actual
  actual="$(run_prod "$dir")"
  if [[ "$actual" != "$expected" ]]; then
    echo "FAIL: $label — expected $expected, got '$actual'"
    exit 1
  fi
  echo "  ok: $label → $actual"
}

assert_no_release() {
  local label="$1" dir="$2"
  local actual
  actual="$(run_prod "$dir" || true)"
  if [[ -n "$actual" ]]; then
    echo "FAIL: $label — expected no release, got '$actual'"
    exit 1
  fi
  echo "  ok: $label → (no release)"
}

# write a labels JSON fixture for the label-path tests.
write_labels() {
  local file="$1"; shift
  printf '%s' "$1" >"$file"
}

echo "running fixtures:"

# ---- commit-scan path (the original 6) --------------------------------

# fixture 1: feat only → minor
d="$(make_repo v1.2.3 "feat: add a thing")"
assert_level "feat only" "v1.3.0" "$d"

# fixture 2: fix only → patch
d="$(make_repo v1.2.3 "fix: address bug")"
assert_level "fix only" "v1.2.4" "$d"

# fixture 3: bang in subject → major
d="$(make_repo v1.2.3 "feat!: rework the api")"
assert_level "feat! bang" "v2.0.0" "$d"

# fixture 4: footer breaking change → major
d="$(make_repo v1.2.3 "$(printf 'refactor: reshape config\n\nBREAKING CHANGE: config file format changed')")"
assert_level "BREAKING CHANGE footer" "v2.0.0" "$d"

# fixture 5: chore only → none
d="$(make_repo v1.2.3 "chore(release): bump" "docs: typo")"
assert_no_release "chore only" "$d"

# fixture 6: mixed → max is major
d="$(make_repo v1.2.3 "fix: tiny" "feat: small" "chore: tidy" "feat(api)!: drop legacy")"
assert_level "mixed, max is major" "v2.0.0" "$d"

# ---- label-override path (the new 3) ---------------------------------

# fixture 7: label=major overrides fix-only commits → major
d="$(make_repo v1.2.3 "fix: trivial")"
LBL="$WORK/labels7.json"
write_labels "$LBL" '[{"labels":[{"name":"release:major"}]}]'
actual="$(cd "$d" && RELEASE_LABELS_FILE="$LBL" bash "$PROD")"
if [[ "$actual" != "v2.0.0" ]]; then
  echo "FAIL: label=major override — expected v2.0.0, got '$actual'"
  exit 1
fi
echo "  ok: label=major overrides fix-only → v2.0.0"

# fixture 8: label=patch overrides feat-only commits → patch
d="$(make_repo v1.2.3 "feat: tiny internal change")"
LBL="$WORK/labels8.json"
write_labels "$LBL" '[{"labels":[{"name":"release:patch"}]}]'
actual="$(cd "$d" && RELEASE_LABELS_FILE="$LBL" bash "$PROD")"
if [[ "$actual" != "v1.2.4" ]]; then
  echo "FAIL: label=patch override — expected v1.2.4, got '$actual'"
  exit 1
fi
echo "  ok: label=patch overrides feat-only → v1.2.4"

# fixture 9: no label, chore-only → no release
d="$(make_repo v1.2.3 "chore: tidy" "docs: typo")"
LBL="$WORK/labels9.json"
write_labels "$LBL" '[]'
actual="$(cd "$d" && RELEASE_LABELS_FILE="$LBL" bash "$PROD" || true)"
if [[ -n "$actual" ]]; then
  echo "FAIL: empty labels + chore-only — expected no release, got '$actual'"
  exit 1
fi
echo "  ok: no label + chore-only → (no release)"

echo "all fixtures passed."
