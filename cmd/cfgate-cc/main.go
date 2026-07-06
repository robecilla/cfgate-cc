package main

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	appName                            = "cfgate-cc"
	defaultHost                        = "127.0.0.1"
	defaultPort                        = 3456
	codexProfileName                   = "cfgate-cc-launch"
	maxAnthropicToolResultContentChars = 120000
)

var version = "dev"

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
// sends max_tokens which gpt-5.x rejects). only valid when cfg.OpenAINative
// is set and cfg.Account + cfg.Gateway are populated.
func openAINativeURL(cfg ProviderConfig) string {
	return fmt.Sprintf("https://gateway.ai.cloudflare.com/v1/%s/%s/openai/chat/completions", cfg.Account, cfg.Gateway)
}

// openAIURLForModel picks the right upstream URL for a model in flight.
// workers-ai @cf/... models always use the legacy /ai/v1 path (the compat
// adapter speaks the same chat/completions shape and routes to workers-ai
// via cf-aig-gateway-id header). other models on a native-enabled config
// use the /openai provider endpoint. falls back to the compat URL when
// OpenAINative is on but Account/Gateway are empty so the user gets a
// working but non-native request rather than a malformed URL.
func openAIURLForModel(cfg ProviderConfig, model string) string {
	if cfg.OpenAINative && cfg.Account != "" && cfg.Gateway != "" && !strings.HasPrefix(modelID(model), "@cf/") {
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

const (
	remoteModelsURL   = "https://models.dev/api.json"
	officialModelsURL = "https://opencode.ai/zen/go/v1/models"
)

// officialModelsResponse matches the OpenCode Go /v1/models response shape.
type officialModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// cloudflareModelsResponse matches the cloudflare /ai/models/search shape.
type cloudflareModelsResponse struct {
	Success bool `json:"success"`
	Result  []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"result"`
}

// remoteModelInfo represents the subset of models.dev metadata ocgo needs.
type remoteModelInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

// remoteAPIResponse is the top-level structure of the models.dev API response.
type remoteAPIResponse struct {
	OpenCodeGo struct {
		Models map[string]remoteModelInfo `json:"models"`
	} `json:"opencode-go"`
}

// httpClient is shared by short-lived metadata fetches. Long-running model
// inference requests keep their larger per-request timeouts below.
var httpClient = &http.Client{Timeout: 8 * time.Second}

type lazyFetcher[T any] struct {
	mu      sync.RWMutex
	data    T
	err     error
	fetched bool
	fetch   func() (T, error)
}

func newLazyFetcher[T any](fetch func() (T, error)) *lazyFetcher[T] {
	return &lazyFetcher[T]{fetch: fetch}
}

// ponytail: data is preserved across failed refreshes so the list command
// can show the last known good model list when models.dev is unreachable.
// stale cache has no TTL — add an explicit refresh subcommand or a max-age
// field here if staleness ever becomes a real complaint.
func (f *lazyFetcher[T]) get() (T, error) {
	f.mu.RLock()
	if f.fetched {
		data, err := f.data, f.err
		f.mu.RUnlock()
		return data, err
	}
	f.mu.RUnlock()
	return f.getOnce()
}

// getOnce is the cold-start path for get(): grab the write lock, re-check
// fetched (so two cold get()s share one fetch, not two), then call f.fetch().
// ponytail: serialised under f.mu; switch to a sync.Once or a coalescing
// channel if cold-start latency under concurrent access ever shows up in
// traces.
func (f *lazyFetcher[T]) getOnce() (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetched {
		return f.data, f.err
	}
	f.fetched = true
	newData, err := f.fetch()
	if err == nil {
		f.data = newData
	}
	f.err = err
	return f.data, f.err
}

// forceFetch always runs the fetch under the write lock, commits data
// only on success, and updates err either way. used by refresh() to
// re-attempt without dropping a cached value — distinct from getOnce,
// which skips the fetch when another goroutine already populated data.
func (f *lazyFetcher[T]) forceFetch() (T, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = true
	newData, err := f.fetch()
	if err == nil {
		f.data = newData
	}
	f.err = err
	return f.data, f.err
}

func (f *lazyFetcher[T]) refresh() {
	_, _ = f.forceFetch()
}

var (
	remoteModels   = newLazyFetcher(fetchRemoteModels)
	officialModels = newLazyFetcher(fetchOfficialModels)
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

type Config struct {
	Host string `json:"host"` // local proxy bind
	Port int    `json:"port"` // local proxy port (default 3456)
}

// ProviderConfig is the per-provider upstream config. each named provider
// (opencode-go, cloudflare, ...) lives in its own json file under configDir().
// upstream-only fields moved out of Config so swapping providers doesn't
// require rewriting local-proxy settings, and so two providers can coexist
// (e.g. cloudflare for prod, opencode-go for the opencode.ai default).
type ProviderConfig struct {
	Name              string                  `json:"-"`                  // populated by loadActiveProvider, used by the proxy + list to dispatch on provider identity
	UpstreamBaseURL   string                  `json:"upstream_base_url"`  // e.g. cloudflare /ai/v1 or opencode-go
	UpstreamAPIKey    string                  `json:"upstream_api_key"`   // bearer/x-api-key value sent upstream
	UpstreamAuth      string                  `json:"upstream_auth"`      // "bearer" | "x-api-key" | "header"
	UpstreamAuthHdr   string                  `json:"upstream_auth_hdr"`  // header name for "header" mode
	UpstreamExtraHdr  map[string]string       `json:"upstream_extra_hdr"` // extra headers for upstream
	EndpointOverrides []ModelEndpointOverride `json:"endpoint_overrides"` // per-model routing
	Gateway           string                  `json:"gateway,omitempty"`  // cloudflare: cf-aig-gateway-id value
	Account           string                  `json:"account,omitempty"`  // cloudflare account id; needed for the /openai native path, derived from UpstreamBaseURL for legacy configs
	// OpenAINative: when true, GPT-style models route through Cloudflare's
	// native /openai provider endpoint (transparent pass-through to OpenAI,
	// not the /ai/v1 compat adapter). required for gpt-5.x because the compat
	// adapter sends max_tokens which gpt-5.x rejects. workers-ai @cf/... models
	// still use the legacy /ai/v1 path even when this is true.
	OpenAINative bool `json:"openai_native,omitempty"`
}

// knownProviders is the fixed enum. add a new provider = add a setup
// subcommand + a row in the file. nothing else.
var knownProviders = []string{"opencode-go", "cloudflare"}

func isKnownProvider(name string) bool {
	return slices.Contains(knownProviders, name)
}

type AnthropicRequest struct {
	Model           string          `json:"model"`
	MaxTokens       int             `json:"max_tokens"`
	System          json.RawMessage `json:"system,omitempty"`
	Messages        []AMessage      `json:"messages"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []ATool         `json:"tools,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Thinking        json.RawMessage `json:"thinking,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort,omitempty"`
	Effort          json.RawMessage `json:"effort,omitempty"`
	Level           json.RawMessage `json:"level,omitempty"`
	Depth           json.RawMessage `json:"depth,omitempty"`
	OutputConfig    json.RawMessage `json:"output_config,omitempty"`
}

type AMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ATool struct {
	Type           string          `json:"type,omitempty"`
	Name           string          `json:"name"`
	Description    string          `json:"description,omitempty"`
	InputSchema    json.RawMessage `json:"input_schema,omitempty"`
	MaxUses        int             `json:"max_uses,omitempty"`
	AllowedDomains []string        `json:"allowed_domains,omitempty"`
	BlockedDomains []string        `json:"blocked_domains,omitempty"`
	UserLocation   json.RawMessage `json:"user_location,omitempty"`
}

type OAIRequest struct {
	Model              string            `json:"model"`
	Messages           []OAIMessage      `json:"messages"`
	Stream             bool              `json:"stream,omitempty"`
	StreamOptions      *OAIStreamOptions `json:"stream_options,omitempty"`
	MaxTokens          int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens int              `json:"max_completion_tokens,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Tools              []OAITool         `json:"tools,omitempty"`
	ReasoningEffort    string            `json:"reasoning_effort,omitempty"`
	AnthropicTools     []ATool           `json:"-"`
}

type OAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	MaxTokens       int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Tools           []ResponseTool  `json:"tools,omitempty"`
	Thinking        json.RawMessage `json:"thinking,omitempty"`
	Reasoning       json.RawMessage `json:"reasoning,omitempty"`
	ReasoningEffort json.RawMessage `json:"reasoning_effort,omitempty"`
	Effort          json.RawMessage `json:"effort,omitempty"`
	Level           json.RawMessage `json:"level,omitempty"`
	Depth           json.RawMessage `json:"depth,omitempty"`
	OutputConfig    json.RawMessage `json:"output_config,omitempty"`
}

type ResponseTool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name,omitempty"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters,omitempty"`
	SearchContextSize string          `json:"search_context_size,omitempty"`
	UserLocation      json.RawMessage `json:"user_location,omitempty"`
}

type OAIMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content,omitempty"`
	ToolCalls        []OAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
}

type OAIContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *OAIImageURL `json:"image_url,omitempty"`
}

type OAIImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type OAITool struct {
	Type     string      `json:"type"`
	Function OAIFunction `json:"function"`
}

type OAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OAIToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Function OAICallFunction `json:"function"`
}

type OAICallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// reasoningContentCache holds the last reasoning_content per tool call id.
// bounded LRU; the oldest entries fall off when the cap is reached.
// ponytail: global lock + list.List, not a third-party LRU.
const reasoningCacheCap = 1024

type reasoningCacheEntry struct {
	key, value string
}

var reasoningContentCache = struct {
	sync.Mutex
	ll   *list.List
	keys map[string]*list.Element
}{ll: list.New(), keys: map[string]*list.Element{}}

func main() {
	root := &cobra.Command{Use: appName, Short: "Proxy for Claude Code and Codex CLI with a pluggable openai/anthropic-compatible upstream (cloudflare ai gateway, opencode-go/zen, etc.)", Version: version}
	root.PersistentFlags().String("name", "", "Name this instance for multi-instance setups. defaults to $CFGATE_CC_NAME. empty = single-tenant mode")
	root.AddCommand(setupCmd(), listCmd(), mappingCmd(), launchCmd(), serveCmd(), stopCmd(), statusCmd(), instancesCmd())
	root.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		resolveInstanceName(cmd)
	}
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func setupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure your upstream provider",
	}
	cmd.AddCommand(setupOpencodeGoCmd(), setupCloudflareCmd())
	return cmd
}

func setupOpencodeGoCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use:   "opencode-go",
		Short: "Save your OpenCode Go API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(key) == "" {
				key = os.Getenv("CFGATE_CC_API_KEY")
			}
			if strings.TrimSpace(key) == "" {
				fmt.Print("Upstream provider API key: ")
				line, err := bufio.NewReader(os.Stdin).ReadString('\n')
				if err != nil && line == "" {
					return err
				}
				key = line
			}
			key = strings.TrimSpace(key)
			if key == "" {
				return errors.New("API key cannot be empty")
			}
			p := ProviderConfig{UpstreamAPIKey: key, UpstreamAuth: "both"}
			return saveProviderConfig("opencode-go", p)
		},
	}
	cmd.Flags().StringVar(&key, "api-key", "", "Upstream provider API key")
	return cmd
}

func setupCloudflareCmd() *cobra.Command {
	var token, account, gateway string
	var force, openAINative bool
	cmd := &cobra.Command{
		Use:   "cloudflare",
		Short: "Configure Cloudflare AI Gateway as the upstream",
		RunE: func(cmd *cobra.Command, args []string) error {
			values, err := readCloudflareValues(os.Stdin, token, account, gateway)
			if err != nil {
				return err
			}
			targetURL := buildCloudflareURL(values.account)
			existing, err := loadProviderConfig("cloudflare")
			if err != nil {
				return err
			}
			if existing.UpstreamBaseURL != "" && existing.UpstreamBaseURL != targetURL && !force {
				return fmt.Errorf("cloudflare provider is already configured for %q; pass --force to overwrite", existing.UpstreamBaseURL)
			}
			p := ProviderConfig{
				UpstreamBaseURL: targetURL,
				UpstreamAPIKey:  values.token,
				UpstreamAuth:    "bearer",
				Gateway:         values.gateway,
				Account:         values.account,
				OpenAINative:    openAINative,
			}
			return saveProviderConfig("cloudflare", p)
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "Cloudflare API token (falls back to $CLOUDFLARE_API_TOKEN)")
	cmd.Flags().StringVar(&account, "account", "", "Cloudflare account ID (falls back to $CLOUDFLARE_ACCOUNT_ID)")
	cmd.Flags().StringVar(&gateway, "gateway", "", "Cloudflare AI Gateway ID (falls back to $CLOUDFLARE_GATEWAY_ID)")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing cloudflare provider config without prompting")
	cmd.Flags().BoolVar(&openAINative, "openai-native", false, "Route OpenAI-style models (gpt-5.x) through Cloudflare's native /openai provider endpoint instead of the /ai/v1 compat adapter")
	return cmd
}

type cloudflareValues struct {
	token, account, gateway string
}

// readCloudflareValues resolves each input from flag → env var → stdin prompt.
// the reader parameter is os.Stdin in production; tests pass a strings.Reader
// to drive the prompts without forking.
func readCloudflareValues(r io.Reader, tokenFlag, accountFlag, gatewayFlag string) (cloudflareValues, error) {
	// ponytail: bufio.NewReader must be created once and reused — wrapping r
	// per-prompt drains the rest of the underlying reader into the new
	// buffer, so the next prompt sees EOF.
	br := bufio.NewReader(r)
	prompt := func(label, envName, flagVal string) (string, error) {
		if v := strings.TrimSpace(flagVal); v != "" {
			return v, nil
		}
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			return v, nil
		}
		fmt.Fprintf(os.Stderr, "%s: ", label)
		line, err := br.ReadString('\n')
		if err != nil && line == "" {
			if err == io.EOF {
				return "", nil
			}
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
	t, err := prompt("Cloudflare API token", "CLOUDFLARE_API_TOKEN", tokenFlag)
	if err != nil {
		return cloudflareValues{}, err
	}
	a, err := prompt("Cloudflare account ID", "CLOUDFLARE_ACCOUNT_ID", accountFlag)
	if err != nil {
		return cloudflareValues{}, err
	}
	g, err := prompt("Cloudflare gateway ID", "CLOUDFLARE_GATEWAY_ID", gatewayFlag)
	if err != nil {
		return cloudflareValues{}, err
	}
	if t == "" || a == "" || g == "" {
		return cloudflareValues{}, errors.New("token, account ID, and gateway ID are all required")
	}
	return cloudflareValues{token: t, account: a, gateway: g}, nil
}

// buildCloudflareURL assembles the AI Gateway REST API URL from the account ID.
// the gateway id rides on a header, not the path; see applyCloudflareGatewayHeader.
// ponytail: if cloudflare ever changes the URL shape, only this function moves.
func buildCloudflareURL(account string) string {
	return fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/ai/v1", account)
}

func listCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Aliases: []string{"ls", "models"}, Short: "List models exposed by the configured upstream", RunE: func(cmd *cobra.Command, args []string) error {
		providerName, err := resolveProvider(cmd)
		if err != nil {
			return err
		}
		out := cmd.OutOrStdout()
		switch providerName {
		case "opencode-go":
			refreshAllModelsForProvider(providerName)
			ids, usedCache, kerr := knownModelIDs()
			if kerr != nil && len(ids) == 0 {
				return fmt.Errorf("list: cannot reach models.dev and no cached model list available: %w", kerr)
			}
			if usedCache {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: models.dev fetch failed (%v); showing the last successfully cached model list\n", kerr)
			}
			fmt.Fprintf(out, "Upstream models (provider %s):\n", providerName)
			for _, m := range ids {
				fmt.Fprintf(out, "  %s\n", m)
			}
			return nil
		case "cloudflare":
			p, err := loadActiveProvider(providerName)
			if err != nil {
				return err
			}
			ids, err := cloudflareModelIDs(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Upstream models (provider %s):\n", providerName)
			for _, m := range ids {
				fmt.Fprintf(out, "  %s\n", m)
			}
			return nil
		default:
			return fmt.Errorf("list: unsupported provider %q", providerName)
		}
	}}
	cmd.Flags().String("provider", "", "Upstream provider (opencode-go, cloudflare). defaults to $CFGATE_CC_PROVIDER or the single configured provider")
	return cmd
}

// knownModelIDs returns the opencode-go model ids. usedCache is true when
// the latest remote fetch errored and we returned the previously cached
// list. caller should warn to stderr in that case. if no cached value
// exists and the fetch failed, the returned slice is nil and err is set.
func knownModelIDs() (ids []string, usedCache bool, err error) {
	off, oerr := getOfficialModels()
	if oerr == nil && len(off) > 0 {
		out := append([]string(nil), off...)
		sort.Strings(out)
		return out, false, nil
	}
	// ponytail: stale-cache branch mirrors the remote path below — forceFetch
	// preserves f.data on failed refreshes, so a populated officialModels
	// cache survives a flaky opencode.ai call. only mirror remote when remote
	// itself didn't make it further; otherwise the fresh remote list wins.
	remote, rerr := getRemoteModels()
	if rerr == nil && len(remote) > 0 {
		out := make([]string, 0, len(remote))
		for id := range remote {
			if strings.TrimSpace(id) != "" {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out, false, nil
	}
	if len(remote) > 0 {
		out := make([]string, 0, len(remote))
		for id := range remote {
			if strings.TrimSpace(id) != "" {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return out, true, rerr
	}
	if len(off) > 0 {
		out := append([]string(nil), off...)
		sort.Strings(out)
		return out, true, oerr
	}
	if rerr != nil {
		return nil, false, rerr
	}
	return nil, false, oerr
}

type openCodeModelMetadata struct {
	DisplayName             string
	Description             string
	InputModalities         []string
	CodexInputModalities    []string
	ContextWindow           int
	MaxContextWindow        int
	UsesAnthropicEndpoint   bool
	ParallelToolCalls       bool
	SupportsImageOriginal   bool
	SupportsSearchTool      bool
	SupportedReasoning      []any
	DefaultReasoningLevel   any
	ReasoningSummaries      bool
	DefaultReasoningSummary string
	// ThinkingBudget maps a normalized effort level (minimal|low|medium|high|max)
	// to a budget_tokens value for the upstream /v1/messages thinking shape.
	// zero / missing = model doesn't take a thinking field.
	ThinkingBudget map[string]int
}

func modelMetadata(model string) openCodeModelMetadata {
	id := modelID(model)
	meta := openCodeModelMetadata{
		DisplayName:             id,
		Description:             "Upstream model",
		InputModalities:         []string{"text"},
		CodexInputModalities:    []string{"text"},
		ContextWindow:           128000,
		MaxContextWindow:        128000,
		DefaultReasoningLevel:   nil,
		SupportedReasoning:      []any{},
		DefaultReasoningSummary: "none",
	}
	if remote, err := getRemoteModels(); err == nil {
		if rm, ok := remote[id]; ok {
			if strings.TrimSpace(rm.Name) != "" {
				meta.DisplayName = rm.Name
				meta.Description = rm.Name + " via upstream"
			}
			if len(rm.Modalities.Input) > 0 {
				meta.InputModalities = append([]string(nil), rm.Modalities.Input...)
				meta.CodexInputModalities = codexSupportedModalities(rm.Modalities.Input)
			}
			if rm.Limit.Context > 0 {
				meta.ContextWindow = rm.Limit.Context
				meta.MaxContextWindow = rm.Limit.Context
			}
		}
	}
	switch id {
	case "minimax-m3":
		if meta.DisplayName == id {
			meta.DisplayName = "MiniMax M3"
			meta.Description = "MiniMax M3 via upstream"
		}
		if meta.ContextWindow == 128000 {
			meta.ContextWindow = 512000
			meta.MaxContextWindow = 512000
		}
		if len(meta.InputModalities) == 1 && meta.InputModalities[0] == "text" {
			meta.InputModalities = []string{"text", "image", "video"}
			meta.CodexInputModalities = []string{"text", "image"}
		}
		meta.UsesAnthropicEndpoint = true
		meta.ParallelToolCalls = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 4096, "medium": 8192, "high": 16384, "max": 32768,
		}
	case "minimax-m2.7":
		meta.UsesAnthropicEndpoint = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 4096, "medium": 8192, "high": 16384, "max": 32768,
		}
	case "minimax-m2.5":
		meta.UsesAnthropicEndpoint = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 2048, "medium": 4096, "high": 8192, "max": 16384,
		}
	case "qwen3.7-max":
		meta.UsesAnthropicEndpoint = true
		meta.SupportsSearchTool = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 2048, "medium": 4096, "high": 8192, "max": 16384,
		}
	case "qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus":
		// ponytail: same buckets as qwen3.7-max, unverified per-model values.
		// models.dev flags these as reasoning-capable on /v1/messages; exact
		// budget split isn't published. refine when upstream docs land.
		meta.UsesAnthropicEndpoint = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 2048, "medium": 4096, "high": 8192, "max": 16384,
		}
	case "glm-5.2", "kimi-k2.7-code":
		// cloudflare workers-ai anthropic-compatible models. routed via
		// /v1/messages, gateway header set by applyCloudflareGatewayHeader.
		meta.UsesAnthropicEndpoint = true
		meta.ThinkingBudget = map[string]int{
			"minimal": 0, "low": 2048, "medium": 4096, "high": 8192, "max": 16384,
		}
	case "kimi-k2.6", "kimi-k2.5", "mimo-v2-omni":
		if len(meta.InputModalities) == 1 && meta.InputModalities[0] == "text" {
			meta.InputModalities = []string{"text", "image"}
			meta.CodexInputModalities = []string{"text", "image"}
		}
	case "gpt-5.5", "gpt-5.4":
		// OpenAI gpt-5.x via cloudflare's /openai native endpoint. multimodal,
		// 1M context (200k output cap on the wire, surfaced as 128k here for
		// the codex catalog). Values mirror the opencode config defaults.
		meta.ContextWindow = 1000000
		meta.MaxContextWindow = 1000000
		meta.InputModalities = []string{"text", "image"}
		meta.CodexInputModalities = []string{"text", "image"}
		meta.SupportsImageOriginal = true
	case "gpt-5.4-mini":
		meta.ContextWindow = 400000
		meta.MaxContextWindow = 400000
		meta.InputModalities = []string{"text", "image"}
		meta.CodexInputModalities = []string{"text", "image"}
		meta.SupportsImageOriginal = true
	}
	return meta
}

