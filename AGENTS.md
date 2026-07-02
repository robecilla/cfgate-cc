# agents

shared instructions for humans and AI tools working in this repo.

## release labels

when you open a PR, **always apply one of these labels** based on the
commits in the branch. `scripts/next-version.sh` reads them with max
precedence and bums the version before tagging the release. without a
`release:*` label the script falls through to a conventional-commits scan
and gets it wrong (e.g. a `feat:` shipped as a `patch` — has happened).

| label | when |
|-------|------|
| `release:major`   | any type with `!` (`feat!:`, `fix!:`…), or a `BREAKING CHANGE:` footer |
| `release:minor`   | `feat:` |
| `release:patch`   | `fix:` |

max precedence: if the branch has both a `feat:` and a `feat!:`, the `!`
wins → `release:major`. same logic for a `BREAKING CHANGE:` footer.

### don't label

`chore`, `docs`, `refactor`, `perf`, `test`, `build`, `ci` produce **no
bump** in the current script. don't apply a `release:*` label to a branch
that's only those — the release workflow will skip it on its own.

### when in doubt, or the rules say the wrong thing

the human label wins. if you genuinely want a `patch` for a `feat:` (or
vice versa), apply the label you want and ignore the table. this is the
override hatch for every weird case.

canonical logic lives in `scripts/next-version.sh` — if this doc and the
script disagree, the script is right; update this file.

## Agent skills

### Issue tracker

GitHub Issues via the `gh` cli. external PRs are not a triage surface. see `docs/agents/issue-tracker.md`.

### Triage labels

the five canonical role names (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`) used verbatim. see `docs/agents/triage-labels.md`.

### Domain docs

single-context: one `CONTEXT.md` + `docs/adr/` at the repo root. see `docs/agents/domain.md`.
