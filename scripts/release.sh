#!/usr/bin/env bash
set -euo pipefail

APP_NAME="${APP_NAME:-cfgate-cc}"
CMD_PATH="${CMD_PATH:-./cmd/cfgate-cc}"
TAG="${1:-${TAG:-}}"

if [[ -z "$TAG" ]]; then
  echo "Usage: $0 v0.1.0"
  echo "   or: TAG=v0.1.0 make release"
  exit 1
fi

VERSION="${TAG#v}"
REPO="${GITHUB_REPOSITORY:-robecilla/cfgate-cc}"
if [[ -z "$REPO" ]]; then
  origin_url="$(git config --get remote.origin.url || true)"
  if [[ "$origin_url" =~ github.com[:/]([^/]+)/([^/.]+)(\.git)?$ ]]; then
    REPO="${BASH_REMATCH[1]}/${BASH_REMATCH[2]}"
  else
    echo "Set GITHUB_REPOSITORY=owner/repo, or configure a GitHub origin remote."
    exit 1
  fi
fi

# homebrew tap is optional. leave HOMEBREW_TAP_REPO unset to skip the
# formula update step entirely (e.g. on a first release before the tap
# repo exists). to enable, create the tap repo and set e.g.
# HOMEBREW_TAP_REPO=robecilla/homebrew-tap.
HOMEBREW_TAP_REPO="${HOMEBREW_TAP_REPO:-}"

if ! command -v gh >/dev/null 2>&1; then
  echo "GitHub CLI is required: brew install gh && gh auth login"
  exit 1
fi

if ! gh auth status >/dev/null 2>&1; then
  echo "GitHub CLI is not authenticated. Run: gh auth login"
  exit 1
fi

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required."
  exit 1
fi

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "Working tree has uncommitted changes. Commit or stash them first."
  exit 1
fi

# Verify the project builds/tests before tagging.
go test ./...

if ! git rev-parse "$TAG" >/dev/null 2>&1; then
  git tag -a "$TAG" -m "$TAG"
fi

git push origin "$TAG"

DIST_DIR="dist"
rm -rf "$DIST_DIR"
mkdir -p "$DIST_DIR"

build_one() {
  local goos="$1"
  local goarch="$2"
  local arch_name="$goarch"
  if [[ "$goarch" == "amd64" ]]; then
    arch_name="x86_64"
  fi

  local dir="$DIST_DIR/${APP_NAME}_${VERSION}_${goos}_${arch_name}"
  mkdir -p "$dir"

  local bin="$APP_NAME"
  if [[ "$goos" == "windows" ]]; then
    bin="$APP_NAME.exe"
  fi

  echo "Building $goos/$goarch..."
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$dir/$bin" "$CMD_PATH"

  cp README.md "$dir/" 2>/dev/null || true
  cp LICENSE "$dir/" 2>/dev/null || true

  tar -C "$DIST_DIR" -czf "$dir.tar.gz" "$(basename "$dir")"
  rm -rf "$dir"
}

build_one darwin amd64
build_one darwin arm64
build_one linux amd64
build_one linux arm64

(
  cd "$DIST_DIR"
  shasum -a 256 *.tar.gz > checksums.txt
)

if gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
  echo "GitHub release $TAG already exists; uploading artifacts with --clobber."
  gh release upload "$TAG" "$DIST_DIR"/*.tar.gz "$DIST_DIR/checksums.txt" --repo "$REPO" --clobber
else
  gh release create "$TAG" "$DIST_DIR"/*.tar.gz "$DIST_DIR/checksums.txt" \
    --repo "$REPO" \
    --title "$TAG" \
    --generate-notes
fi

# Update Homebrew tap formula to install the macOS artifacts.
# skipped when HOMEBREW_TAP_REPO is unset.
if [[ -n "$HOMEBREW_TAP_REPO" ]]; then
  TAP_TMP="$(mktemp -d)"
  trap 'rm -rf "$TAP_TMP"' EXIT

  gh repo clone "$HOMEBREW_TAP_REPO" "$TAP_TMP" -- --quiet
  mkdir -p "$TAP_TMP/Formula"

  DARWIN_ARM_SHA="$(shasum -a 256 "$DIST_DIR/${APP_NAME}_${VERSION}_darwin_arm64.tar.gz" | awk '{print $1}')"
  DARWIN_AMD_SHA="$(shasum -a 256 "$DIST_DIR/${APP_NAME}_${VERSION}_darwin_x86_64.tar.gz" | awk '{print $1}')"

  cat > "$TAP_TMP/Formula/${APP_NAME}.rb" <<EOF_FORMULA
class CfgateCc < Formula
  desc "Proxy Claude Code and Codex CLI through a pluggable openai-compatible upstream"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "MIT"

  on_macos do
    if Hardware::CPU.arm?
      url "https://github.com/${REPO}/releases/download/${TAG}/${APP_NAME}_${VERSION}_darwin_arm64.tar.gz"
      sha256 "${DARWIN_ARM_SHA}"
    else
      url "https://github.com/${REPO}/releases/download/${TAG}/${APP_NAME}_${VERSION}_darwin_x86_64.tar.gz"
      sha256 "${DARWIN_AMD_SHA}"
    end
  end

  def install
    bin.install "${APP_NAME}"
  end

  test do
    system "#{bin}/${APP_NAME}", "--help"
  end
end
EOF_FORMULA

  (
    cd "$TAP_TMP"
    git add "Formula/${APP_NAME}.rb"
    if git diff --cached --quiet; then
      echo "Homebrew formula is already up to date."
    else
      git commit -m "Update ${APP_NAME} to ${TAG}"
      git push
    fi
  )

  TAP_OWNER="${HOMEBREW_TAP_REPO%%/*}"
  TAP_REPO_NAME="${HOMEBREW_TAP_REPO#*/}"
  TAP_NAME="${TAP_REPO_NAME#homebrew-}"

  echo "Install with: brew install ${TAP_OWNER}/${TAP_NAME}/${APP_NAME}"
fi

echo "Release complete: https://github.com/${REPO}/releases/tag/${TAG}"