func codexSupportedModalities(modalities []string) []string {
	out := make([]string, 0, len(modalities))
	seen := map[string]bool{}
	for _, modality := range modalities {
		switch modality {
		case "text", "image":
			if !seen[modality] {
				out = append(out, modality)
				seen[modality] = true
			}
		}
	}
	if len(out) == 0 {
		return []string{"text"}
	}
	return out
}

func fetchRemoteModels() (map[string]remoteModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("remote models API returned status %d", resp.StatusCode)
	}
	var apiResp remoteAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode remote models: %w", err)
	}
	if apiResp.OpenCodeGo.Models == nil {
		return nil, errors.New("remote models API returned no opencode-go models")
	}
	return apiResp.OpenCodeGo.Models, nil
}

func getRemoteModels() (map[string]remoteModelInfo, error) {
	return remoteModels.get()
}

func fetchOfficialModels() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, officialModelsURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official models API returned status %d", resp.StatusCode)
	}
	var apiResp officialModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode official models: %w", err)
	}
	ids := make([]string, 0, len(apiResp.Data))
	seen := map[string]bool{}
	for _, m := range apiResp.Data {
		id := strings.TrimSpace(m.ID)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

func getOfficialModels() ([]string, error) {
	return officialModels.get()
}

func refreshAllModels() {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); officialModels.refresh() }()
	go func() { defer wg.Done(); remoteModels.refresh() }()
	wg.Wait()
}

// refreshAllModelsForProvider dispatches the per-provider refresh chain.
// opencode-go has two upstream sources; cloudflare has no cache (live
// fetch happens at print time, the account's URL is the cache key).
func refreshAllModelsForProvider(name string) {
	if name == "opencode-go" {
		refreshAllModels()
	}
}

// fetchCloudflareModels hits cloudflare's /ai/models/search endpoint and
// returns the @cf/... model names. no static fallback — the REST API is the
// source of truth, a failure is reported to the user.
func fetchCloudflareModels(cfg ProviderConfig) ([]string, error) {
	// cfg.UpstreamBaseURL is the inference base (.../accounts/<id>/ai/v1).
	// model listing lives at (.../accounts/<id>/ai/models/search).
	url := strings.Replace(cfg.UpstreamBaseURL, "/ai/v1", "/ai/models/search", 1)
	if url == cfg.UpstreamBaseURL {
		return nil, fmt.Errorf("fetchCloudflareModels: base URL %q does not contain /ai/v1; cannot derive /ai/models/search", cfg.UpstreamBaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	applyUpstreamAuth(req, cfg)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare models API returned status %d", resp.StatusCode)
	}
	var apiResp cloudflareModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("failed to decode cloudflare models: %w", err)
	}
	if !apiResp.Success {
		return nil, fmt.Errorf("cloudflare models API: success=false")
	}
	ids := make([]string, 0, len(apiResp.Result))
	seen := map[string]bool{}
	for _, m := range apiResp.Result {
		id := strings.TrimSpace(m.Name)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids, nil
}

// cloudflareNativeModelIDs returns the static list of OpenAI models
// routable through the cloudflare /openai native endpoint when
// OpenAINative is enabled. The compat /ai/models/search endpoint doesn't
// surface these, so we list them by hand. Keep the list aligned with
// modelMetadata() — adding a gpt-* model there but not here makes it
// invisible to `cfgate-cc list` until a manual mapping is set.
func cloudflareNativeModelIDs() []string {
	return []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini"}
}

// cloudflareModelIDs returns the live cloudflare model list. unlike
// opencode-go's knownModelIDs chain there's no static fallback — the
// REST API is the only source. when OpenAINative is enabled, append the
// native-side models so `list` shows the full set a user can route to.
func cloudflareModelIDs(cfg ProviderConfig) ([]string, error) {
	ids, err := fetchCloudflareModels(cfg)
	if err != nil {
		if !cfg.OpenAINative {
			return nil, err
		}
		// fall back to native ids only when compat fetch fails, so the
		// user at least sees what's routable on the native path.
		return append([]string(nil), cloudflareNativeModelIDs()...), nil
	}
	if cfg.OpenAINative {
		seen := make(map[string]bool, len(ids))
		for _, id := range ids {
			seen[id] = true
		}
		for _, id := range cloudflareNativeModelIDs() {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		sort.Strings(ids)
	}
	return ids, nil
}

func defaultModelMappings() map[string]map[string]string {
	return map[string]map[string]string{
		"claude": {},
		"codex":  {},
	}
}

// modelMappingMigratedSentinel marks that the old tool-scoped format warning
// has already been printed. file presence = warning already shown. per-
// instance: each named cfgate-cc is self-contained, so its migration state
// lives in its own dir.
var modelMappingMigratedSentinel = func() string { return instanceModelMappingMigratedSentinel(resolvedInstanceName) }

// loadAllModelMappings reads the model-mapping file. the file shape is
// per-provider at the top level: { "opencode-go": { "claude": {...} },
// "cloudflare": { "claude": {...} } }.
//
// if the file is in the old tool-scoped shape (top-level "claude" /
// "codex") and providerName is a known provider, the legacy entries are
// lifted into that provider's section in place — the user's existing
// mappings carry over without a manual `mapping set` re-run. the on-disk
// file is rewritten in the new shape and the migration sentinel is
// removed so the next read sees the new format. pass "" for providerName
// to skip migration (used by tests and the warn-only path).
//
// if the file is in the old shape but providerName is empty or not a
// known provider, the result is empty and a one-shot warning is printed
// (gated by modelMappingMigratedSentinel) — picking a target without
// knowing the active provider would just dump the entries somewhere
// arbitrary.
func loadAllModelMappings(providerName string) (map[string]map[string]map[string]string, error) {
	path := modelMappingFile()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && resolvedInstanceName != "" {
		// ad-hoc (auto-named) instance: fall back to the base model-mapping
		// so all ad-hoc launches for the same provider share one config.
		// named instances always use their own (they may have customized it).
		path = filepath.Join(configDir(), "model-mapping.json")
		b, err = os.ReadFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return map[string]map[string]map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	// two-stage parse: first as a flat map of raw json so we can detect
	// the old tool-scoped shape before committing to a typed unmarshal
	// (the old shape doesn't fit map[string]map[string]map[string]string).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", modelMappingFile(), err)
	}
	if looksLikeOldMappingFormatRaw(raw) {
		if isKnownProvider(providerName) {
			return migrateOldMappingFormatInPlace(raw, providerName)
		}
		warnOldMappingFormatOnce()
		return map[string]map[string]map[string]string{}, nil
	}
	var typed map[string]map[string]map[string]string
	if err := json.Unmarshal(b, &typed); err != nil {
		return nil, fmt.Errorf("parse %s: %w", modelMappingFile(), err)
	}
	if typed == nil {
		typed = map[string]map[string]map[string]string{}
	}
	for name, byTool := range typed {
		for tool, entries := range byTool {
			if typed[name][tool] == nil {
				typed[name][tool] = map[string]string{}
			}
			typed[name][tool] = cleanMappingEntries(entries)
		}
	}
	return typed, nil
}

// cleanMappingEntries drops empty source/target pairs and strips the
// opencode-go/ prefix from targets. shared by the new-format read path
// and the old-format migration so both end up with the same shape on
// disk.
func cleanMappingEntries(entries map[string]string) map[string]string {
	cleaned := map[string]string{}
	for source, target := range entries {
		if strings.TrimSpace(source) != "" && strings.TrimSpace(target) != "" {
			cleaned[strings.TrimSpace(source)] = modelID(target)
		}
	}
	return cleaned
}

// migrateOldMappingFormatInPlace lifts a legacy tool-scoped model-mapping
// file into providerName's section, writes the new-shape file back, and
// drops the migration sentinel. preserves any non-tool top-level entries
// (none expected, but be safe). the caller has already confirmed the
// file is in the old shape via looksLikeOldMappingFormatRaw. returns the
// full post-migration shape so callers see peer provider sections too.
func migrateOldMappingFormatInPlace(raw map[string]json.RawMessage, providerName string) (map[string]map[string]map[string]string, error) {
	oldShape := map[string]map[string]string{}
	for _, tool := range []string{"claude", "codex"} {
		v, ok := raw[tool]
		if !ok {
			continue
		}
		var entries map[string]string
		if err := json.Unmarshal(v, &entries); err != nil {
			return nil, fmt.Errorf("parse legacy %s/%s: %w", modelMappingFile(), tool, err)
		}
		oldShape[tool] = cleanMappingEntries(entries)
	}
	newShape := map[string]map[string]map[string]string{}
	for k, v := range raw {
		if k == "claude" || k == "codex" {
			continue
		}
		var sec map[string]map[string]string
		if err := json.Unmarshal(v, &sec); err == nil {
			newShape[k] = sec
		}
	}
	newShape[providerName] = oldShape
	if err := saveAllModelMappings(newShape); err != nil {
		return nil, err
	}
	_ = os.Remove(modelMappingMigratedSentinel())
	oldMappingFormatWarned = true
	fmt.Fprintf(os.Stderr, "cfgate-cc: migrated legacy model-mapping.json into %q section (one-time)\n", providerName)
	return newShape, nil
}

func saveAllModelMappings(all map[string]map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(modelMappingFile()), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(modelMappingFile(), append(b, '\n'), 0644)
}

// loadModelMappingsForProvider returns the tool → source→target section for
// the named provider. known provider with no section: empty default mapping
// (claude/codex keys, no entries). unknown provider: empty map. either way
// the proxy falls through to the default model passthrough when the section
// has no entries for the requested tool.
func loadModelMappingsForProvider(name string) (map[string]map[string]string, error) {
	all, err := loadAllModelMappings(name)
	if err != nil {
		return nil, err
	}
	if !isKnownProvider(name) {
		return map[string]map[string]string{}, nil
	}
	section := all[name]
	if section == nil {
		return defaultModelMappings(), nil
	}
	return section, nil
}

// saveModelMappingsForProvider updates the section for name and writes the
// file back, preserving other providers' sections.
func saveModelMappingsForProvider(name string, m map[string]map[string]string) error {
	all, err := loadAllModelMappings(name)
	if err != nil {
		return err
	}
	if m == nil {
		m = defaultModelMappings()
	}
	all[name] = m
	return saveAllModelMappings(all)
}

// looksLikeOldMappingFormatRaw detects the old tool-scoped shape by
// checking the top-level keys. only the old format's tools are checked —
// the new format's providers are opencode-go and cloudflare, which the
// old format never used.
func looksLikeOldMappingFormatRaw(raw map[string]json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	for _, tool := range []string{"claude", "codex"} {
		if _, ok := raw[tool]; ok {
			return true
		}
	}
	return false
}

var oldMappingFormatWarned bool

// warnOldMappingFormatOnce prints the migration warning the first time an
// old-format model-mapping.json is read, and creates the sentinel file so
// subsequent runs are silent. the in-process flag avoids duplicate prints
// when many calls happen in one invocation (e.g. tests).
func warnOldMappingFormatOnce() {
	if oldMappingFormatWarned {
		return
	}
	if _, err := os.Stat(modelMappingMigratedSentinel()); err == nil {
		oldMappingFormatWarned = true
		return
	}
	fmt.Fprintf(os.Stderr, "warning: %s is in the old tool-scoped format. run `cfgate-cc mapping --provider <name> <tool> set ...` to re-create mappings per provider.\n", modelMappingFile())
	oldMappingFormatWarned = true
	_ = os.MkdirAll(filepath.Dir(modelMappingMigratedSentinel()), 0755)
	_ = os.WriteFile(modelMappingMigratedSentinel(), []byte("warned at "+time.Now().UTC().Format(time.RFC3339)+"\n"), 0644)
	_ = os.Stderr.Sync()
}

func mappingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "mapping", Short: "Manage tool model mappings to upstream models"}
	cmd.PersistentFlags().String("provider", "", "Upstream provider (opencode-go, cloudflare). defaults to $CFGATE_CC_PROVIDER or the single configured provider")
	cmd.AddCommand(toolMappingCmd("claude"), toolMappingCmd("codex"))
	return cmd
}

// providerFromMappingCmd resolves --provider from the mapping subcommand
// tree. the flag lives on the parent (`mapping`), cobra inherits persistent
// flags to children, so cmd.Flags().Lookup("provider") on a leaf works.
func providerFromMappingCmd(cmd *cobra.Command) (string, error) {
	if cmd != nil {
		if f := cmd.Flags().Lookup("provider"); f != nil {
			if v := strings.TrimSpace(f.Value.String()); v != "" {
				if !isKnownProvider(v) {
					return "", fmt.Errorf("unknown --provider %q (known: %s)", v, strings.Join(knownProviders, ", "))
				}
				return v, nil
			}
		}
	}
	return resolveProvider(cmd)
}

