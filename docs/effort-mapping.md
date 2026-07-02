# effort mapping

how `cfgate-cc` translates the user-facing effort / reasoning / thinking knobs
claude code and codex send into the upstream `thinking` shape on the
anthropic-compatible `/v1/messages` path, and the equivalent on the
chat-completions path.

## why

opencode-go's anthropic-compatible endpoint is stricter than real claude: it
rejects `reasoning`, `reasoning_effort`, `effort`, `level`, `depth`, and
`output_config` outright ("Request body format invalid"). but it does accept
`thinking: {type: enabled, budget_tokens: N}`. the proxy has to translate
the user-facing shape into the only shape the upstream takes, and it has to
pick a sensible budget for the resolved level — that's what the per-model
table is for.

## input shapes (what clients send)

claude code + codex send effort under one of these top-level fields on the
`AnthropicRequest`:

- `thinking` — `{type: enabled, budget_tokens: N}` or `{type: disabled}`
- `reasoning` — `{effort: "high"}` (nested)
- `reasoning_effort` — bare string, e.g. `"high"`
- `effort` — bare string, e.g. `"max"`
- `level` — bare string or number, e.g. `"2"` or `4`
- `depth` — nested object, e.g. `{"level": "high"}`
- `output_config` — nested, e.g. `{"reasoning": {"effort": "medium"}}` or
  `{"reasoning": {"depth": 2}}`

`anthropicThinkingForRequest` walks them in that order and takes the first
non-empty value. the walker is `reasoningEffortFromRaw` (reused from the
chat path) — same shape parsing, so anything claude code or codex can send
on either path is handled here.

## level resolution

the raw value runs through `resolveEffortLevel` (next to
`normalizeReasoningEffort` in `cmd/cfgate-cc/main.go`). the difference from
`normalizeReasoningEffort` is that `max` (and `xhigh`, `4`, `maximum`) is its
own bucket, so the budget table can give it a separate entry.

| raw input        | resolved level |
|------------------|----------------|
| `0` `minimal` `min` `none` `off` `disabled` `false` | `minimal` |
| `1` `low` `light` | `low` |
| `2` `medium` `med` `normal` `default` | `medium` |
| `3` `high` `deep` `true` `enabled` | `high` |
| `4` `xhigh` `max` `maximum` | `max` |
| anything else    | returned verbatim (then table lookup returns nil) |

`""` and `"minimal"` short-circuit to no thinking field at all.

## per-model budget table

defined in `modelMetadata` (`cmd/cfgate-cc/main.go`) on
`openCodeModelMetadata.ThinkingBudget`. keyed by the resolved level:

| model             | minimal | low  | medium | high  | max    |
|-------------------|---------|------|--------|-------|--------|
| `qwen3.7-max`     | 0       | 2048 | 4096   | 8192  | 16384  |
| `qwen3.7-plus`    | 0       | 2048 | 4096   | 8192  | 16384  |
| `qwen3.6-plus`    | 0       | 2048 | 4096   | 8192  | 16384  |
| `qwen3.5-plus`    | 0       | 2048 | 4096   | 8192  | 16384  |
| `minimax-m2.5`    | 0       | 2048 | 4096   | 8192  | 16384  |
| `minimax-m2.7`    | 0       | 4096 | 8192   | 16384 | 32768  |
| `minimax-m3`      | 0       | 4096 | 8192   | 16384 | 32768  |
| `glm-5.2`         | 0       | 2048 | 4096   | 8192  | 16384  |
| `kimi-k2.7-code`  | 0       | 2048 | 4096   | 8192  | 16384  |

`glm-5.2` and `kimi-k2.7-code` are cloudflare workers-ai models
(`@cf/zai-org/glm-5.2`, `@cf/moonshotai/kimi-k2.7-code`) routed via
`/v1/messages` through the cloudflare AI gateway. the gateway id rides on
`cf-aig-gateway-id` (set by `applyCloudflareGatewayHeader` when the wire
model starts with `@cf/`), and the cloudflare AI gateway translates the
anthropic request to the underlying workers-ai model. same table, same
helper — no special-casing.

`0` budget for a level means "no thinking field for this level". a model
that's missing from the table (e.g. `kimi-k2.6`, `mimo-v2-omni`) gets no
thinking field at all — the request goes through the chat-completions path
instead, with `reasoning_effort` forwarded raw.

## override

`ModelEndpointOverride.ThinkingBudgetMax` (`cmd/cfgate-cc/main.go`, glob
match via `path.Match` on `modelID(model)`) replaces the table value for any
matching model:

```json
{
  "endpoint_overrides": [
    { "pattern": "qwen3.7-max", "thinking_budget_max": 4096 }
  ]
}
```

`thinking_budget_max: 0` is the escape hatch: even when the user requested
thinking, the matched model gets no `thinking` field. `modelThinkingBudgetMax`
returns `(int, bool)` — the bool says "a glob matched", not "the value is
nonzero", so a 0 override is distinguishable from "no override configured".

## output shape

the upstream field is the canonical anthropic thinking shape:

```json
"thinking": {"type": "enabled", "budget_tokens": 8192}
```

the helper emits this with a struct (not a `map[string]any`) so the JSON
key order is deterministic — `json.Marshal` on a map sorts keys
alphabetically, which would put `budget_tokens` before `type`.

## chat-completions path (non-anthropic models)

`kimi-k2.6`, `mimo-v2-omni`, `deepseek-v4-flash`, and other non-anthropic-routed
models take the chat-completions path. `applyRawChatReasoningEffort` keeps
only `reasoning_effort` on the body and drops `reasoning`, `thinking`,
`effort`, `level`, `depth`, `output_config`. the proxy uses
`resolveEffortLevel` (same as the anthropic path) so `max`/`xhigh` reach
the upstream as `max` rather than collapsing to `high`. per models.dev,
`deepseek-v4-flash`, `deepseek-v4-pro`, and `glm-5.2` explicitly advertise
`max` in their `reasoning_options`; for other chat-completions models the
upstream decides whether to accept it, and a rejected `max` falls back to
`--effort high`. no budget table on this path — the upstream picks its own
budget.

cloudflare `@cf/...` workers-ai chat models follow the same path: the body
keeps `reasoning_effort`, and the `cf-aig-gateway-id` header is set by
`applyCloudflareGatewayHeader` at the request layer, not the body layer.
