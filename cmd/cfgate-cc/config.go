package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ModelEndpointOverride lets a model id (matched as glob) pick its upstream
// endpoint: "openai" (chat/completions) or "anthropic" (/messages). empty list
// = everything routes to openai. ThinkingBudgetMax (optional) overrides every
// per-model thinking budget entry for matching models — useful when a single
// deployment needs a different cap than the in-code table.
type ModelEndpointOverride struct {
	Pattern           string `json:"pattern"`
	Route             string `json:"route"`
	ThinkingBudgetMax *int   `json:"thinking_budget_max,omitempty"`
}

// Instance is the per-instance config: bind address + one provider config
// + tool→source→target model mapping. one file on disk (config.json), one
// struct in memory. replaces the old {config.json, <provider>.json,
// model-mapping.json, active-provider} tuple.
type Instance struct {
	Host         string         `json:"host"`
	Port         int            `json:"port"`
	Provider     ProviderConfig `json:"provider"`
	ModelMapping ModelMapping   `json:"model_mapping"`
}

// ProviderConfig is the per-provider upstream config shared by every
// provider. cloudflare-only concerns (Gateway, Account, OpenAINative)
// live in the Cloudflare sub-struct and are only populated for the
// cloudflare provider.
type ProviderConfig struct {
	Name              string                  `json:"name"`
	UpstreamBaseURL   string                  `json:"upstream_base_url"`
	UpstreamAPIKey    string                  `json:"upstream_api_key"`
	UpstreamAuth      string                  `json:"upstream_auth"`
	UpstreamAuthHdr   string                  `json:"upstream_auth_hdr,omitempty"`
	UpstreamExtraHdr  map[string]string       `json:"upstream_extra_hdr,omitempty"`
	EndpointOverrides []ModelEndpointOverride `json:"endpoint_overrides,omitempty"`
	Cloudflare        *CloudflareOptions      `json:"cloudflare,omitempty"`
}

// CloudflareOptions carries cloudflare-specific routing concerns. nil
// for non-cloudflare providers; consumers must nil-check before reading.
type CloudflareOptions struct {
	Gateway      string `json:"gateway,omitempty"`
	Account      string `json:"account,omitempty"`
	OpenAINative bool   `json:"openai_native,omitempty"`
}

// ModelMapping is tool → source-model → upstream-target. replaces the
// old per-provider nested shape now that one instance has exactly one
// provider.
type ModelMapping map[string]map[string]string

// knownProviders is the fixed enum. add a new provider = add a setup
// subcommand + a row in the file. nothing else.
var knownProviders = []string{"opencode-go", "cloudflare"}

func isKnownProvider(name string) bool {
	return slices.Contains(knownProviders, name)
}

// providerForUpstreamURL picks the provider name from a URL pattern.
// cloudflare AI gateway urls are recognised by host prefix; anything else
// (including empty) maps to opencode-go.
func providerForUpstreamURL(url string) string {
	if strings.HasPrefix(url, "https://api.cloudflare.com/client/v4/accounts/") {
		return "cloudflare"
	}
	if strings.HasPrefix(url, "https://gateway.ai.cloudflare.com/v1/") {
		return "cloudflare"
	}
	return "opencode-go"
}

// openAIURL builds the upstream openai-compatible chat/completions URL
// from cfg.UpstreamBaseURL. Falls back to the opencode-go default when no
// upstream is configured, preserving the original ocgo behavior.
func openAIURL(cfg ProviderConfig) string {
	if cfg.UpstreamBaseURL != "" {
		return strings.TrimRight(cfg.UpstreamBaseURL, "/") + "/chat/completions"
	}
	return "https://opencode.ai/zen/go/v1/chat/completions"
}

// openAINativeURL builds the Cloudflare AI Gateway native /openai provider
// URL — a transparent pass-through to OpenAI (the /ai/v1 compat adapter
// sends max_tokens which gpt-5.x rejects). only valid when cfg.Cloudflare
// is non-nil and OpenAINative is set.
func openAINativeURL(cfg ProviderConfig) string {
	if cfg.Cloudflare == nil {
		return ""
	}
	return fmt.Sprintf("https://gateway.ai.cloudflare.com/v1/%s/%s/openai/chat/completions", cfg.Cloudflare.Account, cfg.Cloudflare.Gateway)
}

// openAIURLForModel picks the right upstream URL for a model in flight.
// workers-ai @cf/... models always use the legacy /ai/v1 path (the compat
// adapter speaks the same chat/completions shape and routes to workers-ai
// via cf-aig-gateway-id header). other models on a native-enabled config
// use the /openai provider endpoint.
func openAIURLForModel(cfg ProviderConfig, model string) string {
	if cfg.Cloudflare != nil && cfg.Cloudflare.OpenAINative && !strings.HasPrefix(modelID(model), "@cf/") {
		return openAINativeURL(cfg)
	}
	return openAIURL(cfg)
}

// anthropicURL is the upstream anthropic-compatible /messages URL, used
// only for models routed via endpoint_overrides with route=anthropic.
func anthropicURL(cfg ProviderConfig) string {
	if cfg.UpstreamBaseURL != "" {
		return strings.TrimRight(cfg.UpstreamBaseURL, "/") + "/messages"
	}
	return "https://opencode.ai/zen/go/v1/messages"
}

// buildCloudflareURL composes the compat /ai/v1 URL for an account id.
// ponytail: only one call site (legacy migration); no need to lift.
func buildCloudflareURL(account string) string {
	return "https://api.cloudflare.com/client/v4/accounts/" + account + "/ai/v1"
}

// --- path helpers ---

func configDir() string {
	if d := os.Getenv("CFGATE_CC_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cfgate-cc")
}

// instanceDir returns the per-instance state directory. empty name
// returns configDir() itself so callers can use one ternary: when name
// is set, state lives at <configDir>/instances/<name>/; when not, state
// lives at configDir() as today. ponytail: no separate global-vs-instance
// code paths — the instance dir collapses to configDir() in legacy mode.
func instanceDir(name string) string {
	if name == "" {
		return configDir()
	}
	return filepath.Join(configDir(), "instances", name)
}

func instanceConfigFile(name string) string {
	return filepath.Join(instanceDir(name), "config.json")
}

func instancePidFile(name string) string {
	return filepath.Join(instanceDir(name), "cfgate-cc.pid")
}

func instanceActiveProviderFile(name string) string {
	return filepath.Join(instanceDir(name), "active-provider")
}

func instanceLogFile(name string) string {
	return filepath.Join(instanceDir(name), "cfgate-cc.log")
}

func instancePortFile(name string) string {
	return filepath.Join(instanceDir(name), "port")
}