func toolMappingCmd(tool string) *cobra.Command {
	cmd := &cobra.Command{Use: tool, Short: fmt.Sprintf("Manage %s model mappings", tool)}
	cmd.AddCommand(&cobra.Command{Use: "show", Short: "Show current mapping", RunE: func(cmd *cobra.Command, args []string) error {
		name, err := providerFromMappingCmd(cmd)
		if err != nil {
			return err
		}
		m, err := loadModelMappingsForProvider(name)
		if err != nil {
			return err
		}
		printToolMapping(tool, m[tool])
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "get <source-model>", Short: "Get one mapped upstream model", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := providerFromMappingCmd(cmd)
		if err != nil {
			return err
		}
		m, err := loadModelMappingsForProvider(name)
		if err != nil {
			return err
		}
		source := strings.TrimSpace(args[0])
		normalized := modelID(source)
		if target := resolveMappedModel(tool, source, m); target != normalized {
			fmt.Printf("%s -> %s\n", source, target)
			return nil
		}
		return fmt.Errorf("no mapping for %q in %s", source, tool)
	}})
	cmd.AddCommand(&cobra.Command{Use: "set <source-model> <opencode-model>", Short: "Set one model mapping", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := providerFromMappingCmd(cmd)
		if err != nil {
			return err
		}
		source := strings.TrimSpace(args[0])
		target := strings.TrimSpace(args[1])
		if source == "" || target == "" {
			return errors.New("source and target models cannot be empty")
		}
		ok, kerr := knownModelForProvider(name, target)
		if !ok {
			if kerr != nil {
				return fmt.Errorf("cannot validate %q for provider %q: %w (run `cfgate-cc list --provider %s` to see the cached model list)", target, name, kerr, name)
			}
			return fmt.Errorf("unknown upstream model %q for provider %q; run `cfgate-cc list --provider %s`", target, name, name)
		}
		m, err := loadModelMappingsForProvider(name)
		if err != nil {
			return err
		}
		if m[tool] == nil {
			m[tool] = map[string]string{}
		}
		m[tool][source] = modelID(target)
		if err := saveModelMappingsForProvider(name, m); err != nil {
			return err
		}
		fmt.Printf("%s %s -> %s\n", tool, source, modelID(target))
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "unset <source-model>", Aliases: []string{"rm", "remove", "delete"}, Short: "Remove one model mapping", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := providerFromMappingCmd(cmd)
		if err != nil {
			return err
		}
		source := strings.TrimSpace(args[0])
		if source == "" {
			return errors.New("source model cannot be empty")
		}
		m, err := loadModelMappingsForProvider(name)
		if err != nil {
			return err
		}
		if m[tool] == nil {
			m[tool] = map[string]string{}
		}
		if _, ok := m[tool][source]; !ok {
			return fmt.Errorf("no mapping for %q in %s", source, tool)
		}
		delete(m[tool], source)
		if err := saveModelMappingsForProvider(name, m); err != nil {
			return err
		}
		fmt.Printf("removed %s mapping for %s\n", tool, source)
		return nil
	}})
	cmd.AddCommand(&cobra.Command{Use: "open", Short: "Open mapping file in $EDITOR", RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := os.Stat(modelMappingFile()); errors.Is(err, os.ErrNotExist) {
			if err := saveAllModelMappings(map[string]map[string]map[string]string{}); err != nil {
				return err
			}
		}
		editor := os.Getenv("EDITOR")
		if editor == "" {
			editor = "vi"
		}
		c := exec.Command(editor, modelMappingFile())
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}})
	return cmd
}

func printToolMapping(tool string, mapping map[string]string) {
	fmt.Printf("%s -> upstream mapping (%s):\n", displayToolName(tool), modelMappingFile())
	if len(mapping) == 0 {
		fmt.Println("  (empty)")
		return
	}
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-24s -> %s\n", k, mapping[k])
	}
}

func displayToolName(tool string) string {
	if tool == "" {
		return tool
	}
	return strings.ToUpper(tool[:1]) + tool[1:]
}

func printLaunchMapping(tool string, mapping map[string]string) {
	if len(mapping) == 0 {
		fmt.Fprintf(os.Stderr, "No cfgate-cc model mappings configured for %s (%s)\n", tool, modelMappingFile())
		return
	}
	fmt.Fprintf(os.Stderr, "cfgate-cc model mapping enabled for %s (%s)\n", tool, modelMappingFile())
	keys := make([]string, 0, len(mapping))
	for k := range mapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, "  %s -> %s\n", k, mapping[k])
	}
}

func knownOpenCodeModel(model string) bool {
	model = modelID(model)
	ids, _, _ := knownModelIDs()
	for _, id := range ids {
		if id == model {
			return true
		}
	}
	return false
}

// knownModelForProvider dispatches the "is this model valid for this provider"
// check. opencode-go has a static/live chain; cloudflare uses its live
// /v1/models list. unknown providers always return (false, nil). the second
// return is the upstream fetch error, surfaced to the caller so a network
// failure doesn't masquerade as "unknown model". loads the active provider's
// config internally so callers (e.g. mapping set) don't have to thread it
// through.
func knownModelForProvider(name, model string) (bool, error) {
	id := modelID(model)
	switch name {
	case "opencode-go":
		ids, _, kerr := knownModelIDs()
		for _, candidate := range ids {
			if candidate == id {
				return true, nil
			}
		}
		return false, kerr
	case "cloudflare":
		p, err := loadActiveProvider(name)
		if err != nil {
			return false, err
		}
		ids, err := cloudflareModelIDs(p)
		if err != nil {
			return false, err
		}
		for _, candidate := range ids {
			if candidate == id {
				return true, nil
			}
		}
		return false, nil
	default:
		return false, nil
	}
}

// providerKnownModelIDs returns the model id list for the named provider.
// used by writeCodexModelCatalog and any other codex-side caller that needs
// the active provider's model list, not a hardcoded one.
func providerKnownModelIDs(name string, p ProviderConfig) ([]string, error) {
	switch name {
	case "opencode-go":
		refreshAllModels()
		// ponytail: catalog silently uses stale cache on fetch failure; the
		// user-facing list command is where the warning surfaces. add error
		// surfacing here if the codex-side catalog ever needs to differentiate.
		ids, _, _ := knownModelIDs()
		return ids, nil
	case "cloudflare":
		return cloudflareModelIDs(p)
	default:
		return nil, fmt.Errorf("unknown provider %q", name)
	}
}

func loadModelMappings() (map[string]map[string]string, error) {
	return loadModelMappingsForProvider("opencode-go")
}

func saveModelMappings(mappings map[string]map[string]string) error {
	return saveModelMappingsForProvider("opencode-go", mappings)
}

func resolveMappedModel(tool, source string, mappings map[string]map[string]string) string {
	source = strings.TrimSpace(modelID(source))
	entries := mappings[tool]
	if target := entries[source]; target != "" {
		return target
	}
	if tool == "claude" {
		for _, family := range []string{"opus", "sonnet", "haiku"} {
			if source == family || strings.Contains(source, "claude-"+family) {
				if target := entries["claude-"+family]; target != "" {
					return target
				}
			}
		}
	}
	return source
}

func modelID(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), "opencode-go/")
}

func modelUsesAnthropicEndpoint(model string, cfg ProviderConfig) bool {
	id := modelID(model)
	for _, ov := range cfg.EndpointOverrides {
		if ov.Pattern == "" {
			continue
		}
		matched, err := path.Match(ov.Pattern, id)
		if err == nil && matched {
			return ov.Route == "anthropic"
		}
	}
	return modelMetadata(model).UsesAnthropicEndpoint
}

// modelThinkingBudgetMax returns the override max thinking budget for a model
// if any glob in cfg.EndpointOverrides matches. The bool reports whether a
// glob matched at all — a 0 max is a valid override (escape hatch to disable
// thinking for the matched model) and must not be conflated with "no
// override". A matched glob whose ThinkingBudgetMax is nil (a routing-only
// entry) is not an override and returns ok=false.
func modelThinkingBudgetMax(model string, cfg ProviderConfig) (int, bool) {
	id := modelID(model)
	for _, ov := range cfg.EndpointOverrides {
		if ov.Pattern == "" {
			continue
		}
		matched, err := path.Match(ov.Pattern, id)
		if err == nil && matched && ov.ThinkingBudgetMax != nil {
			return *ov.ThinkingBudgetMax, true
		}
	}
	return 0, false
}

func modelSupportsImages(model string) bool {
	for _, modality := range modelMetadata(model).InputModalities {
		if modality == "image" {
			return true
		}
	}
	return false
}

func modelInputModalities(model string) []string {
	modalities := modelMetadata(model).InputModalities
	return append([]string(nil), modalities...)
}

func launchCmd() *cobra.Command {
	var model string
	var yes bool
	var codexConfigOnly bool
	cmd := &cobra.Command{Use: "launch", Short: "Launch tools through cfgate-cc"}
	claude := &cobra.Command{Use: "claude [-- claude args...]", Short: "Launch Claude Code through the configured upstream provider", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		providerName, err := resolveProvider(cmd)
		if err != nil {
			return err
		}
		if resolvedInstanceName == "" {
			resolvedInstanceName = autoInstanceName(providerName)
			fmt.Fprintf(os.Stderr, "cfgate-cc: auto-named instance %q (use --name to pick your own; `cfgate-cc instances` to see all)\n", resolvedInstanceName)
		}
		base, err := resolveInstanceBase(cfg)
		if err != nil {
			return err
		}
		p, err := loadActiveProvider(providerName)
		if err != nil {
			return err
		}
		serverCmd, err := startLaunchServer(base, providerName)
		if err != nil {
			return err
		}
		if serverCmd != nil {
			defer stopManagedServer(serverCmd)
		}
		claudeArgs := append([]string{}, args...)
		if yes {
			claudeArgs = append([]string{"--dangerously-skip-permissions"}, claudeArgs...)
		}
		bin, err := exec.LookPath("claude")
		if err != nil {
			return fmt.Errorf("claude not found in PATH: %w", err)
		}
		c := exec.Command(bin, claudeArgs...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		// ponytail: non-empty placeholder when no key is set; claude code's startup check rejects empty tokens ("login required"). the proxy's applyUpstreamAuth signs every upstream request from cfg.UpstreamAPIKey, so the child's value is decorative.
		authToken := p.UpstreamAPIKey
		if authToken == "" {
			authToken = "cfgate-cc"
		}
		c.Env = append(os.Environ(), "ANTHROPIC_BASE_URL="+base, "ANTHROPIC_AUTH_TOKEN="+authToken)
		mappings, err := loadModelMappingsForProvider(providerName)
		if err != nil {
			return err
		}
		if model != "" {
			c.Env = append(c.Env,
				"ANTHROPIC_MODEL="+model,
				"ANTHROPIC_SMALL_FAST_MODEL="+model,
				"ANTHROPIC_CUSTOM_MODEL_OPTION="+model,
				"ANTHROPIC_CUSTOM_MODEL_OPTION_NAME="+model+" via cfgate-cc",
				"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION=Upstream model routed through cfgate-cc",
			)
		} else {
			opus := resolveMappedModel("claude", "claude-opus", mappings)
			sonnet := resolveMappedModel("claude", "claude-sonnet", mappings)
			haiku := resolveMappedModel("claude", "claude-haiku", mappings)
			if opus != "claude-opus" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_OPUS_MODEL="+opus,
					"ANTHROPIC_DEFAULT_OPUS_MODEL_NAME="+opus+" via cfgate-cc",
					"ANTHROPIC_DEFAULT_OPUS_MODEL_DESCRIPTION=Upstream model routed through cfgate-cc",
				)
			}
			if sonnet != "claude-sonnet" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_SONNET_MODEL="+sonnet,
					"ANTHROPIC_DEFAULT_SONNET_MODEL_NAME="+sonnet+" via cfgate-cc",
					"ANTHROPIC_DEFAULT_SONNET_MODEL_DESCRIPTION=Upstream model routed through cfgate-cc",
				)
			}
			if haiku != "claude-haiku" {
				c.Env = append(c.Env,
					"ANTHROPIC_DEFAULT_HAIKU_MODEL="+haiku,
					"ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME="+haiku+" via cfgate-cc",
					"ANTHROPIC_DEFAULT_HAIKU_MODEL_DESCRIPTION=Upstream model routed through cfgate-cc",
					"ANTHROPIC_SMALL_FAST_MODEL="+haiku,
				)
			}
		}
		// ponytail: writes .claude/settings.json so the supervisor and agent-view subagents also route through our proxy. the supervisor strips ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN from the shell env, so env vars on the child process aren't enough.
		if err := writeClaudeSettings(base, authToken); err != nil {
			fmt.Fprintf(os.Stderr, "cfgate-cc: warning: .claude/settings.json: %v\n", err)
		}
		printLaunchMapping("claude", mappings["claude"])
		return c.Run()
	}}
	claude.Flags().StringVar(&model, "model", "", "Upstream model ID")
	claude.Flags().BoolVar(&yes, "yes", false, "Allow Claude Code to skip permission prompts")
	claude.Flags().String("provider", "", "Upstream provider (opencode-go, cloudflare). defaults to $CFGATE_CC_PROVIDER or the single configured provider")
	codex := &cobra.Command{Use: "codex [-- codex args...]", Short: "Launch Codex CLI through the configured upstream provider", Args: cobra.ArbitraryArgs, RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		providerName, err := resolveProvider(cmd)
		if err != nil {
			return err
		}
		if resolvedInstanceName == "" {
			resolvedInstanceName = autoInstanceName(providerName)
			fmt.Fprintf(os.Stderr, "cfgate-cc: auto-named instance %q (use --name to pick your own; `cfgate-cc instances` to see all)\n", resolvedInstanceName)
		}
		base, err := resolveInstanceBase(cfg)
		if err != nil {
			return err
		}
		p, err := loadActiveProvider(providerName)
		if err != nil {
			return err
		}
		if err := ensureCodexConfig(base, p); err != nil {
			return fmt.Errorf("failed to configure codex: %w", err)
		}
		if codexConfigOnly {
			fmt.Printf("Configured Codex profile %q in %s\n", codexProfileNameFor(resolvedInstanceName), codexProfileConfigFile())
			return nil
		}
		if err := checkCodexVersion(); err != nil {
			return err
		}
		serverCmd, err := startLaunchServer(base, providerName)
		if err != nil {
			return err
		}
		if serverCmd != nil {
			defer stopManagedServer(serverCmd)
		}
		codexArgs := []string{"--profile", codexProfileNameFor(resolvedInstanceName)}
		if model != "" {
			codexArgs = append(codexArgs, "-m", model)
		}
		codexArgs = append(codexArgs, args...)
		bin, err := exec.LookPath("codex")
		if err != nil {
			return fmt.Errorf("codex not found in PATH; install with: npm install -g @openai/codex: %w", err)
		}
		c := exec.Command(bin, codexArgs...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		c.Env = append(os.Environ(), "OPENAI_API_KEY=cfgate-cc")
		if mappings, err := loadModelMappingsForProvider(providerName); err == nil {
			printLaunchMapping("codex", mappings["codex"])
		}
		return c.Run()
	}}
	codex.Flags().StringVar(&model, "model", "", "Upstream model ID")
	codex.Flags().BoolVar(&codexConfigOnly, "config", false, "Configure Codex profile without launching")
	codex.Flags().String("provider", "", "Upstream provider (opencode-go, cloudflare). defaults to $CFGATE_CC_PROVIDER or the single configured provider")
	cmd.AddCommand(claude, codex)
	return cmd
}

func serveCmd() *cobra.Command {
	var background bool
	cmd := &cobra.Command{Use: "serve", Short: "Start local Anthropic-compatible proxy", RunE: func(cmd *cobra.Command, args []string) error {
		providerName, err := resolveProvider(cmd)
		if err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		if background {
			// ensure the instance has a port allocated and persisted to
			// config.json before spawning the subprocess. the subprocess
			// reads loadConfig() to learn its bind port, so we have to
			// commit the port first.
			if _, err := resolveInstanceBase(cfg); err != nil {
				return err
			}
			return startBackground(providerName)
		}
		p, err := loadActiveProvider(providerName)
		if err != nil {
			return err
		}
		return runServer(cfg, p)
	}}
	cmd.Flags().BoolVarP(&background, "background", "b", false, "Run proxy in the background")
	cmd.Flags().String("provider", "", "Upstream provider (opencode-go, cloudflare). defaults to $CFGATE_CC_PROVIDER or the single configured provider")
	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop", Short: "Stop background proxy", RunE: func(cmd *cobra.Command, args []string) error {
		pid, err := readPID()
		if err != nil {
			cfg, cfgErr := loadConfig()
			if cfgErr != nil {
				return errors.New("proxy is not running")
			}
			pid, err = findListenerPID(cfg.Port)
			if err != nil {
				return errors.New("proxy is not running")
			}
		}
		p, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		_ = os.Remove(pidFile())
		_ = os.Remove(activeProviderFile())
		if err := p.Kill(); err != nil {
			return err
		}
		fmt.Printf("Stopped proxy process %d\n", pid)
		return nil
	}}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show proxy status", Run: func(cmd *cobra.Command, args []string) {
		cfg, err := loadConfig()
		if err != nil || !healthy(fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port)) {
			fmt.Println("Proxy is not running")
			return
		}
		provider := "(unknown)"
		if name, err := resolveProvider(cmd); err == nil {
			provider = name
		}
		if pid, err := readPID(); err == nil {
			fmt.Printf("Proxy is running on %s:%d (provider %s, PID %d)\n", cfg.Host, cfg.Port, provider, pid)
			return
		}
		if pid, err := findListenerPID(cfg.Port); err == nil {
			fmt.Printf("Proxy is running on %s:%d (provider %s, PID %d, discovered from listener)\n", cfg.Host, cfg.Port, provider, pid)
			return
		}
		fmt.Printf("Proxy is running on %s:%d (provider %s, no cfgate-cc PID file)\n", cfg.Host, cfg.Port, provider)
	}}
}

// instancesCmd lists all named cfgate-cc instances under configDir()/instances/*
// and prints a table of name, provider, port, pid, and status. empty/no
// instances dir = "no instances" with a hint to set up one with --name.
func instancesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "instances",
		Short: "List running and configured named instances",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			dir := filepath.Join(configDir(), "instances")
			entries, err := os.ReadDir(dir)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(out, "no instances configured (use --name <name> to set one up)")
					return nil
				}
				return err
			}
			fmt.Fprintf(out, "%-12s %-14s %-6s %-7s %s\n", "NAME", "PROVIDER", "PORT", "PID", "STATUS")
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				name := e.Name()
				instDir := instanceDir(name)
				port := readInstancePort(instDir)
				provider := readInstanceActiveProvider(instDir)
				pid := readInstancePID(instDir)
				status := "stopped"
				if port > 0 && healthy(fmt.Sprintf("http://%s:%d", defaultHost, port)) {
					status = "running"
				}
				pidStr := "-"
				if pid > 0 {
					pidStr = strconv.Itoa(pid)
				}
				portStr := "-"
				if port > 0 {
					portStr = strconv.Itoa(port)
				}
				provStr := "-"
				if provider != "" {
					provStr = provider
				}
				fmt.Fprintf(out, "%-12s %-14s %-6s %-7s %s\n", name, provStr, portStr, pidStr, status)
			}
			return nil
		},
	}
}

