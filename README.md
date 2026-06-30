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
  <a href="https://github.com/robecilla/cfgate-cc">cfgate-cc</a> is a small Go CLI for routing Claude Code and Codex CLI through a Cloudflare AI Gateway (or any openai-compatible upstream) — no manual proxy setup required.
  <br/>
  <br/>
  🤖 <em>Claude Code support.</em>  🧠 <em>Codex CLI support.</em> ⚡ <em>Pluggable upstream.</em> ☁️ <em>Cloudflare AI Gateway ready.</em>
</div>

## why `cfgate-cc`?

`cfgate-cc` is a fork of [emanuelcasco/ocgo](https://github.com/emanuelcasco/ocgo) that replaces the hardcoded opencode-go upstream with a pluggable one. it lets [Claude Code](https://docs.anthropic.com/en/docs/claude-code) and [Codex CLI](https://developers.openai.com/codex/cli/) run against any openai-compatible provider — Cloudflare AI Gateway, OpenRouter, OpenCode Go, or a self-hosted box.

the binary is small (one file, single go module), MIT-licensed, and built on ocgo's proven request/response translation. the only thing we changed is the upstream.

```bash
# 1. point at your gateway (or any openai-compatible URL)
export OCGO_UPSTREAM_BASE_URL="https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/compat/v1"
export OCGO_UPSTREAM_API_KEY="<your-token>"

# 2. start coding!
cfgate-cc launch claude --model workers-ai/@cf/zai-org/glm-5.2
cfgate-cc launch codex --model workers-ai/@cf/zai-org/glm-5.2
```

## upstream config

`cfgate-cc` reads the upstream from `~/.config/ocgo/config.json` (ocgo-compatible path), or from `OCGO_UPSTREAM_*` env vars which take precedence.

```json
{
  "upstream_base_url": "https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/compat/v1",
  "upstream_api_key": "<your-token>",
  "upstream_auth": "bearer"
}
```

| field | env var | notes |
|---|---|---|
| `upstream_base_url` | `OCGO_UPSTREAM_BASE_URL` | the openai-compat endpoint. `/chat/completions` is appended automatically. |
| `upstream_api_key` | `OCGO_UPSTREAM_API_KEY` | bearer / x-api-key value |
| `upstream_auth` | `OCGO_UPSTREAM_AUTH` | `bearer` (default), `x-api-key`, or `header` |
| `upstream_auth_hdr` | `OCGO_UPSTREAM_AUTH_HDR` | header name when `upstream_auth=header` |
| `upstream_extra_hdr` | `OCGO_UPSTREAM_EXTRA_HDR` | extra headers as json object |
| `endpoint_overrides` | — | glob → route mapping, e.g. `[{ "pattern": "claude-*", "route": "anthropic" }]` |

the env-var pattern means your fish alias can override everything per-call without touching the config file.

## cloudflare ai gateway example

```bash
# one-line fish alias
alias claude-cf 'OCGO_UPSTREAM_BASE_URL=https://gateway.ai.cloudflare.com/v1/<account>/<gateway>/compat/v1 OCGO_UPSTREAM_API_KEY=<token> cfgate-cc launch claude --model workers-ai/@cf/zai-org/glm-5.2 -- $argv'

claude-cf "echo hi"
```

the model id for cloudflare workers-ai must be prefixed with `workers-ai/`, e.g. `workers-ai/@cf/zai-org/glm-5.2`. just `@cf/...` returns `Invalid provider`. the full list of available cloudflare ai gateway model ids lives in the [cloudflare ai models docs](https://developers.cloudflare.com/ai/models/index.md).

## opencode-go (ocgo compat)

leave `upstream_base_url` unset to use the original opencode-go URL. the qwen/minimax/kimi anthropic-endpoint heuristic from ocgo is preserved — empty `endpoint_overrides` means those models still hit `/messages` and everything else hits `/chat/completions`.

```bash
cfgate-cc launch claude --model kimi-k2.6
```

## side-by-side with ocgo

set `CFGATE_CC_CONFIG_DIR` to keep cfgate-cc's config separate from a running ocgo install:

```bash
CFGATE_CC_CONFIG_DIR=~/.config/cfgate-cc cfgate-cc serve -b
```

defaults to `~/.config/ocgo` for ocgo compat.

## features

- routes claude code through any openai-compatible upstream
- routes codex cli through any openai-compatible upstream
- pluggable auth: bearer, x-api-key, or arbitrary header
- extra headers (e.g. `cf-aig-authorization`) via `upstream_extra_hdr`
- per-model endpoint routing via `endpoint_overrides` (glob → openai or anthropic)
- model mapping (claude → upstream model, codex → upstream model)
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
