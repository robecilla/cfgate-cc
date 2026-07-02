# release process

how a merge to `main` becomes a tagged GitHub release. meant for humans
and agents who want to understand (or fix) the flow without reading the
scripts.

## shape

```
PR merged to main
  -> .github/workflows/release.yml
       -> scripts/next-version.sh   (compute tag)
       -> make release              (scripts/release.sh)
            -> build 4 os/arch tarballs
            -> cut GitHub release with tag
            -> update homebrew tap formula (if HOMEBREW_TAP_REPO is set)
```

## the label rule

`scripts/next-version.sh` decides the next version in two steps:

1. **PR labels win.** scans merged PRs since the last tag for one of
   `release:major`, `release:minor`, `release:patch`. max precedence —
   one `release:major` anywhere in the range is a major bump.
2. **fallback: conventional commits.** if no `release:*` label, scans
   commits since the last tag:
   - `feat:` → minor
   - `fix:` → patch
   - `<type>!:` or `BREAKING CHANGE:` footer → major
   - `chore:` / `docs:` / `refactor:` / `perf:` / `test:` / `ci:` /
     `build:` → no bump (release workflow skips)
   - max precedence: `!` and `BREAKING CHANGE:` beat `feat:`; `feat:`
     beats `fix:`.

## why the label rule exists

the fallback gets it wrong sometimes. PR #9 (`feat(proxy): implement
effort mapping`) merged without a `release:*` label, the fallback saw
`feat:` and bumped minor, but the workflow that day ran with stale logic
that published a patch (`v0.2.2`). had the PR carried `release:minor`
the script's label path would have run first and shipped the right
version. fix: anyone (human or agent) opening a PR applies the label
upfront, see `AGENTS.md` for the rule.

## the release step

`scripts/release.sh vX.Y.Z`:

- refuses to run with a dirty working tree
- skips if the tag is already published (idempotent on workflow
  re-runs)
- builds `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`
- writes `shasums` for all tarballs
- `gh release create` (or `upload --clobber` on a re-run)
- if `HOMEBREW_TAP_REPO` is set, clones the tap, rewrites the formula
  with new version + new darwin shas, commits, pushes

## manual recovery

a release can be re-tagged in place if the wrong version shipped —
delete the tag (local + remote), delete the GitHub release, then
`make release TAG=vX.Y.Z` to republish on the same commit. the tag
moves, the artifacts get rebuilt with the new version string baked in
via `-ldflags "-X main.version=$VERSION"`. the homebrew tap (if any)
needs a separate follow-up commit because the shas change.

re-tagging is destructive: anything that grabbed the old version (a
`brew install`, a `curl | sh` snippet, a downstream package) ends up
on a tag that no longer exists. only do it for releases that are very
recent and not yet widely consumed.