func readInstancePort(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "port"))
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return p
}

// saveInstancePort writes the chosen port to the instance's `port` file.
// ponytail: single file is enough; config.json is human-readable and the
// truth lives here, not there.
func saveInstancePort(port int) error {
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err != nil {
		return err
	}
	return os.WriteFile(instancePortFile(resolvedInstanceName), []byte(strconv.Itoa(port)+"\n"), 0644)
}

func readInstanceActiveProvider(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "active-provider"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readInstancePID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "cfgate-cc.pid"))
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return p
}

func runServer(cfg Config, p ProviderConfig) error {
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err == nil {
		_ = os.WriteFile(pidFile(), []byte(fmt.Sprint(os.Getpid())), 0644)
		_ = os.WriteFile(activeProviderFile(), []byte(p.Name), 0644)
		defer os.Remove(pidFile())
		defer os.Remove(activeProviderFile())
	}
	// resolve the actual bind port from the instance base. when launched
	// by `serve -b` (background), the parent already wrote the port to
	// the `port` file via ensureInstancePort; this read reuses it. when
	// launched standalone (no parent), this allocates.
	base, err := resolveInstanceBase(cfg)
	if err != nil {
		return err
	}
	bindHost, bindPort, err := parseBaseURL(base)
	if err != nil {
		return err
	}
	setupDebug()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("/v1/messages/count_tokens", countTokens)
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) { proxyDo(w, r, p, messagesEndpoint) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) { proxyDo(w, r, p, chatEndpoint) })
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) { proxyDo(w, r, p, responsesEndpoint) })
	addr := fmt.Sprintf("%s:%d", bindHost, bindPort)
	fmt.Printf("cfgate-cc proxy listening on http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

// parseBaseURL extracts host+port from a http://host:port/ URL. ponytail:
// 4 lines vs pulling in net/url for a known shape.
func parseBaseURL(base string) (string, int, error) {
	rest := strings.TrimPrefix(base, "http://")
	rest = strings.TrimSuffix(rest, "/")
	host, portStr, err := net.SplitHostPort(rest)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return "", 0, err
	}
	return host, port, nil
}

func copyHeaders(dst, src http.Header) {
	for k, vals := range src {
		for _, v := range vals {
			dst.Add(k, v)
		}
	}
}

// applyUpstreamAuth picks the upstream auth header from cfg.UpstreamAuth:
// "bearer" (default) → Authorization: Bearer <key>; "x-api-key" → X-API-Key;
// "header" → arbitrary <cfg.UpstreamAuthHdr>; "both" → sends Bearer and
// x-api-key. Then merges cfg.UpstreamExtraHdr.
// ponytail: no fallback to a local key — the upstream key lives in
// ProviderConfig.UpstreamAPIKey. opencode-go's setup writes it there directly.
// if the upstream key is empty, no auth header is sent (better than a
// half-baked `Authorization: Bearer ` going upstream).
func applyUpstreamAuth(req *http.Request, cfg ProviderConfig) {
	key := cfg.UpstreamAPIKey
	if key == "" {
		for k, v := range cfg.UpstreamExtraHdr {
			req.Header.Set(k, v)
		}
		return
	}
	switch cfg.UpstreamAuth {
	case "x-api-key":
		req.Header.Set("X-API-Key", key)
	case "header":
		if cfg.UpstreamAuthHdr != "" {
			req.Header.Set(cfg.UpstreamAuthHdr, key)
		}
	case "both":
		// opencode-go's two endpoints disagree on auth: /v1/chat/completions
		// accepts Bearer, /v1/messages wants x-api-key. sending both works
		// for both. setup opencode-go writes this by default.
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("x-api-key", key)
	default: // "bearer" or empty
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range cfg.UpstreamExtraHdr {
		req.Header.Set(k, v)
	}
}

// cloudflareUpstreamPrefix marks the new cloudflare REST API URL. the
// ai-gateway `/compat/v1` shape is gone; the gateway id rides on a
// header now (cf-aig-gateway-id), required for @cf/... workers-ai models.
const cloudflareUpstreamPrefix = "https://api.cloudflare.com/client/v4/accounts/"

func isCloudflareUpstream(cfg ProviderConfig) bool {
	return strings.HasPrefix(cfg.UpstreamBaseURL, cloudflareUpstreamPrefix)
}

// cloudflarePrepareBody strips the "workers-ai/" prefix from the JSON
// `model` field for the cloudflare REST API. returns the new body and
// the post-strip model id. the model id is what callers pass to
// applyCloudflareGatewayHeader to decide whether to inject the header.
func cloudflarePrepareBody(body []byte, cfg ProviderConfig) ([]byte, string) {
	if !isCloudflareUpstream(cfg) {
		return body, ""
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return body, ""
	}
	raw, ok := fields["model"]
	if !ok {
		return body, ""
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		return body, ""
	}
	wire := strings.TrimPrefix(model, "workers-ai/")
	if wire == model {
		return body, model
	}
	encoded, _ := json.Marshal(wire)
	fields["model"] = encoded
	out, err := json.Marshal(fields)
	if err != nil {
		return body, wire
	}
	return out, wire
}

// applyCloudflareGatewayHeader sets cf-aig-gateway-id for workers-ai
// (@cf/...) models on the cloudflare REST API. third-party models go
// through the default gateway and don't need the header.
func applyCloudflareGatewayHeader(req *http.Request, cfg ProviderConfig, wireModel string) {
	if cfg.Gateway != "" && strings.HasPrefix(wireModel, "@cf/") {
		req.Header.Set("cf-aig-gateway-id", cfg.Gateway)
	}
}

// applyUpstreamAuthForModel picks the right auth-header strategy for the
// given model in flight. opencode-go (or any non-cloudflare provider) keeps
// the existing bearer/x-api-key behavior. cloudflare splits per model:
// @cf/... workers-ai uses a bearer token; non-@cf/ models on a native-
// enabled config use a blank Authorization + cf-aig-authorization header
// so the gateway injects the stored BYOK OpenAI key. non-native
// cloudflare configs (legacy /ai/v1) keep the bearer path.
func applyUpstreamAuthForModel(req *http.Request, cfg ProviderConfig, wireModel string) {
	if cfg.Name != "cloudflare" || strings.HasPrefix(wireModel, "@cf/") || !cfg.OpenAINative {
		applyUpstreamAuth(req, cfg)
		return
	}
	if cfg.UpstreamAPIKey != "" {
		req.Header.Set("cf-aig-authorization", "Bearer "+cfg.UpstreamAPIKey)
	}
	req.Header.Set("Authorization", "")
}

func forwardAnthropic(ctx context.Context, cfg ProviderConfig, ar AnthropicRequest) (*http.Response, error) {
	normalizeAnthropicRequestForUpstream(&ar, cfg)
	body, err := json.Marshal(ar)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicURL(cfg), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	applyUpstreamAuth(req, cfg)
	req.Header.Set("Content-Type", "application/json")
	dlogUpstreamReq(req, body)
	return (&http.Client{Timeout: 10 * time.Minute}).Do(req)
}

func normalizeAnthropicRequestForUpstream(ar *AnthropicRequest, p ProviderConfig) {
	ensureAnthropicRequestDefaults(ar, p)
	// OpenCode Go's Anthropic-compatible endpoint is stricter than Anthropic's
	// Claude endpoint for some model families (notably qwen3.7-max). Claude Code
	// can send Anthropic-specific prompt-caching and extended-thinking fields that
	// make those upstreams return "Request body format invalid". Translate the
	// user-facing effort/reasoning/thinking fields into the upstream's
	// {type: enabled, budget_tokens: N} thinking shape and drop the rest.
	ar.Thinking = anthropicThinkingForRequest(ar, p)
	ar.Reasoning = nil
	ar.ReasoningEffort = nil
	ar.Effort = nil
	ar.Level = nil
	ar.Depth = nil
	ar.OutputConfig = nil
	ar.System = normalizeAnthropicSystem(ar.System)
	for i := range ar.Messages {
		ar.Messages[i].Content = normalizeAnthropicContent(ar.Messages[i].Content)
	}
}

// anthropicThinkingForRequest translates the various user-facing effort /
// reasoning / thinking knobs into the upstream's
// {type: enabled, budget_tokens: N} shape, looked up per-model. Returns nil
// when the user didn't request thinking, the model has no budget for the
// resolved level, or the override cap is 0 (escape hatch back to the old
// "no thinking field" behavior).
func anthropicThinkingForRequest(ar *AnthropicRequest, p ProviderConfig) json.RawMessage {
	// walk the candidates directly with the raw walker — going through
	// downstreamReasoningEffort would route the value through
	// normalizeReasoningEffort, which collapses "max" to "high" and would
	// lose the max bucket from the per-model budget table.
	var effort string
	for _, raw := range []json.RawMessage{ar.Reasoning, ar.Thinking, ar.OutputConfig, ar.ReasoningEffort, ar.Effort, ar.Level, ar.Depth} {
		if e := reasoningEffortFromRaw(raw); e != "" {
			effort = e
			break
		}
	}
	level := resolveEffortLevel(effort)
	if level == "" || level == "minimal" {
		return nil
	}
	budget := modelMetadata(ar.Model).ThinkingBudget[level]
	if max, ok := modelThinkingBudgetMax(ar.Model, p); ok {
		// override always wins, even when 0 — that's the escape hatch to
		// disable thinking for a model that the user requested it on.
		budget = max
	}
	if budget <= 0 {
		return nil
	}
	// struct not map: json.Marshal on map[string]any sorts keys alphabetically,
	// which would reorder the upstream's expected {type, budget_tokens} shape.
	out, err := json.Marshal(struct {
		Type        string `json:"type"`
		BudgetTokens int   `json:"budget_tokens"`
	}{Type: "enabled", BudgetTokens: budget})
	if err != nil {
		return nil
	}
	return out
}

func normalizeAnthropicSystem(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}
	text := systemText(raw)
	if text == "" {
		return nil
	}
	return marshalJSON(text)
}

func normalizeAnthropicContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return raw
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return raw
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var text string
			_ = json.Unmarshal(b["text"], &text)
			out = append(out, map[string]any{"type": "text", "text": text})
		case "image":
			block := map[string]any{"type": "image"}
			if v, ok := rawJSONAny(b["source"]); ok {
				block["source"] = v
			}
			out = append(out, block)
		case "tool_use":
			block := map[string]any{"type": "tool_use"}
			copyRawJSONField(block, b, "id")
			copyRawJSONField(block, b, "name")
			copyRawJSONField(block, b, "input")
			out = append(out, block)
		case "tool_result":
			block := map[string]any{"type": "tool_result"}
			copyRawJSONField(block, b, "tool_use_id")
			copyAnthropicToolResultContent(block, b)
			copyRawJSONField(block, b, "is_error")
			out = append(out, block)
		}
	}
	if len(out) == 0 {
		return marshalJSON("")
	}
	return marshalJSON(out)
}

func copyAnthropicToolResultContent(dst map[string]any, src map[string]json.RawMessage) {
	if v, ok := rawJSONAny(src["content"]); ok {
		dst["content"] = truncateToolResultContent(v)
	}
}

func truncateToolResultContent(v any) any {
	remaining := maxAnthropicToolResultContentChars
	return truncateToolResultContentValue(v, &remaining)
}

func truncateToolResultContentValue(v any, remaining *int) any {
	switch x := v.(type) {
	case string:
		return truncateStringToBudget(x, remaining)
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, truncateToolResultContentValue(val, remaining))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = truncateToolResultContentValue(val, remaining)
		}
		return out
	default:
		return v
	}
}

func truncateStringToBudget(s string, remaining *int) string {
	if *remaining <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= *remaining {
		*remaining -= len(runes)
		return s
	}
	kept := *remaining
	*remaining = 0
	return string(runes[:kept]) + fmt.Sprintf("\n\n[cfgate-cc truncated tool_result content: omitted %d characters]", len(runes)-kept)
}

func copyRawJSONField(dst map[string]any, src map[string]json.RawMessage, key string) {
	if v, ok := rawJSONAny(src[key]); ok {
		dst[key] = v
	}
}

func rawJSONAny(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	return stripCacheControl(v), true
}

func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k == "cache_control" {
				continue
			}
			out[k] = stripCacheControl(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for _, val := range x {
			out = append(out, stripCacheControl(val))
		}
		return out
	default:
		return v
	}
}

func ensureAnthropicRequestDefaults(ar *AnthropicRequest, p ProviderConfig) {
	ar.Model = resolveToolModel("claude", ar.Model, p)
	if ar.MaxTokens == 0 {
		ar.MaxTokens = 4096
	}
}

func resolveToolModel(tool, source string, p ProviderConfig) string {
	mappings, err := loadModelMappingsForProvider(p.Name)
	if err != nil {
		mappings = defaultModelMappings()
	}
	return resolveMappedModel(tool, source, mappings)
}

func prepareChatBody(body []byte, p ProviderConfig) ([]byte, error) {
	var req map[string]any
	if json.Unmarshal(body, &req) != nil {
		return body, nil
	}
	changed := requestStreamingUsage(req)
	if applyRawChatReasoningEffort(req) {
		changed = true
	}
	model, _ := req["model"].(string)
	if mapped := resolveToolModel("codex", model, p); mapped != model {
		req["model"] = mapped
		model = mapped
		changed = true
	}
	if usesMaxCompletionTokens(model) {
		// GPT-5.x rejects max_tokens. Move any value to max_completion_tokens
		// at the raw layer so the chat-completions path doesn't need its own
		// special-cased rewriter.
		if v, ok := req["max_tokens"]; ok {
			if _, already := req["max_completion_tokens"]; !already {
				req["max_completion_tokens"] = v
				changed = true
			}
			delete(req, "max_tokens")
			changed = true
		}
	}
	if rawChatBodyHasImages(req) {
		if !modelSupportsImages(model) {
			return nil, unsupportedImageModelError(model)
		}
		changed = stripRawChatImageDetails(req) || changed
	}
	if !changed {
		return sanitizeRawChatToolMessages(body), nil
	}
	out, err := json.Marshal(req)
	if err != nil {
		return sanitizeRawChatToolMessages(body), nil
	}
	return sanitizeRawChatToolMessages(out), nil
}

func applyRawChatReasoningEffort(req map[string]any) bool {
	effort := rawChatReasoningEffort(req)
	changed := false
	if effort != "" {
		current, _ := req["reasoning_effort"].(string)
		if current != effort {
			req["reasoning_effort"] = effort
			changed = true
		}
	}
	for _, key := range []string{"reasoning", "thinking", "effort", "level", "depth", "output_config"} {
		if _, ok := req[key]; ok {
			delete(req, key)
			changed = true
		}
	}
	return changed
}

func rawChatReasoningEffort(req map[string]any) string {
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "output_config", "effort", "level", "depth"} {
		if effort := reasoningEffortFromAny(req[key]); effort != "" {
			return resolveEffortLevel(effort)
		}
	}
	return ""
}

func downstreamReasoningEffort(values ...json.RawMessage) string {
	for _, raw := range values {
		if effort := reasoningEffortFromRaw(raw); effort != "" {
			return resolveEffortLevel(effort)
		}
	}
	return ""
}

func reasoningEffortFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	return reasoningEffortFromAny(v)
}

func reasoningEffortFromAny(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return formatReasoningNumber(t)
	case map[string]any:
		for _, key := range []string{"effort", "level", "depth", "reasoning_effort"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
		if typ, _ := t["type"].(string); strings.EqualFold(strings.TrimSpace(typ), "enabled") {
			return "high"
		}
		for _, key := range []string{"reasoning", "thinking", "output_config"} {
			if effort := reasoningEffortFromAny(t[key]); effort != "" {
				return effort
			}
		}
	}
	return ""
}

func formatReasoningNumber(n float64) string {
	if n == float64(int64(n)) {
		return strconv.FormatInt(int64(n), 10)
	}
	return strconv.FormatFloat(n, 'f', -1, 64)
}

func normalizeReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "0", "minimal", "min", "none", "off", "disabled", "false":
		return "minimal"
	case "1", "low", "light":
		return "low"
	case "2", "medium", "med", "normal", "default":
		return "medium"
	case "3", "4", "high", "xhigh", "max", "maximum", "deep", "true", "enabled":
		return "high"
	default:
		return strings.TrimSpace(effort)
	}
}

// resolveEffortLevel is normalizeReasoningEffort's stricter sibling: it keeps
// "max" (and its aliases) as a distinct bucket so per-model thinking budgets
// and reasoning_effort payloads can carry it. Used by both the Anthropic
// thinking-block path and the chat-completions /v1/chat/completions path.
func resolveEffortLevel(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "0", "minimal", "min", "none", "off", "disabled", "false":
		return "minimal"
	case "1", "low", "light":
		return "low"
	case "2", "medium", "med", "normal", "default":
		return "medium"
	case "3", "high", "deep", "true", "enabled":
		return "high"
	case "4", "xhigh", "max", "maximum":
		return "max"
	default:
		return strings.TrimSpace(effort)
	}
}

func requestStreamingUsage(req map[string]any) bool {
	streaming, _ := req["stream"].(bool)
	if !streaming {
		return false
	}
	options, ok := req["stream_options"].(map[string]any)
	if !ok {
		options = map[string]any{}
		req["stream_options"] = options
	}
	if enabled, _ := options["include_usage"].(bool); enabled {
		return false
	}
	options["include_usage"] = true
	return true
}

func rawChatBodyHasImages(req map[string]any) bool {
	messages, _ := req["messages"].([]any)
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if contentHasImage(msg["content"]) {
			return true
		}
	}
	return false
}

func validateImageSupport(or OAIRequest) error {
	if requestHasImages(or) && !modelSupportsImages(or.Model) {
		return unsupportedImageModelError(or.Model)
	}
	return nil
}

func unsupportedImageModelError(model string) error {
	if model == "" {
		model = "unknown"
	}
	return fmt.Errorf("model %s does not support image inputs", model)
}

