<h1 align="center">cfgate-cc</h1>
<div align="center">
  <a href="https://github.com/robecilla/cfgate-cc/releases">
    <img alt="GitHub release" src="https://img.shields.io/github/v/release/robecilla/cfgate-cc?color=blue">
  </a>
  <a href="https://github.com/robecilla/cfgate-cc/blob/main/LICENSE">
    <img alt="GitHub license" src="https://img.shields.io/github/license/robecilla/cfgate-cc">
  </a>
  <a href="https://go.dev/doc/go1.22">
    <img alt="Go version" src="https://img.shields.io/github/go-mod/go-version/robecilla/cfgate-cc">
  </a>
</div>

<br/>

<div align="center">
  <a href="https://github.com/robecilla/cfgate-cc">cfgate-cc</a> is a small Go CLI for routing Claude Code and Codex CLI through Cloudflare AI Gateway (or any openai-compatible upstream) — no manual proxy setup required.
  <br/>
  <br/>
  🤖 <em>Claude Code support.</em>  🧠 <em>Codex CLI support.</em> ⚡ <em>Pluggable upstream.</em>
</div>

## why `cfgate-cc`?

`cfgate-cc` is a fork of [emanuelcasco/ocgo](https://github.com/emanuelcasco/ocgo) that replaces the hardcoded opencode-go upstream with a pluggable one. it lets [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and [Codex CLI](https://developers.openai.com/codex/cli/) run against any openai-compatible provider — Cloudflare AI Gateway, OpenRouter, OpenCode Go, or a self-hosted box.

the binary is small (one file, single go module), MIT-licensed, and built on ocgo's proven request/response translation. the only thing we changed is the upstream.

```bash
# 1. configure an upstream
cfgate-cc setup cloudflare --token <token> --account <account> --gateway <gateway>
# or
cfgate-cc setup opencode-go --api-key <key>

# 2. start coding!
cfgate-cc launch claude --provider cloudflare --model workers-ai/@cf/zai-org/glm-5.2
cfgate-cc launch codex  --provider cloudflare --model workers-ai/@cf/zai-org/glm-5.2
```

if you'd rather keep the env-var style, `OCGO_UPSTREAM_BASE_URL` / `OCGO_UPSTREAM_API_KEY` still override the active provider's file at request time. with no provider configured yet, you must still tell cfgate-cc which upstream to use — pass `--provider` or set `$OCGO_PROVIDER`, the env vars alone don't pick one.

## upstream config

`cfgate-cc` keeps one config file per upstream provider under `~/.config/ocgo/` (override with `CFGATE_CC_CONFIG_DIR`):

| file | written by | purpose |
|---|---|---|
| `config.json` | — | local proxy settings: `host`, `port` |
| `opencode-go.json` | `setup opencode-go` | opencode-go / zen upstream |
| `cloudflare.json` | `setup cloudflare` | cloudflare ai gateway upstream |

`OCGO_UPSTREAM_*` env vars still work and override the active provider's file at request time. set `$OCGO_PROVIDER` (or pass `--provider`) to pick which one wins when both are configured.

```json5
// ~/.config/ocgo/cloudflare.json
{
  "upstream_base_url": "https://api.cloudflare.com/client/v4/accounts/<account>/ai/v1",
  "upstream_api_key": "<your-token>",
  "upstream_auth": "bearer",
  "gateway": "<your-gateway-id>"
}
```

| field | env var (overrides file) | notes |
|---|---|---|
| `upstream_base_url` | `OCGO_UPSTREAM_BASE_URL` | the cloudflare REST API endpoint. `/chat/completions` is appended automatically. |
| `upstream_api_key` | `OCGO_UPSTREAM_API_KEY` | bearer / x-api-key value |
| `upstream_auth` | `OCGO_UPSTREAM_AUTH` | `bearer` (default), `x-api-key`, `header`, or `both` (sends `Authorization: Bearer` *and* `x-api-key` — needed for opencode-go, whose `/chat/completions` and `/v1/messages` disagree on the header) |
| `upstream_auth_hdr` | `OCGO_UPSTREAM_AUTH_HDR` | header name when `upstream_auth=header` |
| `upstream_extra_hdr` | `OCGO_UPSTREAM_EXTRA_HDR` | extra headers as json object |
| `gateway` | — | cloudflare AI gateway id. sent as `cf-aig-gateway-id` only for workers-ai (`@cf/...`) models; third-party gateway models use the default gateway and don't need the header. |
| `endpoint_overrides` | — | glob → route mapping, e.g. `[{ "pattern": "claude-*", "route": "anthropic" }]` |

## provider selection

resolution order (first match wins):

1. `--provider` flag on `launch` / `serve` / `status` / `list` / `mapping`
2. `$OCGO_PROVIDER` env var
3. the single configured provider (if exactly one of `opencode-go.json` / `cloudflare.json` exists)
4. error if multiple providers are configured and nothing else pins one down

```bash
cfgate-cc launch claude --provider cloudflare --model workers-ai/@cf/zai-org/glm-5.2
cfgate-cc launch codex --provider opencode-go --model kimi-k2.6
OCGO_PROVIDER=cloudflare cfgate-cc serve -b
cfgate-cc list --provider cloudflare
cfgate-cc mapping --provider cloudflare claude set claude-opus workers-ai/@cf/zai-org/glm-5.2
```

`list` and `mapping` both accept `--provider` — pass it on the leaf subcommand you actually want it for. (it's a local flag on `list` and a persistent flag on `mapping`, so behaviour matches but the cobra wiring differs.)

## model mapping

`cfgate-cc` lets you remap the model id claude/codex sends to a different upstream model. the file is per-provider, so the same source model can map to different upstreams depending on which provider is active:

```json5
// ~/.config/ocgo/model-mapping.json
{
  "opencode-go": {
    "claude": { "claude-opus": "minimax-m3", "claude-sonnet": "qwen3.7-max" },
    "codex":  { "gpt-5": "deepseek-v4-pro" }
  },
  "cloudflare": {
    "claude": { "claude-opus": "workers-ai/@cf/zai-org/glm-5.2" },
    "codex":  {}
  }
}
```

```bash
cfgate-cc mapping --provider opencode-go claude set claude-opus minimax-m3
cfgate-cc mapping --provider cloudflare  claude set claude-opus workers-ai/@cf/zai-org/glm-5.2
cfgate-cc mapping --provider opencode-go claude show
cfgate-cc mapping --provider opencode-go claude get  claude-opus
cfgate-cc mapping --provider opencode-go claude unset claude-opus   # aliases: rm, remove, delete
cfgate-cc mapping open       # open the mapping file in $EDITOR
```

### migrating mappings from a pre-split config

the mapping file used to be tool-scoped (`{"claude": {...}, "codex": {...}}`). it's now per-provider. on first read for a known provider, the legacy entries are lifted into that provider's section in place and the file is rewritten in the new shape — your existing mappings carry over, no manual `mapping set` re-run needed:

```
cfgate-cc: migrated legacy model-mapping.json into "opencode-go" section (one-time)
```

the migration needs an active provider. with multiple providers configured and no `--provider` / `$OCGO_PROVIDER`, the legacy file is left alone and a one-shot warning fires:

> warning: `~/.config/ocgo/model-mapping.json` is in the old tool-scoped format. run `cfgate-cc mapping --provider <name> <tool> set ...` to migrate per provider.

the warning is gated by a sentinel at `~/.config/ocgo/model-mapping.migrated` — it fires once per user, not once per process.

## cloudflare ai gateway example

```bash
# one-time setup (writes ~/.config/ocgo/cloudflare.json)
cfgate-cc setup cloudflare --token <token> --account <account> --gateway <gateway>
# each flag also reads from $CLOUDFLARE_API_TOKEN, $CLOUDFLARE_ACCOUNT_ID,
# $CLOUDFLARE_GATEWAY_ID respectively. pass --force to overwrite an existing
# config pointing at a different account.

# use it
cfgate-cc launch claude --provider cloudflare --model workers-ai/@cf/zai-org/glm-5.2
# launch flags:
#   --yes    pass --dangerously-skip-permissions to claude
#   --config (codex only) write the codex profile but don't launch codex
```

or, if you'd rather keep the fish-alias style, `OCGO_UPSTREAM_*` still overrides the file:

```bash
alias claude-cf 'OCGO_PROVIDER=cloudflare cfgate-cc launch claude --model workers-ai/@cf/zai-org/glm-5.2 -- $argv'

claude-cf "echo hi"
```

the model id for cloudflare workers-ai accepts both `workers-ai/@cf/...` and bare `@cf/...` — the `workers-ai/` prefix is stripped automatically. the full list of available cloudflare ai gateway model ids lives in the [cloudflare ai models docs](https://developers.cloudflare.com/ai/models/index.md).

## opencode-go (ocgo compat)

```bash
# one-time setup (writes ~/.config/ocgo/opencode-go.json)
cfgate-cc setup opencode-go --api-key <key>
# falls back to $OCGO_API_KEY when --api-key is omitted.

# use it
cfgate-cc launch claude --provider opencode-go --model kimi-k2.6
```

leave `upstream_base_url` unset to use the original opencode-go URL. the qwen/minimax anthropic-endpoint heuristic from ocgo is preserved — empty `endpoint_overrides` means `qwen3.7-max` and `minimax-m3` / `minimax-m2.7` / `minimax-m2.5` hit `/messages` and everything else hits `/chat/completions`. (kimi does not use the anthropic endpoint.)

## migrating from a pre-split config

if you have an older `config.json` with the `upstream_*` fields inline, the first `launch` / `serve` / `status` after upgrading moves them into the right provider file:

- `upstream_base_url` matching `https://gateway.ai.cloudflare.com/v1/...` or `https://api.cloudflare.com/client/v4/accounts/...` → `cloudflare.json`
- anything else (opencode-go URL, or just an `upstream_api_key` with no URL) → `opencode-go.json`

it's a one-shot, runs in `loadConfig`, and is a no-op once the upstream fields are gone from `config.json`. if a provider file already exists for the target, the migration leaves your `config.json` alone — you have two configs to reconcile, do it by hand.

## side-by-side with ocgo

set `CFGATE_CC_CONFIG_DIR` to keep cfgate-cc's config separate from a running ocgo install:

```bash
CFGATE_CC_CONFIG_DIR=~/.config/cfgate-cc cfgate-cc serve -b
```

defaults to `~/.config/ocgo` for ocgo compat.

## features

- routes claude code through any openai-compatible upstream
- routes codex cli through any openai-compatible upstream
- pluggable auth: bearer, x-api-key, arbitrary header, or both (for opencode-go)
- extra headers (e.g. `cf-aig-authorization`) via `upstream_extra_hdr`
- per-model endpoint routing via `endpoint_overrides` (glob → openai or anthropic)
- per-provider model mapping (claude → upstream model, codex → upstream model)
- streaming text and tool-call translation
- ocgo-compatible: same proxy lifecycle, same codex profile management, same env vars

## installation

build from source:

```bash
git clone https://github.com/robecilla/cfgate-cc.git
cd cfgate-cc
make install
```

binary lands in `~/go/bin/cfgate-cc` (or `$GOBIN` if set).

## development

```bash
make build      # builds ./bin/cfgate-cc
make test       # runs all tests
make run        # go run ./cmd/cfgate-cc
make clean
```

requirements: go 1.22+, cobra (vendored via go.mod).

## license

MIT. see [LICENSE](LICENSE).

this project is a fork of [emanuelcasco/ocgo](https://github.com/emanuelcasco/ocgo) — original copyright by Emanuel Casco, MIT licensed. the request/response translation, codex profile management, and cobra command tree are all from ocgo. the only thing we changed is making the upstream pluggable so the same binary can target cloudflare ai gateway or any other openai-compatible provider.