func stripRawChatImageDetails(req map[string]any) bool {
	changed := false
	messages, _ := req["messages"].([]any)
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		parts, _ := msg["content"].([]any)
		for _, part := range parts {
			p, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if _, ok := p["detail"]; ok {
				delete(p, "detail")
				changed = true
			}
			image, ok := p["image_url"].(map[string]any)
			if !ok {
				continue
			}
			if _, ok := image["detail"]; ok {
				delete(image, "detail")
				changed = true
			}
		}
	}
	return changed
}

// usesMaxCompletionTokens reports whether the given model id needs the
// max_completion_tokens field instead of max_tokens. applies to gpt-5.x
// (any new-style openai model that dropped the older field).
func usesMaxCompletionTokens(model string) bool {
	id := modelID(model)
	if id == "gpt-5" || strings.HasPrefix(id, "gpt-5.") {
		return true
	}
	return false
}

func convertRequest(ar AnthropicRequest, p ProviderConfig) OAIRequest {
	model := resolveToolModel("claude", ar.Model, p)
	out := OAIRequest{Model: model, Stream: ar.Stream, StreamOptions: streamUsageOptions(ar.Stream), Temperature: ar.Temperature, TopP: ar.TopP, ReasoningEffort: downstreamReasoningEffort(ar.Reasoning, ar.Thinking, ar.OutputConfig, ar.ReasoningEffort, ar.Effort, ar.Level, ar.Depth)}
	if usesMaxCompletionTokens(model) {
		out.MaxCompletionTokens = ar.MaxTokens
	} else {
		out.MaxTokens = ar.MaxTokens
	}
	if sys := systemText(ar.System); sys != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: sys})
	}
	for _, m := range ar.Messages {
		out.Messages = append(out.Messages, contentToOpenAI(m)...)
	}
	for _, t := range ar.Tools {
		if strings.TrimSpace(t.Name) != "" {
			out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: toolParametersOrDefault(t.InputSchema)}})
		}
	}
	return out
}

func responsesToChat(rr ResponsesRequest, p ProviderConfig) OAIRequest {
	model := resolveToolModel("codex", rr.Model, p)
	out := OAIRequest{Model: model, Stream: rr.Stream, StreamOptions: streamUsageOptions(rr.Stream), Temperature: rr.Temperature, TopP: rr.TopP, ReasoningEffort: downstreamReasoningEffort(rr.Reasoning, rr.Thinking, rr.OutputConfig, rr.ReasoningEffort, rr.Effort, rr.Level, rr.Depth)}
	if usesMaxCompletionTokens(model) {
		out.MaxCompletionTokens = rr.MaxTokens
	} else {
		out.MaxTokens = rr.MaxTokens
	}
	if rr.Instructions != "" {
		out.Messages = append(out.Messages, OAIMessage{Role: "system", Content: rr.Instructions})
	}
	out.Messages = append(out.Messages, sanitizeOAIToolMessages(responsesInputToMessages(rr.Input))...)
	for _, t := range rr.Tools {
		if tool, ok := responseBuiltinToolToAnthropic(t); ok {
			out.AnthropicTools = appendUniqueAnthropicTool(out.AnthropicTools, tool)
			continue
		}
		if strings.TrimSpace(t.Name) != "" && (t.Type == "" || t.Type == "function") {
			out.Tools = append(out.Tools, OAITool{Type: "function", Function: OAIFunction{Name: t.Name, Description: t.Description, Parameters: toolParametersOrDefault(t.Parameters)}})
		}
	}
	return out
}

func responseBuiltinToolToAnthropic(t ResponseTool) (ATool, bool) {
	switch strings.ToLower(strings.TrimSpace(t.Type)) {
	case "web_search", "web_search_2025_08_26", "web_search_preview", "web_search_preview_2025_03_11":
		tool := ATool{Type: "web_search_20250305", Name: "web_search", UserLocation: t.UserLocation}
		return tool, true
	case "web_fetch", "web_extractor":
		return ATool{Type: "web_fetch_20250910", Name: "web_fetch"}, true
	default:
		return ATool{}, false
	}
}

func appendUniqueAnthropicTool(tools []ATool, tool ATool) []ATool {
	for _, existing := range tools {
		if existing.Type == tool.Type && existing.Name == tool.Name {
			return tools
		}
	}
	return append(tools, tool)
}

func toolParametersOrDefault(raw json.RawMessage) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

func chatToAnthropic(or OAIRequest, p ProviderConfig) AnthropicRequest {
	model := resolveToolModel("codex", or.Model, p)
	out := AnthropicRequest{Model: model, MaxTokens: or.MaxTokens, Stream: or.Stream, Temperature: or.Temperature, TopP: or.TopP}
	if out.MaxTokens == 0 {
		out.MaxTokens = 4096
	}
	var system []string
	for _, m := range or.Messages {
		role := m.Role
		if role == "developer" {
			role = "system"
		}
		switch role {
		case "system":
			if text := openAIContentText(m.Content); text != "" {
				system = append(system, text)
			}
		case "tool":
			out.Messages = append(out.Messages, AMessage{Role: "user", Content: marshalJSON([]map[string]any{{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": openAIContentText(m.Content)}})})
		case "assistant":
			out.Messages = append(out.Messages, AMessage{Role: "assistant", Content: assistantContentToAnthropic(m)})
		default:
			if role == "" {
				role = "user"
			}
			out.Messages = append(out.Messages, AMessage{Role: role, Content: openAIContentToAnthropic(m.Content)})
		}
	}
	if len(system) > 0 {
		out.System = marshalJSON(strings.Join(system, "\n\n"))
	}
	for _, t := range or.AnthropicTools {
		out.Tools = appendUniqueAnthropicTool(out.Tools, t)
	}
	for _, t := range or.Tools {
		if strings.TrimSpace(t.Function.Name) != "" && (t.Type == "" || t.Type == "function") {
			out.Tools = append(out.Tools, ATool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: toolParametersOrDefault(t.Function.Parameters)})
		}
	}
	return out
}

func assistantContentToAnthropic(m OAIMessage) json.RawMessage {
	blocks := anthropicBlocksFromOpenAIContent(m.Content)
	for _, call := range m.ToolCalls {
		input := any(map[string]any{})
		if strings.TrimSpace(call.Function.Arguments) != "" {
			var parsed any
			if json.Unmarshal([]byte(call.Function.Arguments), &parsed) == nil {
				input = parsed
			} else {
				input = call.Function.Arguments
			}
		}
		blocks = append(blocks, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}
	return marshalJSON(blocks)
}

func openAIContentToAnthropic(content any) json.RawMessage {
	if text, ok := content.(string); ok {
		return marshalJSON(text)
	}
	return marshalJSON(anthropicBlocksFromOpenAIContent(content))
}

func anthropicBlocksFromOpenAIContent(content any) []map[string]any {
	switch v := content.(type) {
	case nil:
		return []map[string]any{{"type": "text", "text": ""}}
	case string:
		if v == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": v}}
	case []OAIContentPart:
		var out []map[string]any
		for _, part := range v {
			out = appendAnthropicPart(out, part.Type, part.Text, part.ImageURL)
		}
		return out
	case []any:
		var out []map[string]any
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			text, _ := m["text"].(string)
			if text == "" {
				text, _ = m["output_text"].(string)
			}
			out = appendAnthropicPart(out, typ, text, imageURLFromAny(m["image_url"], m["url"]))
		}
		return out
	default:
		return []map[string]any{{"type": "text", "text": fmt.Sprint(v)}}
	}
}

func appendAnthropicPart(out []map[string]any, typ, text string, image *OAIImageURL) []map[string]any {
	switch typ {
	case "text", "input_text", "output_text":
		if text != "" {
			out = append(out, map[string]any{"type": "text", "text": text})
		}
	case "image_url", "input_image":
		if image != nil && image.URL != "" {
			out = append(out, map[string]any{"type": "image", "source": anthropicImageSource(image.URL)})
		}
	}
	return out
}

func imageURLFromAny(imageValue, urlValue any) *OAIImageURL {
	if s, ok := imageValue.(string); ok && s != "" {
		return &OAIImageURL{URL: s}
	}
	if m, ok := imageValue.(map[string]any); ok {
		if s, _ := m["url"].(string); s != "" {
			return &OAIImageURL{URL: s}
		}
	}
	if s, ok := urlValue.(string); ok && s != "" {
		return &OAIImageURL{URL: s}
	}
	return nil
}

func anthropicImageSource(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		mediaType := "image/png"
		data := url
		if rest, ok := strings.CutPrefix(url, "data:"); ok {
			if header, body, found := strings.Cut(rest, ","); found {
				data = body
				if mt, _, found := strings.Cut(header, ";"); found && mt != "" {
					mediaType = mt
				}
			}
		}
		return map[string]any{"type": "base64", "media_type": mediaType, "data": data}
	}
	return map[string]any{"type": "url", "url": url}
}

func openAIContentText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []OAIContentPart:
		var b strings.Builder
		for _, part := range v {
			b.WriteString(part.Text)
		}
		return b.String()
	case []any:
		var b strings.Builder
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := m["text"].(string); text != "" {
				b.WriteString(text)
			}
			if text, _ := m["output_text"].(string); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	default:
		return fmt.Sprint(v)
	}
}

func marshalJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func streamUsageOptions(streaming bool) *OAIStreamOptions {
	if !streaming {
		return nil
	}
	return &OAIStreamOptions{IncludeUsage: true}
}

func requestHasImages(or OAIRequest) bool {
	for _, m := range or.Messages {
		if contentHasImage(m.Content) {
			return true
		}
	}
	return false
}

func contentHasImage(content any) bool {
	switch v := content.(type) {
	case []OAIContentPart:
		for _, part := range v {
			if part.Type == "image_url" && part.ImageURL != nil && part.ImageURL.URL != "" {
				return true
			}
		}
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if typ, _ := m["type"].(string); typ == "image_url" || typ == "input_image" {
				return true
			}
		}
	}
	return false
}

func responsesInputToMessages(raw json.RawMessage) []OAIMessage {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []OAIMessage{{Role: "user", Content: s}}
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return []OAIMessage{{Role: "user", Content: string(raw)}}
	}
	var out []OAIMessage
	var pendingCalls []OAIToolCall
	for _, item := range items {
		var typ, role string
		_ = json.Unmarshal(item["type"], &typ)
		_ = json.Unmarshal(item["role"], &role)
		switch typ {
		case "message", "":
			if role == "developer" {
				role = "system"
			}
			if role == "" {
				role = "user"
			}
			out = append(out, OAIMessage{Role: role, Content: responsesContent(item["content"])})
		case "function_call":
			var id, callID, name, args string
			_ = json.Unmarshal(item["id"], &id)
			_ = json.Unmarshal(item["call_id"], &callID)
			_ = json.Unmarshal(item["name"], &name)
			_ = json.Unmarshal(item["arguments"], &args)
			if callID == "" {
				callID = id
			}
			pendingCalls = append(pendingCalls, OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: name, Arguments: args}})
		case "function_call_output":
			if len(pendingCalls) > 0 {
				out = append(out, assistantToolCallsMessage(pendingCalls))
				pendingCalls = nil
			}
			var callID string
			_ = json.Unmarshal(item["call_id"], &callID)
			out = append(out, OAIMessage{Role: "tool", ToolCallID: callID, Content: responsesContentText(item["output"])})
		}
	}
	if len(pendingCalls) > 0 {
		out = append(out, assistantToolCallsMessage(pendingCalls))
	}
	return out
}

func assistantToolCallsMessage(calls []OAIToolCall) OAIMessage {
	return OAIMessage{Role: "assistant", ToolCalls: calls, ReasoningContent: cachedReasoningContent(calls)}
}

const unavailableToolResultContent = "Tool result unavailable."

// sanitizeOAIToolMessages enforces the OpenAI-compatible invariant that an
// assistant message with tool_calls is immediately followed by tool messages
// for those exact call IDs. It drops orphan/late tool messages and inserts a
// conservative placeholder for any missing result.
func sanitizeOAIToolMessages(messages []OAIMessage) []OAIMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]OAIMessage, 0, len(messages))
	for i := 0; i < len(messages); {
		m := messages[i]
		if m.Role == "tool" {
			// A tool message that is not consumed immediately after an assistant
			// tool_calls message is orphaned or late and must not be forwarded.
			i++
			continue
		}
		out = append(out, m)
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			i++
			continue
		}

		expected := toolCallIDOrder(m.ToolCalls)
		seen := map[string]bool{}
		j := i + 1
		for j < len(messages) && messages[j].Role == "tool" {
			toolMsg := messages[j]
			if containsString(expected, toolMsg.ToolCallID) && !seen[toolMsg.ToolCallID] {
				out = append(out, toolMsg)
				seen[toolMsg.ToolCallID] = true
			}
			j++
		}
		for _, id := range expected {
			if !seen[id] {
				out = append(out, OAIMessage{Role: "tool", ToolCallID: id, Content: unavailableToolResultContent})
			}
		}
		i = j
	}
	return out
}

func toolCallIDOrder(calls []OAIToolCall) []string {
	ids := make([]string, 0, len(calls))
	seen := map[string]bool{}
	for _, call := range calls {
		id := strings.TrimSpace(call.ID)
		if id != "" && !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// sanitizeRawChatToolMessages sanitizes a raw chat-completions request while
// preserving unknown top-level and per-message fields. It only rebuilds the
// messages array when it actually needs to drop an orphan/late tool message or
// insert a placeholder for a missing result.
func sanitizeRawChatToolMessages(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil {
		return body
	}
	rawMessages, ok := req["messages"]
	if !ok {
		return body
	}
	var messages []json.RawMessage
	if json.Unmarshal(rawMessages, &messages) != nil {
		return body
	}
	sanitized, changed := sanitizeRawChatMessages(messages)
	if !changed {
		return body
	}
	newMessages, err := json.Marshal(sanitized)
	if err != nil {
		return body
	}
	req["messages"] = newMessages
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

func sanitizeRawChatMessages(messages []json.RawMessage) ([]json.RawMessage, bool) {
	out := make([]json.RawMessage, 0, len(messages))
	changed := false
	for i := 0; i < len(messages); {
		msg := parseRawChatMessage(messages[i])
		if msg.Role == "tool" {
			changed = true
			i++
			continue
		}
		out = append(out, messages[i])
		if msg.Role != "assistant" || len(msg.ToolCallIDs) == 0 {
			i++
			continue
		}

		seen := map[string]bool{}
		j := i + 1
		for j < len(messages) {
			next := parseRawChatMessage(messages[j])
			if next.Role != "tool" {
				break
			}
			if containsString(msg.ToolCallIDs, next.ToolCallID) && !seen[next.ToolCallID] {
				out = append(out, messages[j])
				seen[next.ToolCallID] = true
			} else {
				changed = true
			}
			j++
		}
		for _, id := range msg.ToolCallIDs {
			if !seen[id] {
				out = append(out, rawToolPlaceholderMessage(id))
				changed = true
			}
		}
		i = j
	}
	return out, changed
}

type rawChatMessageInfo struct {
	Role        string
	ToolCallID  string
	ToolCallIDs []string
}

func parseRawChatMessage(raw json.RawMessage) rawChatMessageInfo {
	var msg struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	_ = json.Unmarshal(raw, &msg)
	info := rawChatMessageInfo{Role: msg.Role, ToolCallID: msg.ToolCallID}
	seen := map[string]bool{}
	for _, call := range msg.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id != "" && !seen[id] {
			info.ToolCallIDs = append(info.ToolCallIDs, id)
			seen[id] = true
		}
	}
	return info
}

func rawToolPlaceholderMessage(callID string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"role": "tool", "tool_call_id": callID, "content": unavailableToolResultContent})
	return b
}

func cachedReasoningContent(calls []OAIToolCall) string {
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, call := range calls {
		if e, ok := reasoningContentCache.keys[call.ID]; ok {
			reasoningContentCache.ll.MoveToFront(e)
			if v := e.Value.(*reasoningCacheEntry).value; v != "" {
				return v
			}
		}
	}
	if len(calls) > 0 {
		// Moonshot/Kimi rejects follow-up assistant tool-call messages when
		// thinking is enabled unless reasoning_content is present. Some
		// OpenAI-compatible streams omit reasoning_content on the initial tool
		// call, so provide a minimal placeholder for replayed tool-call history.
		return "Tool call requested."
	}
	return ""
}

func cacheReasoningContent(calls []OAIToolCall, reasoning string) {
	if reasoning == "" || len(calls) == 0 {
		return
	}
	reasoningContentCache.Lock()
	defer reasoningContentCache.Unlock()
	for _, call := range calls {
		if call.ID == "" {
			continue
		}
		if e, ok := reasoningContentCache.keys[call.ID]; ok {
			e.Value.(*reasoningCacheEntry).value = reasoning
			reasoningContentCache.ll.MoveToFront(e)
			continue
		}
		for reasoningContentCache.ll.Len() >= reasoningCacheCap {
			oldest := reasoningContentCache.ll.Back()
			if oldest == nil {
				break
			}
			evicted := oldest.Value.(*reasoningCacheEntry)
			reasoningContentCache.ll.Remove(oldest)
			delete(reasoningContentCache.keys, evicted.key)
		}
		entry := &reasoningCacheEntry{key: call.ID, value: reasoning}
		reasoningContentCache.keys[call.ID] = reasoningContentCache.ll.PushFront(entry)
	}
}

func responsesContent(raw json.RawMessage) any {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var text strings.Builder
	var out []OAIContentPart
	hasImage := false
	for _, p := range parts {
		var typ string
		_ = json.Unmarshal(p["type"], &typ)
		switch typ {
		case "input_text", "output_text", "text":
			for _, key := range []string{"text", "output_text"} {
				var v string
				if json.Unmarshal(p[key], &v) == nil {
					text.WriteString(v)
					out = append(out, OAIContentPart{Type: "text", Text: v})
					break
				}
			}
		case "input_image", "image_url":
			if image := responsesImageURL(p); image != nil {
				hasImage = true
				out = append(out, OAIContentPart{Type: "image_url", ImageURL: image})
			}
		}
	}
	if hasImage {
		return out
	}
	return text.String()
}

func responsesImageURL(p map[string]json.RawMessage) *OAIImageURL {
	var url string
	if json.Unmarshal(p["image_url"], &url) != nil {
		var obj struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(p["image_url"], &obj) == nil {
			url = obj.URL
		}
	}
	if url == "" {
		_ = json.Unmarshal(p["url"], &url)
	}
	if url == "" {
		return nil
	}
	return &OAIImageURL{URL: url}
}

func responsesContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, p := range parts {
		for _, key := range []string{"text", "output_text"} {
			var v string
			if json.Unmarshal(p[key], &v) == nil {
				b.WriteString(v)
			}
		}
	}
	return b.String()
}

func contentToOpenAI(m AMessage) []OAIMessage {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []OAIMessage{{Role: m.Role, Content: s}}
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(m.Content, &blocks) != nil {
		return []OAIMessage{{Role: m.Role, Content: string(m.Content)}}
	}
	var text strings.Builder
	var parts []OAIContentPart
	hasImage := false
	var calls []OAIToolCall
	var toolMsgs []OAIMessage
	for _, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			var v string
			_ = json.Unmarshal(b["text"], &v)
			text.WriteString(v)
			if v != "" {
				parts = append(parts, OAIContentPart{Type: "text", Text: v})
			}
		case "image":
			if image := anthropicImageURL(b); image != nil {
				hasImage = true
				parts = append(parts, OAIContentPart{Type: "image_url", ImageURL: image})
			}
		case "tool_use":
			var id, name string
			_ = json.Unmarshal(b["id"], &id)
			_ = json.Unmarshal(b["name"], &name)
			args := "{}"
			if raw := b["input"]; len(raw) > 0 {
				args = string(raw)
			}
			calls = append(calls, OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: name, Arguments: args}})
		case "tool_result":
			var id string
			_ = json.Unmarshal(b["tool_use_id"], &id)
			toolMsgs = append(toolMsgs, OAIMessage{Role: "tool", ToolCallID: id, Content: blockText(b["content"])})
		}
	}
	if len(calls) > 0 {
		msg := assistantToolCallsMessage(calls)
		msg.Content = openAIContentValue(text.String(), parts, hasImage)
		return []OAIMessage{msg}
	}
	if len(toolMsgs) > 0 {
		out := append([]OAIMessage{}, toolMsgs...)
		if userText := strings.TrimSpace(text.String()); userText != "" {
			// Anthropic can send a user's next text in the same content array as
			// tool_result blocks. Preserve that text as the next user message;
			// dropping it makes the model answer the previous tool result again.
			out = append(out, OAIMessage{Role: m.Role, Content: userText})
		}
		return out
	}
	return []OAIMessage{{Role: m.Role, Content: openAIContentValue(text.String(), parts, hasImage)}}
}

func openAIContentValue(text string, parts []OAIContentPart, hasImage bool) any {
	if hasImage {
		return parts
	}
	return text
}

func anthropicImageURL(b map[string]json.RawMessage) *OAIImageURL {
	var source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if json.Unmarshal(b["source"], &source) != nil {
		return nil
	}
	if source.URL != "" || source.Type == "url" {
		if source.URL == "" {
			return nil
		}
		return &OAIImageURL{URL: source.URL}
	}
	if source.Data == "" {
		return nil
	}
	if strings.HasPrefix(source.Data, "data:") {
		return &OAIImageURL{URL: source.Data}
	}
	mediaType := source.MediaType
	if mediaType == "" {
		mediaType = "image/png"
	}
	return &OAIImageURL{URL: "data:" + mediaType + ";base64," + source.Data}
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return blockText(raw)
}

func blockText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var b strings.Builder
	for _, x := range blocks {
		var t string
		if json.Unmarshal(x["text"], &t) == nil {
			b.WriteString(t)
		}
	}
	return b.String()
}

type tokenUsage struct {
	InputTokens       int
	OutputTokens      int
	TotalTokens       int
	CachedInputTokens int
	Present           bool
}

func usageFromJSON(raw json.RawMessage) tokenUsage {
	var fields map[string]any
	if len(raw) == 0 || json.Unmarshal(raw, &fields) != nil {
		return tokenUsage{}
	}
	return usageFromFields(fields)
}

func usageFromAnyMap(v any) tokenUsage {
	fields, ok := v.(map[string]any)
	if !ok {
		return tokenUsage{}
	}
	return usageFromFields(fields)
}

func mergeUsage(a, b tokenUsage) tokenUsage {
	if !b.Present {
		return a
	}
	a.Present = true
	if b.InputTokens != 0 {
		a.InputTokens = b.InputTokens
	}
	if b.OutputTokens != 0 {
		a.OutputTokens = b.OutputTokens
	}
	if b.TotalTokens != 0 {
		a.TotalTokens = b.TotalTokens
	}
	if b.CachedInputTokens != 0 {
		a.CachedInputTokens = b.CachedInputTokens
	}
	// ponytail: assumes upstream total == input+output. every current upstream
	// (anthropic, openai) follows this; revisit if one splits tool-use tokens.
	if a.InputTokens > 0 || a.OutputTokens > 0 {
		if sum := a.InputTokens + a.OutputTokens; sum > a.TotalTokens {
			a.TotalTokens = sum
		}
	}
	return a
}

func usageFromFields(fields map[string]any) tokenUsage {
	if len(fields) == 0 {
		return tokenUsage{}
	}
	u := tokenUsage{Present: true}
	u.InputTokens = intField(fields, "prompt_tokens")
	if u.InputTokens == 0 {
		u.InputTokens = intField(fields, "input_tokens")
	}
	u.OutputTokens = intField(fields, "completion_tokens")
	if u.OutputTokens == 0 {
		u.OutputTokens = intField(fields, "output_tokens")
	}
	u.TotalTokens = intField(fields, "total_tokens")
	if u.TotalTokens == 0 && (u.InputTokens > 0 || u.OutputTokens > 0) {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	u.CachedInputTokens = cachedTokens(fields)
	return u
}

func intField(fields map[string]any, name string) int {
	v, ok := fields[name]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}

func cachedTokens(fields map[string]any) int {
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if nested, ok := fields[key].(map[string]any); ok {
			if n := intField(nested, "cached_tokens"); n > 0 {
				return n
			}
		}
	}
	if n := intField(fields, "cache_read_input_tokens"); n > 0 {
		return n
	}
	return intField(fields, "cached_tokens")
}

func anthropicUsage(u tokenUsage) map[string]int {
	usage := map[string]int{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens}
	if u.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = u.CachedInputTokens
	}
	return usage
}

func anthropicDeltaUsage(u tokenUsage) map[string]int {
	usage := map[string]int{"output_tokens": u.OutputTokens}
	if u.InputTokens > 0 {
		usage["input_tokens"] = u.InputTokens
	}
	if u.CachedInputTokens > 0 {
		usage["cache_read_input_tokens"] = u.CachedInputTokens
	}
	return usage
}

func responsesUsage(u tokenUsage) map[string]any {
	usage := map[string]any{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": u.TotalTokens}
	if u.CachedInputTokens > 0 {
		usage["input_tokens_details"] = map[string]int{"cached_tokens": u.CachedInputTokens}
	}
	return usage
}

func openAIUsage(u tokenUsage) map[string]any {
	usage := map[string]any{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.TotalTokens}
	if u.CachedInputTokens > 0 {
		usage["prompt_tokens_details"] = map[string]int{"cached_tokens": u.CachedInputTokens}
	}
	return usage
}

func streamAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"cfgate-cc\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n", model)
	if flusher != nil {
		flusher.Flush()
	}
	textStarted := false
	textIndex := -1
	nextIndex := 0
	toolIndexes := map[int]int{}
	var tools []streamedResponseToolCall
	var reasoning strings.Builder
	usage := tokenUsage{}
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		chunk := parseOpenAIStreamChunk([]byte(data))
		if chunk.Usage.Present {
			usage = chunk.Usage
		}
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			if !textStarted {
				textStarted = true
				textIndex = nextIndex
				nextIndex++
				fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n", textIndex)
			}
			b, _ := json.Marshal(chunk.Content)
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", textIndex, b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, tc := range chunk.ToolCalls {
			toolPos, ok := toolIndexes[tc.Index]
			if !ok {
				callID := tc.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", tc.Index)
				}
				toolPos = len(tools)
				toolIndexes[tc.Index] = toolPos
				blockIndex := nextIndex
				nextIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: blockIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: tc.Name}}})
				idJSON, _ := json.Marshal(callID)
				nameJSON, _ := json.Marshal(tc.Name)
				fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":%s,\"name\":%s,\"input\":{}}}\n\n", blockIndex, idJSON, nameJSON)
			}
			if tc.ID != "" {
				tools[toolPos].Call.ID = tc.ID
			}
			if tc.Name != "" {
				tools[toolPos].Call.Function.Name = tc.Name
			}
			if tc.Arguments != "" {
				tools[toolPos].Call.Function.Arguments += tc.Arguments
				b, _ := json.Marshal(tc.Arguments)
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":%s}}\n\n", tools[toolPos].OutputIndex, b)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	var calls []OAIToolCall
	for _, tool := range tools {
		calls = append(calls, tool.Call)
	}
	cacheReasoningContent(calls, reasoning.String())
	if textStarted {
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", textIndex)
	}
	for _, tool := range tools {
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", tool.OutputIndex)
	}
	stopReason := "end_turn"
	if len(tools) > 0 {
		stopReason = "tool_use"
	}
	usageJSON, _ := json.Marshal(anthropicDeltaUsage(usage))
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q,\"stop_sequence\":null},\"usage\":%s}\n\n", stopReason, usageJSON)
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func openAITextDelta(data []byte) string {
	var v struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(data, &v)
	if len(v.Choices) == 0 {
		return ""
	}
	return v.Choices[0].Delta.Content
}

func writeAnthropicResponse(w http.ResponseWriter, body io.Reader, model string) {
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	text := ""
	if len(v.Choices) > 0 {
		text = v.Choices[0].Message.Content
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "cfgate-cc", "type": "message", "role": "assistant", "model": model, "content": []map[string]string{{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": anthropicUsage(usageFromJSON(v.Usage))})
}

type anthropicParsedResponse struct {
	Text      string
	ToolCalls []OAIToolCall
	Usage     tokenUsage
}

func parseAnthropicResponse(body io.Reader) anthropicParsedResponse {
	var v struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	out := anthropicParsedResponse{Usage: usageFromJSON(v.Usage)}
	var text strings.Builder
	for i, block := range v.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			id := block.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", i)
			}
			args := "{}"
			if len(block.Input) > 0 && string(block.Input) != "null" {
				args = string(block.Input)
			}
			out.ToolCalls = append(out.ToolCalls, OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: block.Name, Arguments: args}})
		}
	}
	out.Text = text.String()
	return out
}

func writeChatCompletionsResponseFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	parsed := parseAnthropicResponse(body)
	msg := map[string]any{"role": "assistant", "content": parsed.Text}
	finishReason := "stop"
	if len(parsed.ToolCalls) > 0 {
		msg["tool_calls"] = parsed.ToolCalls
		finishReason = "tool_calls"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl_cfgate", "object": "chat.completion", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finishReason}}, "usage": openAIUsage(parsed.Usage)})
}

func writeResponsesResponseFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	parsed := parseAnthropicResponse(body)
	var output []any
	if parsed.Text != "" || len(parsed.ToolCalls) == 0 {
		output = append(output, map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": parsed.Text}}})
	}
	for _, call := range parsed.ToolCalls {
		output = append(output, map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_cfgate", "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed", "output": output, "usage": responsesUsage(parsed.Usage)})
}

func streamChatCompletionsFromAnthropic(w http.ResponseWriter, body io.Reader, model string, includeUsage bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	writeChatCompletionChunk(w, model, map[string]any{"role": "assistant"}, nil)
	tools := map[int]streamedResponseToolCall{}
	usage := tokenUsage{}
	readSSE(body, func(_ string, data []byte) bool {
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return true
		}
		typ, _ := v["type"].(string)
		switch typ {
		case "message_start":
			if msg, _ := v["message"].(map[string]any); msg != nil {
				usage = mergeUsage(usage, usageFromAnyMap(msg["usage"]))
			}
		case "content_block_start":
			idx := intFromAny(v["index"])
			block, _ := v["content_block"].(map[string]any)
			if blockType, _ := block["type"].(string); blockType == "tool_use" {
				id, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if id == "" {
					id = fmt.Sprintf("call_%d", idx)
				}
				tools[idx] = streamedResponseToolCall{OutputIndex: len(tools), Call: OAIToolCall{ID: id, Type: "function", Function: OAICallFunction{Name: name}}}
				writeChatCompletionChunk(w, model, map[string]any{"tool_calls": []map[string]any{{"index": tools[idx].OutputIndex, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, nil)
			}
		case "content_block_delta":
			idx := intFromAny(v["index"])
			delta, _ := v["delta"].(map[string]any)
			switch deltaType, _ := delta["type"].(string); deltaType {
			case "text_delta":
				if text, _ := delta["text"].(string); text != "" {
					writeChatCompletionChunk(w, model, map[string]any{"content": text}, nil)
				}
			case "input_json_delta":
				if tool, ok := tools[idx]; ok {
					part, _ := delta["partial_json"].(string)
					tool.Call.Function.Arguments += part
					tools[idx] = tool
					writeChatCompletionChunk(w, model, map[string]any{"tool_calls": []map[string]any{{"index": tool.OutputIndex, "function": map[string]any{"arguments": part}}}}, nil)
				}
			}
		case "message_delta":
			usage = mergeUsage(usage, usageFromAnyMap(v["usage"]))
		case "message_stop":
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	})
	finish := "stop"
	if len(tools) > 0 {
		finish = "tool_calls"
	}
	writeChatCompletionChunk(w, model, map[string]any{}, &finish)
	if includeUsage && usage.Present {
		writeChatCompletionUsageChunk(w, model, openAIUsage(usage))
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeChatCompletionChunk(w io.Writer, model string, delta map[string]any, finishReason *string) {
	choice := map[string]any{"index": 0, "delta": delta}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl_cfgate", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []map[string]any{choice}})
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func writeChatCompletionUsageChunk(w io.Writer, model string, usage map[string]any) {
	b, _ := json.Marshal(map[string]any{"id": "chatcmpl_cfgate", "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{}, "usage": usage})
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func streamResponsesFromAnthropic(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	id := "resp_cfgate"
	writeResponseEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "in_progress", "output": []any{}}})
	if flusher != nil {
		flusher.Flush()
	}
	messageStarted := false
	messageOutputIndex := -1
	nextOutputIndex := 0
	var text strings.Builder
	usage := tokenUsage{}
	blockToTool := map[int]int{}
	var tools []streamedResponseToolCall
	readSSE(body, func(_ string, data []byte) bool {
		var v map[string]any
		if json.Unmarshal(data, &v) != nil {
			return true
		}
		typ, _ := v["type"].(string)
		switch typ {
		case "message_start":
			if msg, _ := v["message"].(map[string]any); msg != nil {
				usage = mergeUsage(usage, usageFromAnyMap(msg["usage"]))
			}
		case "content_block_start":
			idx := intFromAny(v["index"])
			block, _ := v["content_block"].(map[string]any)
			switch blockType, _ := block["type"].(string); blockType {
			case "text":
				if !messageStarted {
					messageStarted = true
					messageOutputIndex = nextOutputIndex
					nextOutputIndex++
					writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []any{}}})
					writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
				}
			case "tool_use":
				callID, _ := block["id"].(string)
				name, _ := block["name"].(string)
				if callID == "" {
					callID = fmt.Sprintf("call_%d", idx)
				}
				toolPos := len(tools)
				blockToTool[idx] = toolPos
				outputIndex := nextOutputIndex
				nextOutputIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: outputIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: name}}})
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": callID, "type": "function_call", "call_id": callID, "name": name, "arguments": ""}})
			}
		case "content_block_delta":
			idx := intFromAny(v["index"])
			delta, _ := v["delta"].(map[string]any)
			switch deltaType, _ := delta["type"].(string); deltaType {
			case "text_delta":
				if part, _ := delta["text"].(string); part != "" {
					if !messageStarted {
						messageStarted = true
						messageOutputIndex = nextOutputIndex
						nextOutputIndex++
						writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []any{}}})
						writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
					}
					text.WriteString(part)
					writeResponseEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "delta": part})
				}
			case "input_json_delta":
				toolPos, ok := blockToTool[idx]
				if ok {
					part, _ := delta["partial_json"].(string)
					tools[toolPos].Call.Function.Arguments += part
					writeResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tools[toolPos].Call.ID, "output_index": tools[toolPos].OutputIndex, "delta": part})
				}
			}
		case "message_delta":
			usage = mergeUsage(usage, usageFromAnyMap(v["usage"]))
		case "message_stop":
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	})
	var output []any
	if messageStarted {
		writeResponseEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "text": text.String()})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}}})
		output = append(output, map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}})
	}
	for _, tool := range tools {
		call := tool.Call
		item := map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
		writeResponseEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": call.ID, "output_index": tool.OutputIndex, "arguments": call.Function.Arguments})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.OutputIndex, "item": item})
		output = append(output, item)
	}
	writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "completed", "output": output, "usage": responsesUsage(usage)}})
}

func readSSE(body io.Reader, handle func(event string, data []byte) bool) {
	// ponytail: default scanner caps tokens at 64KB; cloudflare's responses SSE
	// inlines the full instructions field on response.created/in_progress, which
	// blows past that for any non-trivial system prompt and silently drops the
	// rest of the stream. 32MB is way more than any realistic event.
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 0, 1<<20), 32<<20)
	var event string
	var data []string
	flush := func() bool {
		if len(data) == 0 {
			return true
		}
		payload := strings.Join(data, "\n")
		data = nil
		if payload == "[DONE]" {
			return false
		}
		return handle(event, []byte(payload))
	}
	for s.Scan() {
		line := strings.TrimRight(s.Text(), "\r")
		if line == "" {
			if !flush() {
				return
			}
			event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	_ = flush()
}

func streamResponses(w http.ResponseWriter, body io.Reader, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)
	id := "resp_cfgate"
	writeResponseEvent(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "in_progress", "output": []any{}}})
	if flusher != nil {
		flusher.Flush()
	}
	messageStarted := false
	messageDone := false
	messageOutputIndex := -1
	nextOutputIndex := 0
	var text strings.Builder
	var reasoning strings.Builder
	usage := tokenUsage{}
	toolIndexes := map[int]int{}
	var tools []streamedResponseToolCall
	s := bufio.NewScanner(body)
	s.Buffer(make([]byte, 0, 1<<20), 32<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		chunk := parseOpenAIStreamChunk([]byte(data))
		if chunk.Usage.Present {
			usage = chunk.Usage
		}
		if chunk.ReasoningContent != "" {
			reasoning.WriteString(chunk.ReasoningContent)
		}
		if chunk.Content != "" {
			if !messageStarted {
				messageStarted = true
				messageOutputIndex = nextOutputIndex
				nextOutputIndex++
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []any{}}})
				writeResponseEvent(w, "response.content_part.added", map[string]any{"type": "response.content_part.added", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
			}
			text.WriteString(chunk.Content)
			writeResponseEvent(w, "response.output_text.delta", map[string]any{"type": "response.output_text.delta", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "delta": chunk.Content})
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, tc := range chunk.ToolCalls {
			toolPos, ok := toolIndexes[tc.Index]
			if !ok {
				callID := tc.ID
				if callID == "" {
					callID = fmt.Sprintf("call_%d", tc.Index)
				}
				toolPos = len(tools)
				toolIndexes[tc.Index] = toolPos
				outputIndex := nextOutputIndex
				nextOutputIndex++
				tools = append(tools, streamedResponseToolCall{OutputIndex: outputIndex, Call: OAIToolCall{ID: callID, Type: "function", Function: OAICallFunction{Name: tc.Name}}})
				writeResponseEvent(w, "response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": outputIndex, "item": map[string]any{"id": callID, "type": "function_call", "call_id": callID, "name": tc.Name, "arguments": ""}})
			}
			if tc.ID != "" {
				tools[toolPos].Call.ID = tc.ID
			}
			if tc.Name != "" {
				tools[toolPos].Call.Function.Name = tc.Name
			}
			if tc.Arguments != "" {
				tools[toolPos].Call.Function.Arguments += tc.Arguments
				writeResponseEvent(w, "response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "item_id": tools[toolPos].Call.ID, "output_index": tools[toolPos].OutputIndex, "delta": tc.Arguments})
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	var toolCalls []OAIToolCall
	for _, tool := range tools {
		toolCalls = append(toolCalls, tool.Call)
	}
	cacheReasoningContent(toolCalls, reasoning.String())
	if messageStarted && !messageDone {
		messageDone = true
		writeResponseEvent(w, "response.output_text.done", map[string]any{"type": "response.output_text.done", "item_id": "msg_cfgate", "output_index": messageOutputIndex, "content_index": 0, "text": text.String()})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": messageOutputIndex, "item": map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}}})
	}
	var output []any
	if messageStarted {
		output = append(output, map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text.String()}}})
	}
	for _, tool := range tools {
		call := tool.Call
		item := map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments}
		writeResponseEvent(w, "response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "item_id": call.ID, "output_index": tool.OutputIndex, "arguments": call.Function.Arguments})
		writeResponseEvent(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": tool.OutputIndex, "item": item})
		output = append(output, item)
	}
	writeResponseEvent(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": id, "object": "response", "model": model, "status": "completed", "output": output, "usage": responsesUsage(usage)}})
}

type streamedResponseToolCall struct {
	OutputIndex int
	Call        OAIToolCall
}

type openAIStreamToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type openAIStreamChunk struct {
	Content          string
	ReasoningContent string
	ToolCalls        []openAIStreamToolCall
	Usage            tokenUsage
}

func parseOpenAIStreamChunk(data []byte) openAIStreamChunk {
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(data, &v)
	out := openAIStreamChunk{Usage: usageFromJSON(v.Usage)}
	if len(v.Choices) == 0 {
		return out
	}
	delta := v.Choices[0].Delta
	out.Content = delta.Content
	out.ReasoningContent = delta.ReasoningContent
	for _, tc := range delta.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, openAIStreamToolCall{Index: tc.Index, ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out
}

func writeResponseEvent(w io.Writer, event string, payload any) {
	b, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
}

func writeResponsesResponse(w http.ResponseWriter, body io.Reader, model string) {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string        `json:"content"`
				ReasoningContent string        `json:"reasoning_content"`
				ToolCalls        []OAIToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.NewDecoder(body).Decode(&v)
	text := ""
	var output []any
	if len(v.Choices) > 0 {
		text = v.Choices[0].Message.Content
		if len(v.Choices[0].Message.ToolCalls) > 0 {
			cacheReasoningContent(v.Choices[0].Message.ToolCalls, v.Choices[0].Message.ReasoningContent)
			for _, call := range v.Choices[0].Message.ToolCalls {
				output = append(output, map[string]any{"id": call.ID, "type": "function_call", "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments})
			}
		}
	}
	if len(output) == 0 {
		output = append(output, map[string]any{"id": "msg_cfgate", "type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": text}}})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_cfgate", "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "completed", "output": output, "usage": responsesUsage(usageFromJSON(v.Usage))})
}

func countTokens(w http.ResponseWriter, r *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": 0})
}

func ensureServer(base, providerName string) error {
	if healthy(base) {
		return nil
	}
	if err := startBackground(providerName); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if healthy(base) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("proxy did not start")
}

func startLaunchServer(base, providerName string) (*exec.Cmd, error) {
	if healthy(base) {
		active, _ := readActiveProvider()
		if !shouldRestartForLaunch(active, providerName) {
			if active == "" && providerName != "" {
				fmt.Fprintf(os.Stderr, "cfgate-cc: running proxy has no recorded provider; reusing it instead of switching to %q. run `cfgate-cc stop` to force a restart.\n", providerName)
			}
			return nil, nil
		}
		if err := stopRunningServer(); err != nil {
			return nil, err
		}
		// fall through: spawn a fresh server for the new provider.
	}
	cmd, err := startServerProcess(false, providerName)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		if healthy(base) {
			return cmd, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	stopManagedServer(cmd)
	return nil, errors.New("proxy did not start")
}

// shouldRestartForLaunch reports whether a healthy running server should be
// killed and restarted. an empty active provider means "unknown" (legacy
// startup or first launch) — keep the running server to preserve behavior
// for users upgrading into a config without an active-provider file.
func shouldRestartForLaunch(activeProvider, requested string) bool {
	return activeProvider != "" && activeProvider != requested
}

func readActiveProvider() (string, error) {
	b, err := os.ReadFile(activeProviderFile())
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// stopRunningServer kills the process recorded in pidFile and removes
// both the pid file and the active-provider file. used by
// startLaunchServer when the running server's provider doesn't match
// the requested one. a no-op when there's no pid file.
func stopRunningServer() error {
	pid, err := readPID()
	if err != nil {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	_ = os.Remove(pidFile())
	_ = os.Remove(activeProviderFile())
	if err := p.Kill(); err != nil {
		return err
	}
	_, _ = p.Wait()
	return nil
}

func stopManagedServer(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	_ = os.Remove(pidFile())
	_ = os.Remove(activeProviderFile())
}

func healthy(base string) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(base + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func startBackground(providerName string) error {
	_, err := startServerProcess(true, providerName)
	return err
}

func startServerProcess(detached bool, providerName string) (*exec.Cmd, error) {
	bin, err := os.Executable()
	if err != nil {
		return nil, err
	}
	instDir := instanceDir(resolvedInstanceName)
	if err := os.MkdirAll(instDir, 0755); err != nil {
		return nil, err
	}
	args := []string{"serve"}
	cmd := exec.Command(bin, args...)
	env := os.Environ()
	if providerName != "" {
		// pass the resolved name to the subprocess so its resolveProvider
		// sees the same value. env-wins over single-configured inside the
		// subprocess, so this is the one place the user's --provider flag
		// has to cross the process boundary.
		env = append(env, "CFGATE_CC_PROVIDER="+providerName)
	}
	if resolvedInstanceName != "" {
		// propagate --name to the subprocess so its path helpers and
		// logfile land in the same instance dir as the parent.
		env = append(env, "CFGATE_CC_NAME="+resolvedInstanceName)
	}
	cmd.Env = env
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(instanceLogFile(resolvedInstanceName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.Stdin = nil
	if detached && runtime.GOOS != "windows" {
		cmd.SysProcAttr = detachedAttrs()
	}
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, err
	}
	return cmd, nil
}

// configDir returns the cfgate-cc config dir. honors CFGATE_CC_CONFIG_DIR
// override so smoke tests and per-user installs can redirect it without
// touching $HOME.
func configDir() string {
	if d := os.Getenv("CFGATE_CC_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "cfgate-cc")
}
// writeClaudeSettings writes ANTHROPIC_BASE_URL and ANTHROPIC_AUTH_TOKEN into
// the project's .claude/settings.json env block. the claude-code supervisor
// (and thus agent-view background sessions) reads gateway endpoint vars from
// settings.json, not from the shell, so the launch command's child-process env
// doesn't reach subagents.
func writeClaudeSettings(base, authToken string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	dir := filepath.Join(cwd, ".claude")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var s struct {
		Env map[string]string `json:"env,omitempty"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s)
	}
	if s.Env == nil {
		s.Env = make(map[string]string)
	}
	s.Env["ANTHROPIC_BASE_URL"] = base
	s.Env["ANTHROPIC_AUTH_TOKEN"] = authToken
	raw, err = json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0644)
}

func configFile() string       { return instanceConfigFile(resolvedInstanceName) }
func pidFile() string          { return instancePidFile(resolvedInstanceName) }
func activeProviderFile() string { return instanceActiveProviderFile(resolvedInstanceName) }

var modelMappingFile = func() string { return instanceModelMappingFile(resolvedInstanceName) }

// resolvedInstanceName is the --name / CFGATE_CC_NAME value for the current
// command invocation. resolved once at command entry by resolveInstanceName
// and read by all path helpers so a named instance's state never bleeds
// into the global config dir. empty = back-compat mode, all state under
// configDir() directly.
var resolvedInstanceName string

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
func instanceModelMappingFile(name string) string {
	return filepath.Join(instanceDir(name), "model-mapping.json")
}
func instanceModelMappingMigratedSentinel(name string) string {
	return filepath.Join(instanceDir(name), "model-mapping.migrated")
}
func instanceProviderConfigFile(name, provider string) string {
	return filepath.Join(instanceDir(name), provider+".json")
}

// resolveInstanceBase returns the http base URL for the current instance.
// in back-compat mode (no --name) the base is built from cfg.Host/cfg.Port
// exactly as today. with --name, the port is allocated on first call: if
// the instance config has a port, use it; otherwise scan starting at
// cfg.Port+1 for a free port, write the chosen port back to the instance
// config so the spawned subprocess reads the same value, and return the
// URL. the port is committed BEFORE the subprocess starts so the kernel
// port-bind is the only thing racing the next instance.
//
// ponytail: port scan uses net.Listen("tcp", ":0") — go's stdlib idiom
// for "ask the kernel for a free port". no new dep. closes the listener
// immediately so the port is free for the subprocess to bind. if 100
// consecutive ports are taken, surface that as an error instead of
// silently grabbing something weird.
func resolveInstanceBase(cfg Config) (string, error) {
	if resolvedInstanceName == "" {
		return fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port), nil
	}
	port, err := ensureInstancePort(cfg)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d", cfg.Host, port), nil
}

func ensureInstancePort(cfg Config) (int, error) {
	// ponytail: persisted port lives in `port`, not config.json. if it's
	// there, trust it — even if the kernel says it's busy (a healthy
	// server is bound to it, which is exactly the case we want). never
	// re-scan and overwrite: that loses the original port and points
	// the next caller at a port with no server on it.
	if p := readInstancePort(instanceDir(resolvedInstanceName)); p > 0 {
		return p, nil
	}
	// no persisted port (fresh instance): scan from defaultPort+1.
	// never squat on the legacy base port — that's reserved for
	// back-compat (no --name) mode.
	for p := defaultPort + 1; p <= defaultPort+100; p++ {
		if isPortFree(p) {
			if err := saveInstancePort(p); err != nil {
				return 0, err
			}
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port found in %d..%d", defaultPort+1, defaultPort+100)
}

// isPortFree reports whether the kernel can bind a tcp listener on port.
// ponytail: bind to 127.0.0.1:<port> specifically — macOS lets [::]:port
// and 127.0.0.1:port coexist (separate IPv4/IPv6 sockets), so the more
// obvious ":port" form reports a free port when a v4 loopback listener
// is already there. cfgate-cc serves on 127.0.0.1, so we test the same
// address family.
func isPortFree(port int) bool {
	if port <= 0 {
		return false
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// resolveInstanceName reads --name (or CFGATE_CC_NAME) from the command
// invocation. precedence: explicit --name flag > CFGATE_CC_NAME env > empty.
// empty = back-compat, single-tenant mode. checks both Local and
// Persistent flags because --name is a PersistentFlag on the root, and
// cobra's cmd.Flags() returns only Local. falls back to env-only when
// cmd is nil (e.g. inside a subprocess that re-resolves before serving).
// stored in resolvedInstanceName so path helpers don't need to thread
// the value through every signature.
func resolveInstanceName(cmd *cobra.Command) string {
	if cmd != nil {
		for _, set := range []*pflag.FlagSet{cmd.Flags(), cmd.PersistentFlags()} {
			if f := set.Lookup("name"); f != nil {
				if v := strings.TrimSpace(f.Value.String()); v != "" {
					resolvedInstanceName = v
					return v
				}
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("CFGATE_CC_NAME")); v != "" {
		resolvedInstanceName = v
		return v
	}
	resolvedInstanceName = ""
	return ""
}

// autoInstanceName returns a unique-per-launch name like "oc-a3f2" for
// ad-hoc `launch` invocations that didn't pass --name. ponytail: 2 random
// bytes (16 bits) is enough for the "two tabs" use case — collision
// probability is ~1/65k per pair. if you spin up thousands of ad-hoc
// instances, the math changes; bump the byte count or check for
// collisions before then.
func autoInstanceName(provider string) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is a broken OS. fall back to a pid-based
		// suffix so we still get a unique name; never return "".
		return fmt.Sprintf("%s-x%x", provider, os.Getpid())
	}
	return fmt.Sprintf("%s-%s", provider, hex.EncodeToString(b[:]))
}

// codexProfileFilename picks the per-instance codex profile filename. empty
// name = the default profile name, shared with single-tenant installs.
// named instance = "<base>-<name>.config.toml" so two instances can each
// own a profile in ~/.codex/ without clobbering each other.
func codexProfileFilename(name string) string {
	if name == "" {
		return codexProfileName + ".config.toml"
	}
	return codexProfileName + "-" + name + ".config.toml"
}

// codexProfileNameFor returns the codex profile name (used in --profile
// and in [profiles.<name>] / [model_providers.<name>] section headings).
// per-instance: two named cfgate-ccs each get a distinct profile name in
// the shared ~/.codex/config.toml. empty = the original single-tenant name.
func codexProfileNameFor(name string) string {
	if name == "" {
		return codexProfileName
	}
	return codexProfileName + "-" + name
}

func codexConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func codexProfileConfigFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", codexProfileFilename(resolvedInstanceName))
}

func codexModelCatalogFile() string {
	return codexModelCatalogFileFor(resolvedInstanceName)
}

func codexModelCatalogFileFor(name string) string {
	home, _ := os.UserHomeDir()
	if name == "" {
		return filepath.Join(home, ".codex", "cfgate-cc-models.json")
	}
	return filepath.Join(home, ".codex", "cfgate-cc-models-"+name+".json")
}

func ensureCodexConfig(base string, p ProviderConfig) error {
	path := codexConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := writeCodexModelCatalog(codexModelCatalogFile(), p); err != nil {
		return err
	}
	return writeCodexProfile(path, strings.TrimRight(base, "/")+"/v1/", resolvedInstanceName)
}

func writeCodexProfile(path, baseURL, instanceName string) error {
	profilePath := filepath.Join(filepath.Dir(path), codexProfileFilename(instanceName))
	catalogPath := filepath.Join(filepath.Dir(path), filepath.Base(codexModelCatalogFileFor(instanceName)))
	profileName := codexProfileNameFor(instanceName)
	profileText := strings.Join([]string{
		fmt.Sprintf("openai_base_url = %q", baseURL),
		`forced_login_method = "api"`,
		fmt.Sprintf("model_provider = %q", profileName),
		fmt.Sprintf("model_catalog_json = %q", catalogPath),
		`model_reasoning_effort = "minimal"`,
		`model_reasoning_summary = "none"`,
		"",
		fmt.Sprintf("[model_providers.%s]", profileName),
		`name = "Upstream"`,
		fmt.Sprintf("base_url = %q", baseURL),
		`wire_api = "responses"`,
		"",
	}, "\n")
	if err := os.WriteFile(profilePath, []byte(profileText), 0644); err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	text := ""
	if err == nil {
		text = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cleaned := stripLegacyCodexProfile(text, instanceName)
	return os.WriteFile(path, []byte(cleaned), 0644)
}

func stripLegacyCodexProfile(text, instanceName string) string {
	target := codexProfileNameFor(instanceName)
	var out []string
	inRemovedSection := false
	currentSection := ""
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			currentSection = trimmed
			inRemovedSection = isLegacyCodexProfileSection(currentSection, target)
			if inRemovedSection {
				continue
			}
		}
		if inRemovedSection {
			continue
		}
		if currentSection == "" && strings.HasPrefix(trimmed, "profile") {
			parts := strings.SplitN(trimmed, "=", 2)
			if len(parts) == 2 && strings.TrimSpace(parts[0]) == "profile" {
				val := strings.Trim(strings.TrimSpace(parts[1]), `"'`)
				if val == target {
					continue
				}
			}
		}
		out = append(out, line)
	}
	return strings.TrimLeft(strings.Join(out, "\n"), "\n")
}

func isLegacyCodexProfileSection(section, target string) bool {
	// strip stale sections for the cfgate-cc profile name in case a user
	// is upgrading and the prior install wrote the old name into their
	// ~/.codex/config.toml. for named instances, target is the per-
	// instance profile name; we only strip sections matching this run's
	// profile so peer instances' profiles in the same config.toml stay.
	name := target
	profiles := fmt.Sprintf("[profiles.%s", name)
	providers := fmt.Sprintf("[model_providers.%s", name)
	if section == fmt.Sprintf("[profiles.%s]", name) ||
		strings.HasPrefix(section, profiles+".") ||
		section == fmt.Sprintf("[model_providers.%s]", name) ||
		strings.HasPrefix(section, providers+".") {
		return true
	}
	return false
}

func writeCodexModelCatalog(path string, p ProviderConfig) error {
	mappings, err := loadModelMappingsForProvider(p.Name)
	if err != nil {
		mappings = defaultModelMappings()
	}
	providerIDs, err := providerKnownModelIDs(p.Name, p)
	if err != nil {
		return err
	}
	models := make([]map[string]any, 0, len(providerIDs)+len(mappings["codex"]))
	seen := map[string]bool{}
	addModel := func(id, target, description string, i int) {
		if seen[id] {
			return
		}
		seen[id] = true
		meta := modelMetadata(target)
		displayName := id
		if id == target {
			displayName = meta.DisplayName
		}
		models = append(models, map[string]any{
			"slug":                             id,
			"display_name":                     displayName,
			"description":                      description,
			"default_reasoning_level":          meta.DefaultReasoningLevel,
			"supported_reasoning_levels":       meta.SupportedReasoning,
			"shell_type":                       "shell_command",
			"visibility":                       "list",
			"supported_in_api":                 true,
			"priority":                         i,
			"availability_nux":                 nil,
			"upgrade":                          nil,
			"base_instructions":                "You are Codex, a coding agent running in a terminal-based coding assistant.",
			"supports_reasoning_summaries":     meta.ReasoningSummaries,
			"default_reasoning_summary":        meta.DefaultReasoningSummary,
			"support_verbosity":                false,
			"default_verbosity":                nil,
			"apply_patch_tool_type":            nil,
			"web_search_tool_type":             "text",
			"truncation_policy":                map[string]any{"mode": "tokens", "limit": 10000},
			"supports_parallel_tool_calls":     meta.ParallelToolCalls,
			"supports_image_detail_original":   meta.SupportsImageOriginal,
			"context_window":                   meta.ContextWindow,
			"max_context_window":               meta.MaxContextWindow,
			"auto_compact_token_limit":         nil,
			"effective_context_window_percent": 95,
			"experimental_supported_tools":     []any{},
			"input_modalities":                 meta.CodexInputModalities,
			"supports_search_tool":             meta.SupportsSearchTool,
		})
	}
	for i, id := range providerIDs {
		addModel(id, id, modelMetadata(id).Description, i)
	}
	keys := make([]string, 0, len(mappings["codex"]))
	for source := range mappings["codex"] {
		keys = append(keys, source)
	}
	sort.Strings(keys)
	for i, source := range keys {
		target := mappings["codex"][source]
		knownIDs, _, _ := knownModelIDs()
		addModel(source, target, "cfgate-cc mapping to "+target, len(knownIDs)+i)
	}
	b, err := json.MarshalIndent(map[string]any{"models": models}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0644)
}

func checkCodexVersion() error {
	if _, err := exec.LookPath("codex"); err != nil {
		return fmt.Errorf("codex is not installed, install with: npm install -g @openai/codex")
	}
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return fmt.Errorf("failed to get codex version: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return fmt.Errorf("unexpected codex version output: %s", string(out))
	}
	version := fields[len(fields)-1]
	if compareVersions(version, "0.81.0") < 0 {
		return fmt.Errorf("codex version %s is too old, minimum required is 0.81.0; update with: npm update -g @openai/codex", version)
	}
	return nil
}

func compareVersions(a, b string) int {
	ap, bp := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if ap[i] > bp[i] {
			return 1
		}
		if ap[i] < bp[i] {
			return -1
		}
	}
	return 0
}

func versionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	fields := strings.Split(v, ".")
	var out [3]int
	for i := 0; i < len(fields) && i < 3; i++ {
		part := fields[i]
		for j, r := range part {
			if r < '0' || r > '9' {
				part = part[:j]
				break
			}
		}
		out[i], _ = strconv.Atoi(part)
	}
	return out
}

// loadConfig returns the slimmed local-proxy config. upstream fields moved
// to per-provider files; see loadActiveProvider.
func loadConfig() (Config, error) {
	migrateConfigIfNeeded()
	migrateCloudflareURLIfNeeded()
	cfg := Config{
		Host: defaultHost,
		Port: defaultPort,
	}
	b, err := os.ReadFile(configFile())
	if err == nil {
		if bytes.Contains(b, []byte(`"api_key"`)) {
			fmt.Fprintln(os.Stderr, "cfgate-cc: config.json contains api_key which is no longer used; remove it to silence this warning.")
		}
		_ = json.Unmarshal(b, &cfg)
	}
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	return cfg, nil
}

// providerConfigFile returns the path to the per-provider config file.
// ponytail: filename pattern fixed; do not derive from a user input.
func providerConfigFile(name string) string {
	return instanceProviderConfigFile(resolvedInstanceName, name)
}

func loadProviderConfig(name string) (ProviderConfig, error) {
	var p ProviderConfig
	path := providerConfigFile(name)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && resolvedInstanceName != "" {
		// ad-hoc (auto-named) instance: the provider config lives in the
		// base configDir, not in the per-instance dir. fall back so the
		// `launch` flow works without requiring `setup --name <name>`.
		path = filepath.Join(configDir(), name+".json")
		b, err = os.ReadFile(path)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil
		}
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("parse %s: %w", path, err)
	}
	return p, nil
}

func saveProviderConfig(name string, p ProviderConfig) error {
	if !isKnownProvider(name) {
		return fmt.Errorf("unknown provider %q", name)
	}
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.WriteFile(providerConfigFile(name), append(b, '\n'), 0600); err != nil {
		return err
	}
	fmt.Printf("Saved provider %q to %s\n", name, providerConfigFile(name))
	return nil
}

// listConfiguredProviders returns provider names that have a config file
// present. used by resolveProvider for the "single configured wins" rule.
func listConfiguredProviders() ([]string, error) {
	var out []string
	for _, name := range knownProviders {
		// ad-hoc instance: the per-instance dir won't have the provider
		// config; the base configDir does. check both, instance first so
		// a per-instance config can shadow the base.
		candidates := []string{providerConfigFile(name)}
		if resolvedInstanceName != "" {
			candidates = append(candidates, filepath.Join(configDir(), name+".json"))
		}
		found := false
		for _, p := range candidates {
			if _, err := os.Stat(p); err == nil {
				found = true
				break
			} else if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
		if found {
			out = append(out, name)
		}
	}
	return out, nil
}

// resolveProvider picks a provider name from the four precedence sources:
// --provider flag > $CFGATE_CC_PROVIDER > single configured provider > error.
// returns an error when none of the four yield a name.
func resolveProvider(cmd *cobra.Command) (string, error) {
	if cmd != nil {
		if f := cmd.Flags().Lookup("provider"); f != nil {
			if v := strings.TrimSpace(f.Value.String()); v != "" {
				if !isKnownProvider(v) {
					return "", fmt.Errorf("unknown --provider %q (known: %s)", v, strings.Join(knownProviders, ", "))
				}
				return v, nil
			}
		}
	}
	if v := strings.TrimSpace(os.Getenv("CFGATE_CC_PROVIDER")); v != "" {
		if !isKnownProvider(v) {
			return "", fmt.Errorf("unknown $CFGATE_CC_PROVIDER %q (known: %s)", v, strings.Join(knownProviders, ", "))
		}
		return v, nil
	}
	names, err := listConfiguredProviders()
	if err != nil {
		return "", err
	}
	switch len(names) {
	case 0:
		return "", errors.New("no provider configured; run `cfgate-cc setup opencode-go` or `cfgate-cc setup cloudflare`")
	case 1:
		return names[0], nil
	default:
		return "", fmt.Errorf("multiple providers configured (%s); pass --provider or set $CFGATE_CC_PROVIDER", strings.Join(names, ", "))
	}
}

// loadActiveProvider returns the provider config for name, with
// CFGATE_CC_UPSTREAM_* env vars applied on top so the fish-alias pattern
// (env-overrides-file) still works for the active provider. sets Name
// so downstream code (proxy handlers, list dispatch) can identify the
// provider without re-resolving it.
func loadActiveProvider(name string) (ProviderConfig, error) {
	p, err := loadProviderConfig(name)
	if err != nil {
		return p, err
	}
	p.Name = name
	if v := os.Getenv("CFGATE_CC_UPSTREAM_BASE_URL"); v != "" {
		p.UpstreamBaseURL = v
	}
	if v := os.Getenv("CFGATE_CC_UPSTREAM_API_KEY"); v != "" {
		p.UpstreamAPIKey = v
	}
	if v := os.Getenv("CFGATE_CC_UPSTREAM_AUTH"); v != "" {
		p.UpstreamAuth = v
	}
	if v := os.Getenv("CFGATE_CC_UPSTREAM_AUTH_HDR"); v != "" {
		p.UpstreamAuthHdr = v
	}
	if raw := os.Getenv("CFGATE_CC_UPSTREAM_EXTRA_HDR"); raw != "" {
		var hdrs map[string]string
		if err := json.Unmarshal([]byte(raw), &hdrs); err == nil {
			p.UpstreamExtraHdr = hdrs
		}
	}
	return p, nil
}

// migrateConfigIfNeeded is a one-shot upgrade helper. if config.json still
// carries upstream_* fields from the pre-split era, move them into the
// matching per-provider file and strip them from config.json. no-op when
// config.json is already slim or when the target provider file already
// exists (caller wins; no clobber).
func migrateConfigIfNeeded() {
	b, err := os.ReadFile(configFile())
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	// any upstream_* field counts as "old config". opencode-go users
	// sometimes had upstream_api_key set with no upstream_base_url.
	hasUpstream := false
	for _, k := range []string{"upstream_base_url", "upstream_api_key", "upstream_auth", "upstream_auth_hdr", "upstream_extra_hdr", "endpoint_overrides"} {
		if _, ok := raw[k]; ok {
			hasUpstream = true
			break
		}
	}
	if !hasUpstream {
		return
	}
	var url string
	if v, ok := raw["upstream_base_url"]; ok {
		_ = json.Unmarshal(v, &url)
	}
	name := providerForUpstreamURL(url)
	if _, err := os.Stat(providerConfigFile(name)); err == nil {
		// target file exists, leave config.json alone. the user has two
		// configs to reconcile; we'd rather not silently overwrite.
		return
	}
	p := ProviderConfig{}
	if v, ok := raw["upstream_api_key"]; ok {
		_ = json.Unmarshal(v, &p.UpstreamAPIKey)
	}
	if v, ok := raw["upstream_auth"]; ok {
		_ = json.Unmarshal(v, &p.UpstreamAuth)
	}
	if v, ok := raw["upstream_auth_hdr"]; ok {
		_ = json.Unmarshal(v, &p.UpstreamAuthHdr)
	}
	if v, ok := raw["upstream_extra_hdr"]; ok {
		_ = json.Unmarshal(v, &p.UpstreamExtraHdr)
	}
	if v, ok := raw["endpoint_overrides"]; ok {
		_ = json.Unmarshal(v, &p.EndpointOverrides)
	}
	p.UpstreamBaseURL = url
	if err := os.MkdirAll(configDir(), 0755); err != nil {
		fmt.Fprintln(os.Stderr, "config migration: mkdir:", err)
		return
	}
	if err := saveProviderConfig(name, p); err != nil {
		fmt.Fprintln(os.Stderr, "config migration: save provider:", err)
		return
	}
	delete(raw, "upstream_base_url")
	delete(raw, "upstream_api_key")
	delete(raw, "upstream_auth")
	delete(raw, "upstream_auth_hdr")
	delete(raw, "upstream_extra_hdr")
	delete(raw, "endpoint_overrides")
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config migration: marshal config:", err)
		return
	}
	if err := os.WriteFile(configFile(), append(out, '\n'), 0600); err != nil {
		fmt.Fprintln(os.Stderr, "config migration: write config:", err)
	}
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

// migrateCloudflareURLIfNeeded rewrites a cloudflare.json that still
// points at the deprecated /compat/v1 URL into the new REST API URL,
// pulling the gateway id out of the path and into ProviderConfig.Gateway
// (the new shape uses a cf-aig-gateway-id header instead).
// idempotent: a no-op once the URL is already on the REST API.
//
// also back-fills ProviderConfig.Account from the REST URL when the field
// is missing — required by the /openai native endpoint which doesn't carry
// the account id in the path.
func migrateCloudflareURLIfNeeded() {
	const oldPrefix = "https://gateway.ai.cloudflare.com/v1/"
	path := providerConfigFile("cloudflare")
	p, err := loadProviderConfig("cloudflare")
	if err != nil || (!strings.HasPrefix(p.UpstreamBaseURL, oldPrefix) && !strings.HasPrefix(p.UpstreamBaseURL, cloudflareUpstreamPrefix)) {
		return
	}
	dirty := false
	if strings.HasPrefix(p.UpstreamBaseURL, oldPrefix) {
		rest := strings.TrimPrefix(p.UpstreamBaseURL, oldPrefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return
		}
		p.UpstreamBaseURL = buildCloudflareURL(parts[0])
		p.Gateway = parts[1]
		dirty = true
	}
	if p.Account == "" && strings.HasPrefix(p.UpstreamBaseURL, cloudflareUpstreamPrefix) {
		rest := strings.TrimPrefix(p.UpstreamBaseURL, cloudflareUpstreamPrefix)
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) > 0 && parts[0] != "" {
			p.Account = parts[0]
			dirty = true
		}
	}
	if !dirty {
		return
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(b, '\n'), 0600)
}

func readPID() (int, error) {
	b, err := os.ReadFile(pidFile())
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscan(string(b), &pid)
	return pid, err
}

// ponytail: ~120 lines inline, keeps the single-file layout. lift into
// debug.go if a second consumer of CFGATE_CC_DEBUG ever appears.
var debugEnabled bool

const (
	debugMaxBodyBytes  = 1 << 20
	debugStreamSamples = 2
)

func setupDebug() {
	v := os.Getenv("CFGATE_CC_DEBUG")
	if v == "" {
		return
	}
	var w io.Writer = os.Stderr
	if v != "1" {
		f, err := os.OpenFile(v, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "cfgate-cc: CFGATE_CC_DEBUG=%q: %v\n", v, err)
			return
		}
		w = f
	}
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags)
	debugEnabled = true
	fmt.Fprintf(os.Stderr, "debug log: %s\n", v)
}

func dlogf(format string, args ...any) {
	if !debugEnabled {
		return
	}
	log.Printf(format, args...)
}

func redactHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "x-api-key", "cookie":
		return true
	}
	return false
}

func dlogHeaders(h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := "[REDACTED]"
		if !redactHeader(k) {
			v = strings.Join(h[k], ", ")
		}
		dlogf("    %s: %s", k, v)
	}
}

func dlogBody(label string, body []byte) {
	switch {
	case len(body) == 0:
		dlogf("    %s body: (empty)", label)
	case len(body) > debugMaxBodyBytes:
		out := make([]byte, 0, debugMaxBodyBytes+64)
		out = append(out, body[:debugMaxBodyBytes]...)
		out = append(out, fmt.Sprintf("\n[truncated: %d bytes total]", len(body))...)
		dlogf("    %s body: %s", label, out)
	default:
		dlogf("    %s body: %s", label, body)
	}
}

func dlogIncoming(label string, r *http.Request, body []byte) {
	dlogf("[%s] incoming %s %s from %s", label, r.Method, r.URL.Path, r.RemoteAddr)
	dlogHeaders(r.Header)
	dlogBody("incoming", body)
}

func dlogUpstreamReq(req *http.Request, body []byte) {
	dlogf("[upstream] %s %s", req.Method, req.URL.String())
	dlogHeaders(req.Header)
	dlogBody("upstream", body)
}

func dlogUpstreamResp(resp *http.Response) {
	dlogf("[upstream] response %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	dlogf("    content-length: %s", resp.Header.Get("Content-Length"))
	dlogf("    transfer-encoding: %s", resp.Header.Get("Transfer-Encoding"))
}

func dlogClientResp(label string, status int) {
	dlogf("[%s] client response: %d", label, status)
}

func dlogHandlerErr(label string, err error, status int) {
	dlogf("[%s] error: %v (status %d)", label, err, status)
}

func isSSEResponse(resp *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// streamReader wraps an SSE response body, tracking bytes + event count
// and capturing the first/last N events verbatim. on EOF it logs a
// summary. pass it in place of resp.Body to streamAnthropic and friends —
// no signature change.
type streamReader struct {
	r       io.Reader
	label   string
	start   time.Time
	bytes   int64
	pending []byte
	first   []string
	last    []string
	total   int
	logged  bool
}

func (s *streamReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && debugEnabled {
		s.bytes += int64(n)
		s.scan(p[:n])
	}
	if err == io.EOF && debugEnabled && !s.logged {
		s.logged = true
		s.logEnd()
	}
	return n, err
}

func (s *streamReader) scan(buf []byte) {
	s.pending = append(s.pending, buf...)
	for {
		idx := bytes.Index(s.pending, []byte("\n\n"))
		if idx < 0 {
			return
		}
		s.recordEvent(string(s.pending[:idx]))
		s.pending = s.pending[idx+2:]
	}
}

func (s *streamReader) recordEvent(e string) {
	s.total++
	if s.total <= debugStreamSamples {
		s.first = append(s.first, e)
		return
	}
	s.last = append(s.last, e)
	if len(s.last) > debugStreamSamples {
		s.last = s.last[1:]
	}
}

func (s *streamReader) logEnd() {
	dlogf("[%s] stream end: events=%d bytes=%d elapsed=%s", s.label, s.total, s.bytes, time.Since(s.start).Round(time.Millisecond))
	for i, e := range s.first {
		dlogf("    event[first/%d]: %s", i+1, e)
	}
	base := s.total - len(s.last)
	for i, e := range s.last {
		dlogf("    event[last/%d]: %s", base+i+1, e)
	}
}
