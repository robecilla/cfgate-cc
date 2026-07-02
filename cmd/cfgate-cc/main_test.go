package main

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// testProviderCfg is the default cfg passed to helpers that need a provider
// identity to read the right mapping section / model list. tests that don't
// care about provider-specific behavior use this; tests that do (e.g. cloudflare
// dispatch) construct their own.
var testProviderCfg = ProviderConfig{Name: "opencode-go"}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cfgate-cc-test-*")
	if err != nil {
		panic(err)
	}
	old := modelMappingFile
	oldRemoteModels := remoteModels
	oldOfficialModels := officialModels
	modelMappingFile = func() string { return filepath.Join(dir, "model-mapping.json") }
	remoteModels = newLazyFetcher(func() (map[string]remoteModelInfo, error) {
		return nil, errors.New("remote model fetch disabled in tests")
	})
	officialModels = newLazyFetcher(func() ([]string, error) { return nil, errors.New("official model fetch disabled in tests") })
	code := m.Run()
	modelMappingFile = old
	remoteModels = oldRemoteModels
	officialModels = oldOfficialModels
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestWriteCodexProfile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := writeCodexProfile(path, "http://127.0.0.1:3456/v1/"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cfgate-cc-launch.config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{
		`openai_base_url = "http://127.0.0.1:3456/v1/"`,
		`forced_login_method = "api"`,
		`model_provider = "cfgate-cc-launch"`,
		`model_catalog_json = `,
		`model_reasoning_effort = "minimal"`,
		`model_reasoning_summary = "none"`,
		"[model_providers.cfgate-cc-launch]",
		`name = "Upstream"`,
		`base_url = "http://127.0.0.1:3456/v1/"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in:\n%s", want, content)
		}
	}
	if strings.Contains(content, "[profiles.cfgate-cc-launch]") {
		t.Fatalf("new Codex profile file must not contain legacy [profiles] table:\n%s", content)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != "" {
		t.Fatalf("root Codex config should not contain stale cfgate-cc-launch profile entries:\n%s", string(b))
	}
}

func TestWriteCodexProfileMigratesLegacySections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := "profile = \"cfgate-cc-launch\"\nkeep = \"top\"\n\n[profiles.cfgate-cc-launch]\nopenai_base_url = \"http://old/v1/\"\n\n[profiles.cfgate-cc-launch.features]\nmemories = false\n\n[other]\nkey = \"value\"\n\n[model_providers.cfgate-cc-launch]\nbase_url = \"http://old/v1/\"\n\n[model_providers.cfgate-cc-launch.headers]\nfoo = \"bar\"\n"
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeCodexProfile(path, "http://new/v1/"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	content := string(b)
	for _, gone := range []string{"http://old", `profile = "cfgate-cc-launch"`} {
		if strings.Contains(content, gone) {
			t.Fatalf("legacy Codex profile config %q was not removed:\n%s", gone, content)
		}
	}
	for _, gone := range []string{"[profiles.cfgate-cc-launch]", "[profiles.cfgate-cc-launch.features]", "[model_providers.cfgate-cc-launch]", "[model_providers.cfgate-cc-launch.headers]", `openai_base_url = "http://new/v1/"`} {
		if strings.Contains(content, gone) {
			t.Fatalf("legacy Codex profile config %q was re-added:\n%s", gone, content)
		}
	}
	if !strings.Contains(content, `keep = "top"`) || !strings.Contains(content, "[other]") || !strings.Contains(content, `key = "value"`) {
		t.Fatalf("unrelated section was not preserved:\n%s", content)
	}
	profile, _ := os.ReadFile(filepath.Join(dir, "cfgate-cc-launch.config.toml"))
	if !strings.Contains(string(profile), `openai_base_url = "http://new/v1/"`) || !strings.Contains(string(profile), "[model_providers.cfgate-cc-launch]") {
		t.Fatalf("new profile file was not written correctly:\n%s", string(profile))
	}
}

func TestWriteCodexModelCatalog(t *testing.T) {
	withTempModelMappingFile(t, filepath.Join(t.TempDir(), "model-mapping.json"))
	// feed the opencode-go model set through the remote fetcher so the
	// catalog writer has a deterministic set to work with.
	withModelFetchers(t, map[string]remoteModelInfo{
		"deepseek-v4-pro": testRemoteModel("DeepSeek V4 Pro", 128000, "text"),
		"qwen3.7-max":     testRemoteModel("Qwen 3.7 Max", 128000, "text"),
		"minimax-m3":      testRemoteModel("MiniMax M3", 512000, "text", "image"),
	}, nil)
	path := filepath.Join(t.TempDir(), "cfgate-cc-models.json")
	if err := writeCodexModelCatalog(path, testProviderCfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(b)
	for _, want := range []string{`"models"`, `"slug": "deepseek-v4-pro"`, `"slug": "qwen3.7-max"`, `"slug": "minimax-m3"`, `"context_window": 128000`, `"truncation_policy"`, `"supports_image_detail_original": false`, `"image"`} {
		if !strings.Contains(content, want) {
			t.Fatalf("missing %q in:\n%s", want, content)
		}
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(b, &catalog); err != nil {
		t.Fatal(err)
	}
	var minimax map[string]any
	var qwen map[string]any
	for _, model := range catalog.Models {
		switch model["slug"] {
		case "minimax-m3":
			minimax = model
		case "qwen3.7-max":
			qwen = model
		}
	}
	if minimax == nil {
		t.Fatal("minimax-m3 not found in catalog")
	}
	if got := int(minimax["context_window"].(float64)); got != 512000 {
		t.Fatalf("minimax-m3 context_window = %d, want 512000", got)
	}
	if got := minimax["display_name"]; got != "MiniMax M3" {
		t.Fatalf("minimax-m3 display_name = %v, want MiniMax M3", got)
	}
	modalities := fmt.Sprint(minimax["input_modalities"])
	for _, want := range []string{"text", "image"} {
		if !strings.Contains(modalities, want) {
			t.Fatalf("minimax-m3 modalities missing %s: %v", want, minimax["input_modalities"])
		}
	}
	if strings.Contains(modalities, "video") {
		t.Fatalf("Codex catalog modalities must not include unsupported video modality: %v", minimax["input_modalities"])
	}
	if qwen == nil {
		t.Fatal("qwen3.7-max not found in catalog")
	}
	if got := qwen["supports_search_tool"]; got != true {
		t.Fatalf("qwen3.7-max supports_search_tool = %v, want true", got)
	}
}

func TestModelMappingsLoadSaveAndResolve(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)

	m, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveMappedModel("claude", "claude-sonnet-4-5", m); got != "claude-sonnet-4-5" {
		t.Fatalf("unconfigured claude model should pass through, got %q", got)
	}
	m["claude"]["claude-sonnet"] = "kimi-k2.6"
	m["claude"]["claude-sonnet-4-5"] = "qwen3.7-max"
	m["codex"]["gpt-5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveMappedModel("claude", "claude-sonnet-4-5", reloaded); got != "qwen3.7-max" {
		t.Fatalf("custom claude mapping = %q", got)
	}
	if got := resolveMappedModel("codex", "gpt-5", reloaded); got != "deepseek-v4-pro" {
		t.Fatalf("custom codex mapping = %q", got)
	}
}

func TestPrepareChatBodyAppliesCodexMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)
	m := defaultModelMappings()
	m["codex"]["gpt-5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	body, err := prepareChatBody([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`), testProviderCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"model":"deepseek-v4-pro"`) {
		t.Fatalf("mapping was not applied: %s", string(body))
	}
}

func TestMappingUnsetCommandRemovesMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)
	t.Setenv("CFGATE_CC_PROVIDER", "opencode-go")
	m := defaultModelMappings()
	m["codex"]["gpt-5.5"] = "deepseek-v4-pro"
	if err := saveModelMappings(m); err != nil {
		t.Fatal(err)
	}
	cmd := toolMappingCmd("codex")
	cmd.SetArgs([]string{"unset", "gpt-5.5"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded["codex"]["gpt-5.5"]; ok {
		t.Fatalf("mapping was not removed: %+v", reloaded["codex"])
	}
}

func withTempModelMappingFile(t *testing.T, path string) {
	t.Helper()
	old := modelMappingFile
	modelMappingFile = func() string { return path }
	t.Cleanup(func() { modelMappingFile = old })
}

func withModelFetchers(t *testing.T, remote map[string]remoteModelInfo, official []string) {
	t.Helper()
	oldRemoteModels := remoteModels
	oldOfficialModels := officialModels
	remoteModels = newLazyFetcher(func() (map[string]remoteModelInfo, error) {
		if remote == nil {
			return nil, errors.New("remote unavailable")
		}
		return remote, nil
	})
	officialModels = newLazyFetcher(func() ([]string, error) {
		if official == nil {
			return nil, errors.New("official unavailable")
		}
		return official, nil
	})
	t.Cleanup(func() {
		remoteModels = oldRemoteModels
		officialModels = oldOfficialModels
	})
}

func testRemoteModel(name string, contextWindow int, modalities ...string) remoteModelInfo {
	var m remoteModelInfo
	m.Name = name
	m.Limit.Context = contextWindow
	m.Modalities.Input = append([]string(nil), modalities...)
	return m
}

func TestKnownModelIDsPreferOfficialThenRemoteThenFallback(t *testing.T) {
	withModelFetchers(t, map[string]remoteModelInfo{"remote-b": {}, "remote-a": {}}, []string{"official-b", "official-a"})
	if got, usedCache, err := knownModelIDs(); err != nil || usedCache || strings.Join(got, ",") != "official-a,official-b" {
		t.Fatalf("official IDs = %v, usedCache = %v, err = %v", got, usedCache, err)
	}

	withModelFetchers(t, map[string]remoteModelInfo{"remote-b": {}, "remote-a": {}}, nil)
	if got, usedCache, err := knownModelIDs(); err != nil || usedCache || strings.Join(got, ",") != "remote-a,remote-b" {
		t.Fatalf("remote fallback IDs = %v, usedCache = %v, err = %v", got, usedCache, err)
	}

	withModelFetchers(t, nil, nil)
	if got, usedCache, err := knownModelIDs(); err == nil || usedCache || len(got) != 0 {
		t.Fatalf("expected (nil, false, err); got (%v, %v, %v)", got, usedCache, err)
	}
}

func TestListProviderDispatch(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "")

	t.Run("opencode-go uses known chain", func(t *testing.T) {
		withModelFetchers(t, nil, []string{"minimax-m3", "kimi-k2.6"})
		if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(dir, "opencode-go.json"))

		cmd := listCmd()
		outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.SetOut(outBuf)
		cmd.SetErr(errBuf)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("list opencode-go: %v", err)
		}
		if !strings.Contains(outBuf.String(), "minimax-m3") {
			t.Fatalf("opencode-go list should include model: %s", outBuf.String())
		}
		if !strings.Contains(outBuf.String(), "provider opencode-go") {
			t.Fatalf("list should label provider: %s", outBuf.String())
		}
		if strings.Contains(errBuf.String(), "warning:") {
			t.Fatalf("unexpected warning on success path: %s", errBuf.String())
		}
	})

	t.Run("opencode-go warns and uses cached list on refresh failure", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(dir, "opencode-go.json"))

		// stub both fetchers so refreshAllModels() doesn't hit the live
		// opencode.ai endpoint; otherwise a connected dev machine gets
		// fresh officialModels, usedCache=false, and the warning assertion
		// below fails.
		withModelFetchers(t, nil, nil)

		// one lazyFetcher with a flag-controlled fetch: prime while the
		// flag is "succeed", then flip to "fail" and let the next refresh
		// hit the failing path. the fetcher instance — and its cached data
		// — stays the same across the flip.
		primed := map[string]remoteModelInfo{"model-a": {}, "model-b": {}}
		fail := false
		var failMu sync.Mutex
		oldRemote := remoteModels
		remoteModels = newLazyFetcher(func() (map[string]remoteModelInfo, error) {
			failMu.Lock()
			defer failMu.Unlock()
			if fail {
				return nil, errors.New("simulated offline")
			}
			return primed, nil
		})
		t.Cleanup(func() { remoteModels = oldRemote })

		if _, err := getRemoteModels(); err != nil {
			t.Fatalf("prime cache: %v", err)
		}
		failMu.Lock()
		fail = true
		failMu.Unlock()

		cmd := listCmd()
		outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.SetOut(outBuf)
		cmd.SetErr(errBuf)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("expected success on stale cache, got: %v", err)
		}
		if !strings.Contains(outBuf.String(), "model-a") || !strings.Contains(outBuf.String(), "model-b") {
			t.Fatalf("cached models missing from output: %s", outBuf.String())
		}
		if !strings.Contains(errBuf.String(), "warning:") {
			t.Fatalf("expected warning on stderr, got: %s", errBuf.String())
		}
	})

	t.Run("opencode-go errors when both fetchers fail with no cache", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(dir, "opencode-go.json"))

		withModelFetchers(t, nil, nil)
		cmd := listCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Fatal("expected error when no data is available")
		}
	})

	t.Run("cloudflare hits live /ai/models/search", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/ai/models/search") {
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("Authorization") == "" {
				t.Errorf("missing Authorization on cloudflare /ai/models/search call")
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintln(w, `{"success":true,"result":[{"id":"@cf/meta/llama-3.1-8b-instruct","name":"@cf/meta/llama-3.1-8b-instruct"},{"id":"workers-ai/@cf/zai-org/glm-5.2","name":"workers-ai/@cf/zai-org/glm-5.2"}]}`)
		}))
		defer srv.Close()
		if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(`{"upstream_base_url":"`+srv.URL+`/ai/v1","upstream_api_key":"tok","upstream_auth":"bearer"}`), 0600); err != nil {
			t.Fatal(err)
		}
		defer os.Remove(filepath.Join(dir, "cloudflare.json"))

		cmd := listCmd()
		buf := &bytes.Buffer{}
		cmd.SetOut(buf)
		cmd.SetErr(buf)
		cmd.SetArgs([]string{"--provider", "cloudflare"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("list cloudflare: %v", err)
		}
		for _, want := range []string{"@cf/meta/llama-3.1-8b-instruct", "workers-ai/@cf/zai-org/glm-5.2", "provider cloudflare"} {
			if !strings.Contains(buf.String(), want) {
				t.Fatalf("cloudflare list missing %q: %s", want, buf.String())
			}
		}
	})

	t.Run("unknown provider errors", func(t *testing.T) {
		_ = os.Remove(filepath.Join(dir, "opencode-go.json"))
		_ = os.Remove(filepath.Join(dir, "cloudflare.json"))
		cmd := listCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"--provider", "bogus"})
		if err := cmd.Execute(); err == nil {
			t.Fatal("expected error for unknown provider")
		}
	})

	t.Run("no config errors", func(t *testing.T) {
		_ = os.Remove(filepath.Join(dir, "opencode-go.json"))
		_ = os.Remove(filepath.Join(dir, "cloudflare.json"))
		cmd := listCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); err == nil {
			t.Fatal("expected no-config error")
		}
	})
}

func TestModelMetadataUsesRemoteFixtureWithoutLiveNetwork(t *testing.T) {
	withModelFetchers(t, map[string]remoteModelInfo{
		"kimi-k2.6": testRemoteModel("Kimi K2.6", 262144, "text", "image", "video"),
	}, nil)
	meta := modelMetadata("kimi-k2.6")
	if meta.DisplayName != "Kimi K2.6" || meta.ContextWindow != 262144 {
		t.Fatalf("remote metadata was not applied: %+v", meta)
	}
	if strings.Join(meta.InputModalities, ",") != "text,image,video" {
		t.Fatalf("input modalities = %+v", meta.InputModalities)
	}
	if strings.Join(meta.CodexInputModalities, ",") != "text,image" {
		t.Fatalf("codex modalities should exclude unsupported video: %+v", meta.CodexInputModalities)
	}
}

func TestCodexModelCatalogAllowsImagesForKnownVisionModels(t *testing.T) {
	if !modelSupportsImages("kimi-k2.6") {
		t.Fatal("kimi-k2.6 should support image inputs")
	}
	if !modelSupportsImages("minimax-m3") {
		t.Fatal("minimax-m3 should support image inputs")
	}
	if modelSupportsImages("deepseek-v4-pro") {
		t.Fatal("deepseek-v4-pro should not support image inputs")
	}
	for _, tc := range []struct {
		model string
		want  []string
	}{
		{model: "kimi-k2.6", want: []string{"text", "image"}},
		{model: "minimax-m3", want: []string{"text", "image", "video"}},
		{model: "deepseek-v4-pro", want: []string{"text"}},
	} {
		got := modelInputModalities(tc.model)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s modalities = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

func TestLoadModelMappingsForProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)

	// no file: each provider gets a fresh empty section
	for _, name := range []string{"opencode-go", "cloudflare"} {
		m, err := loadModelMappingsForProvider(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := m["claude"]; !ok {
			t.Fatalf("%s: claude section missing: %+v", name, m)
		}
	}

	// write one section, read both — the other provider's section must be empty
	if err := saveModelMappingsForProvider("opencode-go", map[string]map[string]string{
		"claude": {"claude-opus": "minimax-m3"},
		"codex":  {},
	}); err != nil {
		t.Fatal(err)
	}
	oc, err := loadModelMappingsForProvider("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if oc["claude"]["claude-opus"] != "minimax-m3" {
		t.Fatalf("opencode-go claude-opus = %q", oc["claude"]["claude-opus"])
	}
	cf, err := loadModelMappingsForProvider("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if len(cf["claude"]) != 0 {
		t.Fatalf("cloudflare claude should be empty, got %+v", cf["claude"])
	}

	// unknown provider: empty section, no error
	weird, err := loadModelMappingsForProvider("not-a-provider")
	if err != nil {
		t.Fatal(err)
	}
	if len(weird) != 0 {
		t.Fatalf("unknown provider should get empty section, got %+v", weird)
	}
}

func TestSaveModelMappingsForProviderPreservesOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	withTempModelMappingFile(t, path)

	// seed opencode-go
	if err := saveModelMappingsForProvider("opencode-go", map[string]map[string]string{
		"claude": {"claude-opus": "minimax-m3"},
		"codex":  {"gpt-5": "deepseek-v4-pro"},
	}); err != nil {
		t.Fatal(err)
	}
	// add cloudflare — must not wipe opencode-go
	if err := saveModelMappingsForProvider("cloudflare", map[string]map[string]string{
		"claude": {"claude-opus": "workers-ai/@cf/zai-org/glm-5.2"},
		"codex":  {},
	}); err != nil {
		t.Fatal(err)
	}
	oc, _ := loadModelMappingsForProvider("opencode-go")
	if oc["claude"]["claude-opus"] != "minimax-m3" {
		t.Fatalf("opencode-go claude-opus lost: %q", oc["claude"]["claude-opus"])
	}
	if oc["codex"]["gpt-5"] != "deepseek-v4-pro" {
		t.Fatalf("opencode-go codex gpt-5 lost: %q", oc["codex"]["gpt-5"])
	}
	cf, _ := loadModelMappingsForProvider("cloudflare")
	if cf["claude"]["claude-opus"] != "workers-ai/@cf/zai-org/glm-5.2" {
		t.Fatalf("cloudflare claude-opus wrong: %q", cf["claude"]["claude-opus"])
	}

	// verify on disk: top-level keys are provider names, not tool names
	b, _ := os.ReadFile(path)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["opencode-go"]; !ok {
		t.Fatalf("file should have opencode-go section, got: %s", string(b))
	}
	if _, ok := raw["cloudflare"]; !ok {
		t.Fatalf("file should have cloudflare section, got: %s", string(b))
	}
	if _, ok := raw["claude"]; ok {
		t.Fatalf("file should NOT have top-level claude (old format leak): %s", string(b))
	}
}

func TestLoadModelMappingsAutoMigratesOldFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-mapping.json")
	withTempModelMappingFile(t, path)
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	oldWarned := oldMappingFormatWarned
	oldMappingFormatWarned = false
	t.Cleanup(func() { oldMappingFormatWarned = oldWarned })

	old := `{"claude":{"claude-haiku":"opencode-go/deepseek-v4-flash","claude-opus":"minimax-m3","claude-sonnet":"kimi-k2.7-code"},"codex":{}}`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	// load with a known provider: legacy entries get lifted into that
	// provider's section in place, the on-disk file is rewritten, and
	// the returned shape reflects the original entries (with the
	// opencode-go/ prefix stripped from the haiku target).
	all, err := loadAllModelMappings("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if all["opencode-go"] == nil {
		t.Fatalf("expected opencode-go section, got %+v", all)
	}
	claude := all["opencode-go"]["claude"]
	if claude["claude-opus"] != "minimax-m3" ||
		claude["claude-sonnet"] != "kimi-k2.7-code" ||
		claude["claude-haiku"] != "deepseek-v4-flash" {
		t.Fatalf("entries not lifted as expected: %+v", claude)
	}
	if codex := all["opencode-go"]["codex"]; len(codex) != 0 {
		t.Fatalf("codex section should be empty, got %+v", codex)
	}

	// on-disk file is now in the new shape with no legacy tool keys at
	// the top level
	b, _ := os.ReadFile(path)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["claude"]; ok {
		t.Fatalf("legacy top-level claude should be gone, got: %s", string(b))
	}
	if _, ok := raw["opencode-go"]; !ok {
		t.Fatalf("opencode-go section should be at top level, got: %s", string(b))
	}

	// sentinel was cleared so the warning stays silent
	if _, err := os.Stat(filepath.Join(dir, "model-mapping.migrated")); err == nil {
		t.Fatalf("sentinel should be removed after migration: %v", err)
	}

	// subsequent load sees the new format and returns the same section
	// without touching the file again
	again, err := loadAllModelMappings("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if again["opencode-go"]["claude"]["claude-opus"] != "minimax-m3" {
		t.Fatalf("second load lost the migrated entry: %+v", again)
	}
}

func TestLoadModelMappingsAutoMigrationPreservesPeerProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-mapping.json")
	withTempModelMappingFile(t, path)
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	oldWarned := oldMappingFormatWarned
	oldMappingFormatWarned = false
	t.Cleanup(func() { oldMappingFormatWarned = oldWarned })

	// malformed-but-tolerable: legacy tool keys alongside a real
	// per-provider section. migration should leave the peer section
	// alone and lift the legacy keys into the active provider.
	hybrid := `{"opencode-go":{"claude":{"claude-opus":"minimax-m3"}},"claude":{"claude-haiku":"deepseek-v4-flash"},"codex":{}}`
	if err := os.WriteFile(path, []byte(hybrid), 0644); err != nil {
		t.Fatal(err)
	}
	all, err := loadAllModelMappings("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if all["opencode-go"]["claude"]["claude-opus"] != "minimax-m3" {
		t.Fatalf("peer opencode-go section was clobbered: %+v", all["opencode-go"])
	}
	if all["cloudflare"]["claude"]["claude-haiku"] != "deepseek-v4-flash" {
		t.Fatalf("legacy entries not lifted into cloudflare: %+v", all["cloudflare"])
	}
}

func TestOneShotMappingFormatWarning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-mapping.json")
	withTempModelMappingFile(t, path)
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	// reset the in-process guard so the warning fires fresh for this test
	oldWarned := oldMappingFormatWarned
	oldMappingFormatWarned = false
	t.Cleanup(func() { oldMappingFormatWarned = oldWarned })

	// write an old-format file
	old := `{"claude":{"claude-opus":"minimax-m3"},"codex":{}}`
	if err := os.WriteFile(path, []byte(old), 0644); err != nil {
		t.Fatal(err)
	}

	// capture stderr
	capturePath := filepath.Join(dir, "stderr.log")
	f, err := os.Create(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = f
	t.Cleanup(func() {
		os.Stderr = oldStderr
		f.Close()
	})

	// first load: warning fires, sentinel is created
	all, err := loadAllModelMappings("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("old format should produce empty result, got %+v", all)
	}
	_ = f.Sync()
	captured, _ := os.ReadFile(capturePath)
	if !strings.Contains(string(captured), "old tool-scoped format") {
		t.Fatalf("expected warning on first load, got: %q", string(captured))
	}
	if _, err := os.Stat(filepath.Join(dir, "model-mapping.migrated")); err != nil {
		t.Fatalf("sentinel file should be created: %v", err)
	}

	// truncate the capture file for the second load
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)

	// second load: warning is silent (sentinel exists)
	oldMappingFormatWarned = false // simulate a fresh process
	_, _ = loadAllModelMappings("")
	_ = f.Sync()
	captured, _ = os.ReadFile(capturePath)
	if len(captured) != 0 {
		t.Fatalf("second load should be silent, got: %q", string(captured))
	}
}

func TestMappingSetUsesPerProviderFormat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "opencode-go")
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// stub the model list so `set` validates the target
	withModelFetchers(t, nil, []string{"minimax-m3"})

	path := filepath.Join(dir, "model-mapping.json")
	withTempModelMappingFile(t, path)

	cmd := mappingCmd()
	cmd.SetArgs([]string{"claude", "set", "claude-opus", "minimax-m3"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mapping set: %v", err)
	}

	// the on-disk file should have a top-level "opencode-go" key
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"opencode-go"`) {
		t.Fatalf("mapping file should have opencode-go section, got: %s", string(b))
	}
	oc, _ := loadModelMappingsForProvider("opencode-go")
	if oc["claude"]["claude-opus"] != "minimax-m3" {
		t.Fatalf("opencode-go mapping not written: %+v", oc["claude"])
	}
	// cloudflare section should not exist (or be empty)
	cf, _ := loadModelMappingsForProvider("cloudflare")
	if v, ok := cf["claude"]["claude-opus"]; ok && v != "" {
		t.Fatalf("cloudflare section should not have the opencode-go mapping, got: %+v", cf)
	}
}

func TestAnthropicEndpointModels(t *testing.T) {
	// empty cfg: no overrides, falls back to modelMetadata heuristic
	cfg := ProviderConfig{}
	for _, model := range []string{"qwen3.7-max", "qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "minimax-m3", "minimax-m2.7", "glm-5.2", "kimi-k2.7-code", "opencode-go/qwen3.7-max", "opencode-go/qwen3.7-plus", "opencode-go/minimax-m3", "opencode-go/glm-5.2", "opencode-go/kimi-k2.7-code"} {
		if !modelUsesAnthropicEndpoint(model, cfg) {
			t.Fatalf("%s should use Anthropic-compatible upstream", model)
		}
	}
	for _, model := range []string{"kimi-k2.6"} {
		if modelUsesAnthropicEndpoint(model, cfg) {
			t.Fatalf("%s should use OpenAI-compatible upstream", model)
		}
	}
}

func TestChatToAnthropicForCodexModel(t *testing.T) {
	or := responsesToChat(ResponsesRequest{Model: "qwen3.7-max", Stream: true, Input: []byte(`[{"type":"message","role":"user","content":"hello"}]`), Tools: []ResponseTool{{Type: "function", Name: "shell", Description: "run", Parameters: []byte(`{"type":"object"}`)}}}, testProviderCfg)
	ar := chatToAnthropic(or, testProviderCfg)
	if ar.Model != "qwen3.7-max" || !ar.Stream || ar.MaxTokens == 0 {
		t.Fatalf("bad anthropic request metadata: %+v", ar)
	}
	if len(ar.Messages) != 1 || ar.Messages[0].Role != "user" || string(ar.Messages[0].Content) != `"hello"` {
		t.Fatalf("bad anthropic messages: %+v", ar.Messages)
	}
	if len(ar.Tools) != 1 || ar.Tools[0].Name != "shell" {
		t.Fatalf("bad anthropic tools: %+v", ar.Tools)
	}
}

func TestResponsesToChatMapsBuiltInWebToolsForAnthropicModels(t *testing.T) {
	or := responsesToChat(ResponsesRequest{
		Model:  "qwen3.7-max",
		Input:  []byte(`[{"type":"message","role":"user","content":"search the web"}]`),
		Tools:  []ResponseTool{{Type: "web_search_preview"}, {Type: "web_search"}, {Type: "web_extractor"}, {Type: "function", Name: "shell", Parameters: []byte(`{"type":"object"}`)}},
		Stream: true,
	}, testProviderCfg)
	if len(or.AnthropicTools) != 2 {
		t.Fatalf("expected web search and fetch anthropic tools, got %+v", or.AnthropicTools)
	}
	ar := chatToAnthropic(or, testProviderCfg)
	if len(ar.Tools) != 3 {
		t.Fatalf("expected 2 built-in tools plus shell, got %+v", ar.Tools)
	}
	if ar.Tools[0].Type != "web_search_20250305" || ar.Tools[0].Name != "web_search" {
		t.Fatalf("bad web search tool mapping: %+v", ar.Tools[0])
	}
	if ar.Tools[1].Type != "web_fetch_20250910" || ar.Tools[1].Name != "web_fetch" {
		t.Fatalf("bad web fetch tool mapping: %+v", ar.Tools[1])
	}
	if ar.Tools[2].Type != "" || ar.Tools[2].Name != "shell" {
		t.Fatalf("bad function tool mapping: %+v", ar.Tools[2])
	}
}

func TestResponsesToChatSkipsEmptyToolNamesAndDefaultsParameters(t *testing.T) {
	or := responsesToChat(ResponsesRequest{
		Model: "minimax-m2.7",
		Input: []byte(`"hello"`),
		Tools: []ResponseTool{
			{Type: "function", Name: ""},
			{Type: "function", Name: "shell"},
		},
	}, testProviderCfg)
	if len(or.Tools) != 1 {
		t.Fatalf("expected one valid tool, got %+v", or.Tools)
	}
	if or.Tools[0].Function.Name != "shell" {
		t.Fatalf("bad tool name: %+v", or.Tools[0])
	}
	if string(or.Tools[0].Function.Parameters) != `{"type":"object","properties":{}}` {
		t.Fatalf("bad default params: %s", string(or.Tools[0].Function.Parameters))
	}
	ar := chatToAnthropic(or, testProviderCfg)
	if len(ar.Tools) != 1 || ar.Tools[0].Name != "shell" || string(ar.Tools[0].InputSchema) != `{"type":"object","properties":{}}` {
		t.Fatalf("bad anthropic tools: %+v", ar.Tools)
	}
}

func TestNormalizeAnthropicRequestForStrictUpstream(t *testing.T) {
	ar := AnthropicRequest{
		Model:        "opencode-go/qwen3.7-max",
		Thinking:     []byte(`{"type":"enabled","budget_tokens":1024}`),
		Reasoning:    []byte(`{"effort":"high"}`),
		OutputConfig: []byte(`{"reasoning":{"depth":2}}`),
		System:       []byte(`[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}]`),
		Tools:        []ATool{{Type: "web_search_20250305", Name: "web_search", MaxUses: 8, AllowedDomains: []string{"example.com"}}},
		Messages: []AMessage{{Role: "user", Content: []byte(`[
			{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},
			{"type":"thinking","thinking":"private","signature":"abc"},
			{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]}
		]`)}},
	}
	normalizeAnthropicRequestForUpstream(&ar, testProviderCfg)
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	for _, gone := range []string{"opencode-go/", "reasoning", "output_config", "cache_control", "signature"} {
		if strings.Contains(out, gone) {
			t.Fatalf("strict upstream request still contains %q: %s", gone, out)
		}
	}
	for _, want := range []string{
		`"model":"qwen3.7-max"`,
		`"system":"rules"`,
		`"type":"text"`,
		`"text":"hello"`,
		`"tool_use_id":"toolu_1"`,
		`"type":"web_search_20250305"`,
		`"name":"web_search"`,
		`"max_uses":8`,
		`"allowed_domains":["example.com"]`,
		`"type":"enabled"`,
		`"budget_tokens":8192`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in normalized request: %s", want, out)
		}
	}
}

func TestNormalizeAnthropicToolResultTruncatesLargeFetchContent(t *testing.T) {
	large := strings.Repeat("a", maxAnthropicToolResultContentChars+50) + "tail-should-be-omitted"
	ar := AnthropicRequest{
		Model: "qwen3.7-max",
		Messages: []AMessage{{Role: "user", Content: marshalJSON([]map[string]any{{
			"type":        "tool_result",
			"tool_use_id": "toolu_fetch",
			"content":     []map[string]any{{"type": "text", "text": large}},
		}})}},
	}

	normalizeAnthropicRequestForUpstream(&ar, testProviderCfg)
	body, err := json.Marshal(ar)
	if err != nil {
		t.Fatal(err)
	}
	out := string(body)
	if strings.Contains(out, "tail-should-be-omitted") {
		t.Fatalf("large fetched content was not truncated: %s", out[len(out)-200:])
	}
	for _, want := range []string{`"tool_use_id":"toolu_fetch"`, "cfgate-cc truncated tool_result content"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in normalized request: %s", want, out)
		}
	}
}

func TestNormalizeQwenAnthropicRequestThinkingVariants(t *testing.T) {
	zero := 0.0
	topP := 0.8
	for _, tc := range []struct {
		name          string
		req           AnthropicRequest
		expectBudget  int // 0 = expect no thinking field
	}{
		{
			name: "thinking enabled with budget",
			req: AnthropicRequest{
				Thinking: []byte(`{"type":"enabled","budget_tokens":2048}`),
			},
			// walker turns type:enabled into "high" → qwen high = 8192
			expectBudget: 8192,
		},
		{
			name: "thinking disabled with zero temperature",
			req: AnthropicRequest{
				Thinking:    []byte(`{"type":"disabled"}`),
				Temperature: &zero,
			},
			expectBudget: 0,
		},
		{
			name: "reasoning effort high",
			req: AnthropicRequest{
				ReasoningEffort: []byte(`"high"`),
				TopP:            &topP,
			},
			expectBudget: 8192,
		},
		{
			name: "nested output config reasoning",
			req: AnthropicRequest{
				OutputConfig: []byte(`{"reasoning":{"effort":"medium"}}`),
			},
			expectBudget: 4096,
		},
		{
			name: "legacy effort level depth fields",
			req: AnthropicRequest{
				Effort: []byte(`"low"`),
				Level:  []byte(`2`),
				Depth:  []byte(`{"level":"high"}`),
			},
			// walker hits effort first → low → 2048
			expectBudget: 2048,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := tc.req
			ar.Model = "opencode-go/qwen3.7-max"
			ar.Stream = true
			ar.MaxTokens = 1234
			ar.System = []byte(`plain rules`)
			ar.Tools = []ATool{{Name: "Bash", Description: "run command", InputSchema: []byte(`{"type":"object","properties":{"command":{"type":"string"}}}`)}}
			ar.Messages = []AMessage{{Role: "user", Content: []byte(`[
				{"type":"text","text":"hello qwen","cache_control":{"type":"ephemeral"}},
				{"type":"thinking","thinking":"hidden chain of thought","signature":"sig_123"},
				{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"pwd","cache_control":{"type":"ephemeral"}}}
			]`)}}

			normalizeAnthropicRequestForUpstream(&ar, testProviderCfg)
			body, err := json.Marshal(ar)
			if err != nil {
				t.Fatal(err)
			}
			out := string(body)
			// "thinking" stays now — it's where the budget lands. everything
			// else from the upstream-rejected list still has to go.
			for _, gone := range []string{"opencode-go/", "reasoning", "reasoning_effort", "output_config", "effort", "level", "depth", "cache_control", "signature", "hidden chain of thought"} {
				if strings.Contains(out, gone) {
					t.Fatalf("normalized qwen request still contains %q: %s", gone, out)
				}
			}
			if tc.expectBudget == 0 {
				if strings.Contains(out, `"thinking"`) {
					t.Fatalf("expected no thinking field, got: %s", out)
				}
			} else {
				budgetWant := fmt.Sprintf(`"budget_tokens":%d`, tc.expectBudget)
				if !strings.Contains(out, budgetWant) {
					t.Fatalf("missing %q in normalized qwen request: %s", budgetWant, out)
				}
				if !strings.Contains(out, `"type":"enabled"`) {
					t.Fatalf("missing thinking type=enabled in normalized qwen request: %s", out)
				}
			}
			for _, want := range []string{`"model":"qwen3.7-max"`, `"stream":true`, `"max_tokens":1234`, `"system":"plain rules"`, `"name":"Bash"`, `"id":"toolu_1"`, `"command":"pwd"`, `"text":"hello qwen"`} {
				if !strings.Contains(out, want) {
					t.Fatalf("missing %q in normalized qwen request: %s", want, out)
				}
			}
			if tc.req.Temperature != nil && !strings.Contains(out, `"temperature":0`) {
				t.Fatalf("temperature option was not preserved: %s", out)
			}
			if tc.req.TopP != nil && !strings.Contains(out, `"top_p":0.8`) {
				t.Fatalf("top_p option was not preserved: %s", out)
			}
		})
	}
}

// cloudflare workers-ai anthropic-compatible models share the same thinking
// budget table as qwen. cover glm-5.2 and kimi-k2.7-code with the same set of
// inputs the qwen test uses — confirms the cloudflare path picks the table up
// from modelMetadata just like the opencode-go path does.
func TestNormalizeGlm5AnthropicRequestThinkingVariants(t *testing.T) {
	for _, tc := range []struct {
		name         string
		req          AnthropicRequest
		expectBudget int
	}{
		{name: "thinking enabled with budget", req: AnthropicRequest{Thinking: []byte(`{"type":"enabled","budget_tokens":2048}`)}, expectBudget: 8192},
		{name: "reasoning effort high", req: AnthropicRequest{ReasoningEffort: []byte(`"high"`)}, expectBudget: 8192},
		{name: "nested output config medium", req: AnthropicRequest{OutputConfig: []byte(`{"reasoning":{"effort":"medium"}}`)}, expectBudget: 4096},
		{name: "legacy effort low", req: AnthropicRequest{Effort: []byte(`"low"`)}, expectBudget: 2048},
		{name: "no effort set", req: AnthropicRequest{}, expectBudget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := tc.req
			ar.Model = "opencode-go/glm-5.2"
			ar.Messages = []AMessage{{Role: "user", Content: []byte(`"hello glm"`)}}
			normalizeAnthropicRequestForUpstream(&ar, testProviderCfg)
			body, err := json.Marshal(ar)
			if err != nil {
				t.Fatal(err)
			}
			out := string(body)
			for _, gone := range []string{"opencode-go/", "reasoning", "reasoning_effort", "output_config", "effort", "level", "depth"} {
				if strings.Contains(out, gone) {
					t.Fatalf("normalized glm-5.2 request still contains %q: %s", gone, out)
				}
			}
			if tc.expectBudget == 0 {
				if strings.Contains(out, `"thinking"`) {
					t.Fatalf("expected no thinking field, got: %s", out)
				}
				return
			}
			want := fmt.Sprintf(`"budget_tokens":%d`, tc.expectBudget)
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in normalized glm-5.2 request: %s", want, out)
			}
			if !strings.Contains(out, `"type":"enabled"`) {
				t.Fatalf("missing thinking type=enabled in normalized glm-5.2 request: %s", out)
			}
		})
	}
}

func TestNormalizeKimiCodeAnthropicRequestThinkingVariants(t *testing.T) {
	for _, tc := range []struct {
		name         string
		req          AnthropicRequest
		expectBudget int
	}{
		{name: "max effort", req: AnthropicRequest{Effort: []byte(`"max"`)}, expectBudget: 16384},
		{name: "xhigh collapses to max bucket", req: AnthropicRequest{Effort: []byte(`"xhigh"`)}, expectBudget: 16384},
		{name: "numeric 4 → max", req: AnthropicRequest{Effort: []byte(`4`)}, expectBudget: 16384},
		{name: "high", req: AnthropicRequest{ReasoningEffort: []byte(`"high"`)}, expectBudget: 8192},
		{name: "medium", req: AnthropicRequest{OutputConfig: []byte(`{"reasoning":{"effort":"medium"}}`)}, expectBudget: 4096},
		{name: "low", req: AnthropicRequest{Effort: []byte(`"low"`)}, expectBudget: 2048},
		{name: "no effort set", req: AnthropicRequest{}, expectBudget: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ar := tc.req
			ar.Model = "opencode-go/kimi-k2.7-code"
			ar.Messages = []AMessage{{Role: "user", Content: []byte(`"hello kimi"`)}}
			normalizeAnthropicRequestForUpstream(&ar, testProviderCfg)
			body, err := json.Marshal(ar)
			if err != nil {
				t.Fatal(err)
			}
			out := string(body)
			for _, gone := range []string{"opencode-go/", "reasoning", "reasoning_effort", "output_config", "effort", "level", "depth"} {
				if strings.Contains(out, gone) {
					t.Fatalf("normalized kimi-k2.7-code request still contains %q: %s", gone, out)
				}
			}
			if tc.expectBudget == 0 {
				if strings.Contains(out, `"thinking"`) {
					t.Fatalf("expected no thinking field, got: %s", out)
				}
				return
			}
			want := fmt.Sprintf(`"budget_tokens":%d`, tc.expectBudget)
			if !strings.Contains(out, want) {
				t.Fatalf("missing %q in normalized kimi-k2.7-code request: %s", want, out)
			}
			if !strings.Contains(out, `"type":"enabled"`) {
				t.Fatalf("missing thinking type=enabled in normalized kimi-k2.7-code request: %s", out)
			}
		})
	}
}

func TestAnthropicThinkingForRequest(t *testing.T) {
	cfgWithMax := ProviderConfig{EndpointOverrides: []ModelEndpointOverride{{Pattern: "qwen3.7-max", ThinkingBudgetMax: ptr(999)}}}
	for _, tc := range []struct {
		name    string
		req     AnthropicRequest
		cfg     ProviderConfig
		want    string // expected budget substring; empty = expect nil
		wantNil bool
	}{
		{name: "minimal returns nil", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"minimal"`)}, wantNil: true},
		{name: "no effort returns nil", req: AnthropicRequest{Model: "qwen3.7-max"}, wantNil: true},
		{name: "low qwen", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"low"`)}, want: `"budget_tokens":2048`},
		{name: "max minimax-m3", req: AnthropicRequest{Model: "minimax-m3", Effort: []byte(`"max"`)}, want: `"budget_tokens":32768`},
		{name: "low glm-5.2", req: AnthropicRequest{Model: "glm-5.2", Effort: []byte(`"low"`)}, want: `"budget_tokens":2048`},
		{name: "max kimi-k2.7-code", req: AnthropicRequest{Model: "kimi-k2.7-code", Effort: []byte(`"max"`)}, want: `"budget_tokens":16384`},
		{name: "xhigh kimi-k2.7-code → max bucket", req: AnthropicRequest{Model: "kimi-k2.7-code", Effort: []byte(`"xhigh"`)}, want: `"budget_tokens":16384`},
		{name: "max qwen3.7-plus", req: AnthropicRequest{Model: "qwen3.7-plus", Effort: []byte(`"max"`)}, want: `"budget_tokens":16384`},
		{name: "high qwen3.6-plus", req: AnthropicRequest{Model: "qwen3.6-plus", Effort: []byte(`"high"`)}, want: `"budget_tokens":8192`},
		{name: "low qwen3.5-plus", req: AnthropicRequest{Model: "qwen3.5-plus", Effort: []byte(`"low"`)}, want: `"budget_tokens":2048`},
		{name: "no budget qwen3.7-plus when minimal", req: AnthropicRequest{Model: "qwen3.7-plus", Effort: []byte(`"minimal"`)}, wantNil: true},
		{name: "unknown level returns nil", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"gibberish"`)}, wantNil: true},
		{name: "model without budget returns nil", req: AnthropicRequest{Model: "kimi-k2.6", Effort: []byte(`"high"`)}, wantNil: true},
		{name: "override thinking_budget_max wins", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"high"`)}, cfg: cfgWithMax, want: `"budget_tokens":999`},
		{name: "override thinking_budget_max=0 falls back to no thinking", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"high"`)}, cfg: ProviderConfig{EndpointOverrides: []ModelEndpointOverride{{Pattern: "qwen3.7-max", ThinkingBudgetMax: ptr(0)}}}, wantNil: true},
		{name: "routing-only override keeps table budget", req: AnthropicRequest{Model: "qwen3.7-max", Effort: []byte(`"high"`)}, cfg: ProviderConfig{EndpointOverrides: []ModelEndpointOverride{{Pattern: "qwen3.7-max", Route: "anthropic"}}}, want: `"budget_tokens":8192`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			if cfg.Name == "" && len(cfg.EndpointOverrides) == 0 {
				cfg = testProviderCfg
			}
			out := anthropicThinkingForRequest(&tc.req, cfg)
			if tc.wantNil {
				if out != nil {
					t.Fatalf("expected nil, got %s", string(out))
				}
				return
			}
			if out == nil {
				t.Fatalf("expected thinking with %q, got nil", tc.want)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("thinking = %s, want substring %q", string(out), tc.want)
			}
		})
	}
}

func TestForwardAnthropicSendsNormalizedBody(t *testing.T) {
	var forwarded string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-key" {
			t.Fatalf("missing API key header: %q", r.Header.Get("X-API-Key"))
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		forwarded = string(b)
		for _, gone := range []string{"opencode-go/", "reasoning", "cache_control", "signature"} {
			if strings.Contains(forwarded, gone) {
				t.Fatalf("forwarded body still contains %q: %s", gone, forwarded)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	cfg := ProviderConfig{UpstreamBaseURL: srv.URL, UpstreamAuth: "x-api-key", UpstreamAPIKey: "test-key"}

	resp, err := forwardAnthropic(context.Background(), cfg, AnthropicRequest{
		Model:     "opencode-go/qwen3.7-max",
		Thinking:  []byte(`{"type":"enabled"}`),
		System:    []byte(`[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}]`),
		Messages:  []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},{"type":"thinking","thinking":"private","signature":"abc"}]`)}},
		MaxTokens: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, want := range []string{`"model":"qwen3.7-max"`, `"system":"rules"`, `"messages"`, `"text":"hello"`, `"thinking":{"type":"enabled","budget_tokens":8192}`} {
		if !strings.Contains(forwarded, want) {
			t.Fatalf("missing %q in forwarded body: %s", want, forwarded)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("0.80.9", "0.81.0") >= 0 {
		t.Fatal("0.80.9 should be older")
	}
	if compareVersions("0.81.0", "0.81.0") != 0 {
		t.Fatal("same versions should compare equal")
	}
	if compareVersions("codex-cli", "0.81.0") >= 0 {
		t.Fatal("invalid version should compare as old")
	}
	if compareVersions("0.87.0", "0.81.0") <= 0 {
		t.Fatal("0.87.0 should be newer")
	}
}

func TestResponsesInputToMessages(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"message","role":"developer","content":"rules"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]`))
	if len(messages) != 2 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].Role != "system" || messages[0].Content != "rules" {
		t.Fatalf("bad developer conversion: %+v", messages[0])
	}
	if messages[1].Role != "user" || messages[1].Content != "hello" {
		t.Fatalf("bad user conversion: %+v", messages[1])
	}
}

func TestResponsesInputFunctionCallUsesCallID(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","id":"fc_123","call_id":"call_123","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_123","output":"/tmp"}]`))
	if len(messages) != 2 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].ToolCalls[0].ID != "call_123" {
		t.Fatalf("tool call ID should match call_id for follow-up tool output: %+v", messages[0].ToolCalls[0])
	}
	if messages[0].ReasoningContent == "" {
		t.Fatalf("assistant tool call history should include fallback reasoning_content: %+v", messages[0])
	}
	if messages[1].ToolCallID != "call_123" {
		t.Fatalf("bad tool output ID: %+v", messages[1])
	}
}

func TestAnthropicToolUseHistoryIncludesFallbackReasoning(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "assistant", Content: []byte(`[{"type":"tool_use","id":"call_123","name":"Bash","input":{"command":"pwd"}}]`)})
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	if messages[0].Role != "assistant" || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("bad tool call conversion: %+v", messages[0])
	}
	if messages[0].ReasoningContent == "" {
		t.Fatalf("assistant tool call history should include fallback reasoning_content: %+v", messages[0])
	}
}

func TestAnthropicToolResultPreservesFollowingUserText(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "user", Content: []byte(`[{"type":"tool_result","tool_use_id":"call_123","content":"09:33:16"},{"type":"text","text":"https://figma.example/design what's going on here?"}]`)})
	if len(messages) != 2 {
		t.Fatalf("got %d messages: %+v", len(messages), messages)
	}
	if messages[0].Role != "tool" || messages[0].ToolCallID != "call_123" || messages[0].Content != "09:33:16" {
		t.Fatalf("bad tool result conversion: %+v", messages[0])
	}
	if messages[1].Role != "user" || !strings.Contains(contentString(messages[1].Content), "figma.example") {
		t.Fatalf("following user text was not preserved: %+v", messages[1])
	}
}

func TestResponsesInputPreservesImages(t *testing.T) {
	messages := responsesInputToMessages([]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64,abc","detail":"high"}]}]`))
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	parts, ok := messages[0].Content.([]OAIContentPart)
	if !ok {
		t.Fatalf("content should be multimodal parts: %+v", messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Type != "text" || parts[0].Text != "describe this" {
		t.Fatalf("bad text part: %+v", parts)
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,abc" || parts[1].ImageURL.Detail != "" {
		t.Fatalf("bad image part: %+v", parts[1])
	}
}

func TestResponsesImageKeepsKimiModel(t *testing.T) {
	req := ResponsesRequest{Model: "kimi-k2.6", Input: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64=abc"}]}]`)}
	out := responsesToChat(req, testProviderCfg)
	if out.Model != "kimi-k2.6" {
		t.Fatalf("image request should keep Kimi model, got %q", out.Model)
	}
	if err := validateImageSupport(out); err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
}

func TestResponsesImageRejectsUnsupportedModel(t *testing.T) {
	req := ResponsesRequest{Model: "deepseek-v4-pro", Input: []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"describe this"},{"type":"input_image","image_url":"data:image/png;base64=abc"}]}]`)}
	out := responsesToChat(req, testProviderCfg)
	if err := validateImageSupport(out); err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func TestRawChatImageKeepsKimiAndStripsDetail(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"kimi-k2.6","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc","detail":"high"}}]}]}`), testProviderCfg)
	if err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
	if !strings.Contains(string(body), `"model":"kimi-k2.6"`) {
		t.Fatalf("image chat body should keep Kimi model: %s", string(body))
	}
	if strings.Contains(string(body), `"detail"`) {
		t.Fatalf("image detail should be stripped for compatibility: %s", string(body))
	}
}

func TestRawChatImageRejectsUnsupportedModel(t *testing.T) {
	_, err := prepareChatBody([]byte(`{"model":"deepseek-v4-pro","messages":[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}]}`), testProviderCfg)
	if err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func TestRawChatStreamRequestsUsage(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"kimi-k2.6","stream":true,"stream_options":{"foo":"bar"},"messages":[{"role":"user","content":"hello"}]}`), testProviderCfg)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	options, ok := req["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("missing stream options in %s", string(body))
	}
	if options["include_usage"] != true || options["foo"] != "bar" {
		t.Fatalf("bad stream options: %+v", options)
	}
}

func TestNormalizeReasoningEffort(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "minimal", want: "minimal"},
		{in: "0", want: "minimal"},
		{in: "low", want: "low"},
		{in: "1", want: "low"},
		{in: "medium", want: "medium"},
		{in: "2", want: "medium"},
		{in: "high", want: "high"},
		{in: "xhigh", want: "high"},
		{in: "max", want: "high"},
	} {
		if got := normalizeReasoningEffort(tc.in); got != tc.want {
			t.Fatalf("normalizeReasoningEffort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRawChatReasoningEffortPassThrough(t *testing.T) {
	body, err := prepareChatBody([]byte(`{"model":"glm-5.1","reasoning":{"effort":"xhigh"},"thinking":{"type":"enabled"},"output_config":{"reasoning":{"depth":2}},"messages":[{"role":"user","content":"hello"}]}`), testProviderCfg)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["reasoning_effort"] != "max" {
		t.Fatalf("reasoning_effort = %v, want max in %s", req["reasoning_effort"], string(body))
	}
	for _, key := range []string{"reasoning", "thinking", "effort", "level", "depth", "output_config"} {
		if _, ok := req[key]; ok {
			t.Fatalf("%s should be stripped from forwarded chat body: %s", key, string(body))
		}
	}
}

// TestReasoningEffortCollapsedForChatCompletions locks in that the cloudflare
// @cf/... workers-ai chat path keeps reasoning_effort when the user set it —
// it has to, because the gateway header is set by the request layer, not the
// body normalizer, and the body still has to carry the effort to the upstream
// chat-completions endpoint.
func TestReasoningEffortCollapsedForChatCompletions(t *testing.T) {
	cfg := ProviderConfig{Name: "cloudflare", Gateway: "test-gw"}
	raw := []byte(`{"model":"@cf/meta/llama-3.1-8b-instruct","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	body, err := prepareChatBody(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if req["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low in %s", req["reasoning_effort"], string(body))
	}
	if req["model"] != "@cf/meta/llama-3.1-8b-instruct" {
		t.Fatalf("model = %v, want unchanged @cf/... workers-ai id", req["model"])
	}
}

func TestReasoningEffortExtraction(t *testing.T) {
	for _, tc := range []struct {
		raw  json.RawMessage
		want string
	}{
		{raw: []byte(`"low"`), want: "low"},
		{raw: []byte(`3`), want: "high"},
		{raw: []byte(`{"level":"medium"}`), want: "medium"},
		{raw: []byte(`{"type":"enabled"}`), want: "high"},
		{raw: []byte(`{"reasoning":{"depth":1}}`), want: "low"},
		{raw: []byte(`"max"`), want: "max"},
		{raw: []byte(`"xhigh"`), want: "max"},
		{raw: []byte(`4`), want: "max"},
	} {
		if got := downstreamReasoningEffort(tc.raw); got != tc.want {
			t.Fatalf("downstreamReasoningEffort(%s) = %q, want %q", string(tc.raw), got, tc.want)
		}
	}
}

func TestRawChatReasoningEffortPreservesMax(t *testing.T) {
	for _, tc := range []struct {
		req map[string]any
		key string
	}{
		{req: map[string]any{"effort": "max"}, key: "effort=max"},
		{req: map[string]any{"level": "xhigh"}, key: "level=xhigh"},
		{req: map[string]any{"reasoning": map[string]any{"effort": "4"}}, key: "reasoning.effort=4"},
	} {
		if got := rawChatReasoningEffort(tc.req); got != "max" {
			t.Fatalf("rawChatReasoningEffort(%s) = %q, want max", tc.key, got)
		}
	}
}

func TestConvertedStreamingRequestsAskForUsage(t *testing.T) {
	anthropic := convertRequest(AnthropicRequest{Model: "kimi-k2.6", Stream: true, Messages: []AMessage{{Role: "user", Content: []byte(`hello`)}}}, testProviderCfg)
	if anthropic.StreamOptions == nil || !anthropic.StreamOptions.IncludeUsage {
		t.Fatalf("anthropic conversion should request stream usage: %+v", anthropic.StreamOptions)
	}
	responses := responsesToChat(ResponsesRequest{Model: "kimi-k2.6", Stream: true, Input: []byte(`"hello"`)}, testProviderCfg)
	if responses.StreamOptions == nil || !responses.StreamOptions.IncludeUsage {
		t.Fatalf("responses conversion should request stream usage: %+v", responses.StreamOptions)
	}
	plain := responsesToChat(ResponsesRequest{Model: "kimi-k2.6", Input: []byte(`"hello"`)}, testProviderCfg)
	if plain.StreamOptions != nil {
		t.Fatalf("non-streaming conversion should not set stream options: %+v", plain.StreamOptions)
	}
}

func TestConvertedRequestsForwardReasoningEffort(t *testing.T) {
	anthropic := convertRequest(AnthropicRequest{Model: "glm-5.1", Reasoning: []byte(`{"effort":"high"}`), Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"hello"}]`)}}}, testProviderCfg)
	if anthropic.ReasoningEffort != "high" {
		t.Fatalf("anthropic reasoning effort = %q, want high", anthropic.ReasoningEffort)
	}
	responses := responsesToChat(ResponsesRequest{Model: "glm-5.1", OutputConfig: []byte(`{"reasoning":{"depth":2}}`), Input: []byte(`[{"type":"message","role":"user","content":"hello"}]`)}, testProviderCfg)
	if responses.ReasoningEffort != "medium" {
		t.Fatalf("responses reasoning effort = %q, want medium", responses.ReasoningEffort)
	}
}

func TestAnthropicContentPreservesImages(t *testing.T) {
	messages := contentToOpenAI(AMessage{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"abc"}}]`)})
	if len(messages) != 1 {
		t.Fatalf("got %d messages", len(messages))
	}
	parts, ok := messages[0].Content.([]OAIContentPart)
	if !ok {
		t.Fatalf("content should be multimodal parts: %+v", messages[0].Content)
	}
	if len(parts) != 2 || parts[0].Text != "what is this?" {
		t.Fatalf("bad text part: %+v", parts)
	}
	if parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/jpeg;base64,abc" {
		t.Fatalf("bad image part: %+v", parts[1])
	}
}

func TestAnthropicImageKeepsKimiModel(t *testing.T) {
	out := convertRequest(AnthropicRequest{Model: "kimi-k2.6", Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]`)}}}, testProviderCfg)
	if out.Model != "kimi-k2.6" {
		t.Fatalf("image request should keep Kimi model, got %q", out.Model)
	}
	if err := validateImageSupport(out); err != nil {
		t.Fatalf("Kimi image request should validate: %v", err)
	}
}

func TestAnthropicImageRejectsUnsupportedModel(t *testing.T) {
	out := convertRequest(AnthropicRequest{Model: "deepseek-v4-pro", Messages: []AMessage{{Role: "user", Content: []byte(`[{"type":"text","text":"what is this?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"abc"}}]`)}}}, testProviderCfg)
	if err := validateImageSupport(out); err == nil || !strings.Contains(err.Error(), "deepseek-v4-pro") {
		t.Fatalf("DeepSeek image request should be rejected, got %v", err)
	}
}

func contentString(v any) string {
	s, _ := v.(string)
	return s
}

func TestBuildCloudflareURL(t *testing.T) {
	got := buildCloudflareURL("a9ffc25861cdda67e0b6c8e7475bc5e3")
	want := "https://api.cloudflare.com/client/v4/accounts/a9ffc25861cdda67e0b6c8e7475bc5e3/ai/v1"
	if got != want {
		t.Fatalf("buildCloudflareURL = %q, want %q", got, want)
	}
}

func TestSetupCloudflareWritesAssembledConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")

	cmd := setupCloudflareCmd()
	cmd.SetArgs([]string{"--token", "tok-xyz", "--account", "acct-123", "--gateway", "gw-456"})
	cmd.SetIn(strings.NewReader(""))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "cloudflare.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p ProviderConfig
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.UpstreamBaseURL != "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1" {
		t.Fatalf("upstream_base_url = %q", p.UpstreamBaseURL)
	}
	if p.UpstreamAPIKey != "tok-xyz" {
		t.Fatalf("upstream_api_key = %q", p.UpstreamAPIKey)
	}
	if p.UpstreamAuth != "bearer" {
		t.Fatalf("upstream_auth = %q, want bearer", p.UpstreamAuth)
	}
	if p.Gateway != "gw-456" {
		t.Fatalf("gateway = %q, want gw-456", p.Gateway)
	}
}

func TestReadCloudflareValuesPromptsAndFallsBackToEnv(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-tok")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")

	in := strings.NewReader("env-acct\nenv-gw\n")
	values, err := readCloudflareValues(in, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if values.token != "env-tok" || values.account != "env-acct" || values.gateway != "env-gw" {
		t.Fatalf("unexpected values: %+v", values)
	}
}

func TestReadCloudflareValuesFlagOverridesEnv(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-tok")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "env-acct")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "env-gw")

	values, err := readCloudflareValues(strings.NewReader(""), "flag-tok", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if values.token != "flag-tok" || values.account != "env-acct" || values.gateway != "env-gw" {
		t.Fatalf("flag should win over env: %+v", values)
	}
}

func TestReadCloudflareValuesRejectsMissing(t *testing.T) {
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")
	if _, err := readCloudflareValues(strings.NewReader("\n\n\n"), "", "", ""); err == nil {
		t.Fatal("expected error when all values are empty")
	}
}

func TestSetupCloudflareRefusesToOverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CLOUDFLARE_API_TOKEN", "")
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	t.Setenv("CLOUDFLARE_GATEWAY_ID", "")

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	existing := `{"upstream_base_url":"https://example.com/v1/","upstream_api_key":"k","upstream_auth":"bearer"}`
	if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(existing), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := setupCloudflareCmd()
	cmd.SetArgs([]string{"--token", "t", "--account", "a", "--gateway", "g"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("expected overwrite guard error, got %v", err)
	}

	cmd = setupCloudflareCmd()
	cmd.SetArgs([]string{"--token", "t", "--account", "a", "--gateway", "g", "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "cloudflare.json"))
	if !strings.Contains(string(b), "https://api.cloudflare.com/client/v4/accounts/a/ai/v1") {
		t.Fatalf("forced overwrite did not update URL:\n%s", string(b))
	}
}

func TestParseWindowsNetstatPID(t *testing.T) {
	output := strings.Join([]string{
		"Proto  Local Address          Foreign Address        State           PID",
		"TCP    127.0.0.1:3456       0.0.0.0:0              LISTENING       4321",
		"TCP    [::1]:9999           [::]:0                 LISTENING       8765",
		"TCP    127.0.0.1:34560      0.0.0.0:0              LISTENING       1111",
	}, "\n")
	pid, err := parseWindowsNetstatPID(output, 3456)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 4321 {
		t.Fatalf("pid = %d, want 4321", pid)
	}
}

func TestParseWindowsNetstatPIDMatchesIPv6(t *testing.T) {
	output := "TCP    [::]:3456             [::]:0                 LISTENING       2468\n"
	pid, err := parseWindowsNetstatPID(output, 3456)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 2468 {
		t.Fatalf("pid = %d, want 2468", pid)
	}
}

func TestWriteAnthropicResponseIncludesUsage(t *testing.T) {
	body := strings.NewReader(`{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16,"prompt_tokens_details":{"cached_tokens":4}}}`)
	w := httptest.NewRecorder()
	writeAnthropicResponse(w, body, "kimi-k2.6")
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %+v", out)
	}
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(5) || usage["cache_read_input_tokens"] != float64(4) {
		t.Fatalf("bad anthropic usage: %+v", usage)
	}
}

func TestWriteResponsesResponseIncludesUsage(t *testing.T) {
	body := strings.NewReader(`{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":2}}}`)
	w := httptest.NewRecorder()
	writeResponsesResponse(w, body, "kimi-k2.6")
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage, ok := out["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %+v", out)
	}
	if usage["input_tokens"] != float64(7) || usage["output_tokens"] != float64(3) || usage["total_tokens"] != float64(10) {
		t.Fatalf("bad responses usage: %+v", usage)
	}
	details, ok := usage["input_tokens_details"].(map[string]any)
	if !ok || details["cached_tokens"] != float64(2) {
		t.Fatalf("bad cached details: %+v", usage["input_tokens_details"])
	}
}

func TestParseOpenAIStreamChunkReadsUsageOnly(t *testing.T) {
	chunk := parseOpenAIStreamChunk([]byte(`{"choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`))
	if !chunk.Usage.Present || chunk.Usage.InputTokens != 8 || chunk.Usage.OutputTokens != 4 || chunk.Usage.TotalTokens != 12 {
		t.Fatalf("bad stream usage: %+v", chunk.Usage)
	}
	if chunk.Content != "" || len(chunk.ToolCalls) != 0 {
		t.Fatalf("usage-only chunk should not include deltas: %+v", chunk)
	}
}

func TestStreamAnthropicIncludesFinalUsage(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamAnthropic(w, body, "kimi-k2.6")
	out := w.Body.String()
	for _, want := range []string{`"input_tokens":7`, `"output_tokens":3`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStreamResponsesIncludesCompletedUsage(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamResponses(w, body, "kimi-k2.6")
	out := w.Body.String()
	for _, want := range []string{`event: response.completed`, `"input_tokens":7`, `"output_tokens":3`, `"total_tokens":10`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStreamResponsesFromAnthropicAccumulatesUsage(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0,"total_tokens":100}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":50}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	w := httptest.NewRecorder()
	streamResponsesFromAnthropic(w, body, "kimi-k2.6")
	out := w.Body.String()
	if !strings.Contains(out, `"total_tokens":150`) {
		t.Fatalf("total_tokens should be 150 (100 input + 50 output), got:\n%s", out)
	}
}

func TestStreamChatCompletionsFromAnthropicForwardsUsage(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0,"total_tokens":100}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":50}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	w := httptest.NewRecorder()
	streamChatCompletionsFromAnthropic(w, body, "kimi-k2.6", true)
	out := w.Body.String()
	for _, want := range []string{`"prompt_tokens":100`, `"completion_tokens":50`, `"total_tokens":150`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

func TestStreamChatCompletionsFromAnthropicSkipsUsageWhenNotRequested(t *testing.T) {
	body := strings.NewReader(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0}}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	w := httptest.NewRecorder()
	streamChatCompletionsFromAnthropic(w, body, "kimi-k2.6", false)
	if strings.Contains(w.Body.String(), `"usage"`) {
		t.Fatalf("usage chunk should not appear when includeUsage=false:\n%s", w.Body.String())
	}
}

func TestReasoningContentCacheBoundedLRU(t *testing.T) {
	reasoningContentCache.Lock()
	reasoningContentCache.ll = list.New()
	reasoningContentCache.keys = map[string]*list.Element{}
	reasoningContentCache.Unlock()

	// fill to cap-1, re-touch the first key (moves to front), then push one
	// more. the first key should survive; the second key (oldest now) should
	// be the one evicted.
	for i := 0; i < reasoningCacheCap-1; i++ {
		cacheReasoningContent([]OAIToolCall{{ID: fmt.Sprintf("call_%d", i), Function: OAICallFunction{Name: "x"}}}, fmt.Sprintf("r%d", i))
	}
	if got := cachedReasoningContent([]OAIToolCall{{ID: "call_0"}}); got != "r0" {
		t.Fatalf("re-touch should not lose the first key, got %q", got)
	}
	cacheReasoningContent([]OAIToolCall{{ID: fmt.Sprintf("call_%d", reasoningCacheCap-1), Function: OAICallFunction{Name: "x"}}}, fmt.Sprintf("r%d", reasoningCacheCap-1))
	if got := cachedReasoningContent([]OAIToolCall{{ID: "call_0"}}); got != "r0" {
		t.Fatalf("call_0 should still be present after re-touch, got %q", got)
	}

	// fill past the cap. first key must be evicted; later keys must remain.
	for i := 0; i <= reasoningCacheCap; i++ {
		cacheReasoningContent([]OAIToolCall{{ID: fmt.Sprintf("call_%d", i), Function: OAICallFunction{Name: "x"}}}, fmt.Sprintf("r%d", i))
	}
	reasoningContentCache.Lock()
	size := len(reasoningContentCache.keys)
	_, hasFirst := reasoningContentCache.keys["call_0"]
	_, hasLast := reasoningContentCache.keys[fmt.Sprintf("call_%d", reasoningCacheCap)]
	reasoningContentCache.Unlock()

	if size > reasoningCacheCap {
		t.Fatalf("cache size %d exceeds cap %d", size, reasoningCacheCap)
	}
	if hasFirst {
		t.Fatal("call_0 should have been evicted")
	}
	if !hasLast {
		t.Fatalf("call_%d should still be present", reasoningCacheCap)
	}
}

func TestMergeUsagePreservesUpstreamTotal(t *testing.T) {
	// an upstream that already reports total == input+output should not be
	// clobbered by the sum-fallback.
	a := tokenUsage{Present: true, InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	b := tokenUsage{Present: true, InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	got := mergeUsage(a, b)
	if got.TotalTokens != 150 {
		t.Fatalf("mergeUsage should preserve upstream total=150, got %d", got.TotalTokens)
	}
}

func TestShouldRestartForLaunch(t *testing.T) {
	cases := []struct {
		active, requested string
		want              bool
	}{
		{"", "opencode-go", false},         // unknown active → keep running
		{"opencode-go", "opencode-go", false}, // match → keep
		{"opencode-go", "cloudflare", true},   // mismatch → restart
	}
	for _, c := range cases {
		if got := shouldRestartForLaunch(c.active, c.requested); got != c.want {
			t.Errorf("shouldRestartForLaunch(%q,%q)=%v want %v", c.active, c.requested, got, c.want)
		}
	}
}

func TestActiveProviderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.WriteFile(activeProviderFile(), []byte("cloudflare"), 0644); err != nil {
		t.Fatal(err)
	}
	got, err := readActiveProvider()
	if err != nil {
		t.Fatal(err)
	}
	if got != "cloudflare" {
		t.Fatalf("readActiveProvider=%q want cloudflare", got)
	}
}

func TestStopRunningServerNoPidFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := stopRunningServer(); err != nil {
		t.Fatalf("stopRunningServer with no pid file should be a no-op, got %v", err)
	}
}

func TestStreamAnthropicForwardsToolCalls(t *testing.T) {
	reasoningContentCache.Lock()
	reasoningContentCache.ll = list.New()
	reasoningContentCache.keys = map[string]*list.Element{}
	reasoningContentCache.Unlock()
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"Need pwd.","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamAnthropic(w, body, "deepseek-v4-flash")
	out := w.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"name":"Bash"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"command\":"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","call_id":"call_abc","name":"Bash","arguments":"{\"command\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_abc","output":"/tmp"}]`))
	if messages[0].ReasoningContent != "Need pwd." {
		t.Fatalf("missing cached reasoning content: %+v", messages[0])
	}
}

func TestStreamResponsesForwardsToolCalls(t *testing.T) {
	reasoningContentCache.Lock()
	reasoningContentCache.ll = list.New()
	reasoningContentCache.keys = map[string]*list.Element{}
	reasoningContentCache.Unlock()
	body := strings.NewReader(strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"I should call the tool.","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	w := httptest.NewRecorder()
	streamResponses(w, body, "deepseek-v4-flash")
	out := w.Body.String()
	for _, want := range []string{
		"event: response.output_item.added",
		`"type":"function_call"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"cmd\":\"pwd\"}"`,
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "response.output_text.delta") {
		t.Fatalf("tool-only stream should not emit text deltas:\n%s", out)
	}
	messages := responsesInputToMessages([]byte(`[{"type":"function_call","call_id":"call_abc","name":"shell","arguments":"{\"cmd\":\"pwd\"}"},{"type":"function_call_output","call_id":"call_abc","output":"/tmp"}]`))
	if messages[0].ReasoningContent != "I should call the tool." {
		t.Fatalf("missing cached reasoning content: %+v", messages[0])
	}
}

func TestSanitizeOAIToolMessagesInsertsMissingBeforeNextMessage(t *testing.T) {
	in := []OAIMessage{
		{Role: "user", Content: "run pwd"},
		{Role: "assistant", ToolCalls: []OAIToolCall{{ID: "call_missing", Type: "function", Function: OAICallFunction{Name: "Bash"}}}},
		{Role: "assistant", Content: "done"},
	}
	out := sanitizeOAIToolMessages(in)
	if len(out) != 4 {
		t.Fatalf("expected placeholder insertion, got %+v", out)
	}
	if out[2].Role != "tool" || out[2].ToolCallID != "call_missing" || out[2].Content != unavailableToolResultContent {
		t.Fatalf("bad placeholder at index 2: %+v", out[2])
	}
}

func TestSanitizeOAIToolMessagesDropsLateToolMessage(t *testing.T) {
	in := []OAIMessage{
		{Role: "assistant", ToolCalls: []OAIToolCall{{ID: "call_1", Type: "function", Function: OAICallFunction{Name: "Bash"}}}},
		{Role: "assistant", Content: "done"},
		{Role: "tool", ToolCallID: "call_1", Content: "late result"},
	}
	out := sanitizeOAIToolMessages(in)
	if len(out) != 3 {
		t.Fatalf("expected placeholder plus assistant only, got %+v", out)
	}
	if out[1].Role != "tool" || out[1].ToolCallID != "call_1" || out[1].Content != unavailableToolResultContent {
		t.Fatalf("expected placeholder before next assistant, got %+v", out[1])
	}
	if out[2].Role != "assistant" || out[2].Content != "done" {
		t.Fatalf("assistant message not preserved after placeholder: %+v", out[2])
	}
}

func TestSanitizeOAIToolMessagesPreservesValidConsecutiveResults(t *testing.T) {
	in := []OAIMessage{
		{Role: "assistant", ToolCalls: []OAIToolCall{
			{ID: "call_a", Type: "function", Function: OAICallFunction{Name: "Bash"}},
			{ID: "call_b", Type: "function", Function: OAICallFunction{Name: "Read"}},
		}},
		{Role: "tool", ToolCallID: "call_b", Content: "b"},
		{Role: "tool", ToolCallID: "call_a", Content: "a"},
		{Role: "user", Content: "thanks"},
	}
	out := sanitizeOAIToolMessages(in)
	if len(out) != len(in) {
		t.Fatalf("valid sequence should remain same length, got %+v", out)
	}
	if out[1].Content != "b" || out[2].Content != "a" {
		t.Fatalf("existing tool result order should be preserved: %+v", out)
	}
}

func TestSanitizeRawChatToolMessagesPreservesUnknownFields(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","messages":[{"role":"assistant","content":"","name":"assistant-name","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}],"audio":{"id":"aud"}},{"role":"assistant","content":"done","refusal":"no"}]}`)
	out := sanitizeRawChatToolMessages(body)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(req["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 {
		t.Fatalf("expected inserted placeholder, got %d messages: %s", len(messages), out)
	}
	if _, ok := messages[0]["name"]; !ok {
		t.Fatalf("unknown assistant field name was dropped: %s", messages[0])
	}
	if _, ok := messages[0]["audio"]; !ok {
		t.Fatalf("unknown assistant field audio was dropped: %s", messages[0])
	}
	if _, ok := messages[2]["refusal"]; !ok {
		t.Fatalf("unknown later assistant field refusal was dropped: %s", messages[2])
	}
	var placeholder struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(marshalJSON(messages[1]), &placeholder); err != nil {
		t.Fatal(err)
	}
	if placeholder.Role != "tool" || placeholder.ToolCallID != "call_1" || placeholder.Content != unavailableToolResultContent {
		t.Fatalf("bad placeholder: %+v", placeholder)
	}
}

func TestSanitizeRawChatToolMessagesDropsLateToolMessage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{}"}}]},{"role":"assistant","content":"done"},{"role":"tool","tool_call_id":"call_1","content":"late"}]}`)
	out := sanitizeRawChatToolMessages(body)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatal(err)
	}
	var roles []struct {
		Role       string `json:"role"`
		ToolCallID string `json:"tool_call_id"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(req["messages"], &roles); err != nil {
		t.Fatal(err)
	}
	if len(roles) != 3 {
		t.Fatalf("late tool should be dropped and placeholder inserted, got %+v", roles)
	}
	if roles[1].Role != "tool" || roles[1].ToolCallID != "call_1" || roles[1].Content != unavailableToolResultContent {
		t.Fatalf("expected placeholder at index 1, got %+v", roles[1])
	}
	if roles[2].Role != "assistant" || roles[2].Content != "done" {
		t.Fatalf("expected assistant after placeholder, got %+v", roles[2])
	}
}

func TestResolveProviderFlagWinsOverEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "opencode-go")
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().String("provider", "", "")
	if err := cmd.Flags().Set("provider", "cloudflare"); err != nil {
		t.Fatal(err)
	}
	got, err := resolveProvider(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got != "cloudflare" {
		t.Fatalf("flag should win: got %q", got)
	}
}

func TestResolveProviderEnvWinsOverSingle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "opencode-go")
	if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "opencode-go" {
		t.Fatalf("env should win over single-configured: got %q", got)
	}
}

func TestResolveProviderSingleConfiguredWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "")
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := resolveProvider(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "opencode-go" {
		t.Fatalf("single configured should win: got %q", got)
	}
}

func TestResolveProviderAmbiguous(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "")
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := resolveProvider(nil)
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	if !strings.Contains(err.Error(), "multiple providers") {
		t.Fatalf("error should mention multiple providers: %v", err)
	}
}

func TestResolveProviderUnknownValueRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "bogus")

	_, err := resolveProvider(nil)
	if err == nil {
		t.Fatal("expected unknown-provider error")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error should name the bad value: %v", err)
	}
}

func TestResolveProviderNoConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_PROVIDER", "")

	_, err := resolveProvider(nil)
	if err == nil {
		t.Fatal("expected no-config error")
	}
	if !strings.Contains(err.Error(), "no provider configured") {
		t.Fatalf("error should say no provider: %v", err)
	}
}

func TestMigrateConfigMovesUpstreamToProviderFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old := `{"host":"127.0.0.1","port":3456,"api_key":"local","upstream_base_url":"https://gateway.ai.cloudflare.com/v1/acct-123/gw-456/compat/v1","upstream_api_key":"tok-xyz","upstream_auth":"bearer","endpoint_overrides":[]}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" || cfg.Port != 3456 {
		t.Fatalf("config lost: %+v", cfg)
	}

	p, err := loadProviderConfig("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamBaseURL != "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1" {
		t.Fatalf("cloudflare upstream_base_url = %q", p.UpstreamBaseURL)
	}
	if p.Gateway != "gw-456" {
		t.Fatalf("cloudflare gateway = %q, want gw-456", p.Gateway)
	}
	if p.UpstreamAPIKey != "tok-xyz" {
		t.Fatalf("cloudflare upstream_api_key = %q", p.UpstreamAPIKey)
	}

	// config.json should no longer carry upstream fields
	b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	if strings.Contains(string(b), "upstream_base_url") || strings.Contains(string(b), "tok-xyz") {
		t.Fatalf("config.json still has upstream fields after migration:\n%s", string(b))
	}
}

func TestMigrateConfigOpencodeGoFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old := `{"host":"127.0.0.1","port":3456,"upstream_api_key":"cfgate-cc-key","upstream_auth":"bearer"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	p, err := loadProviderConfig("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamAPIKey != "cfgate-cc-key" {
		t.Fatalf("opencode-go upstream_api_key = %q", p.UpstreamAPIKey)
	}
}

func TestMigrateCloudflareURLIfNeeded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old := `{"upstream_base_url":"https://gateway.ai.cloudflare.com/v1/acct-123/gw-456/compat/v1","upstream_api_key":"tok-xyz","upstream_auth":"bearer"}`
	if err := os.WriteFile(filepath.Join(dir, "cloudflare.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	migrateCloudflareURLIfNeeded()

	p, err := loadProviderConfig("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamBaseURL != "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1" {
		t.Fatalf("upstream_base_url = %q", p.UpstreamBaseURL)
	}
	if p.Gateway != "gw-456" {
		t.Fatalf("gateway = %q, want gw-456", p.Gateway)
	}
	if p.UpstreamAPIKey != "tok-xyz" {
		t.Fatalf("upstream_api_key lost: %q", p.UpstreamAPIKey)
	}

	// idempotent: a second pass leaves the file alone.
	migrateCloudflareURLIfNeeded()
	p, _ = loadProviderConfig("cloudflare")
	if p.UpstreamBaseURL != "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1" {
		t.Fatalf("second pass rewrote URL: %q", p.UpstreamBaseURL)
	}
}

func TestStripWorkersAIPrefix(t *testing.T) {
	cfg := ProviderConfig{UpstreamBaseURL: "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1"}

	t.Run("strips workers-ai prefix", func(t *testing.T) {
		body := []byte(`{"model":"workers-ai/@cf/zai-org/glm-5.2","messages":[]}`)
		out, wire := cloudflarePrepareBody(body, cfg)
		if wire != "@cf/zai-org/glm-5.2" {
			t.Fatalf("wire model = %q", wire)
		}
		if !strings.Contains(string(out), `"model":"@cf/zai-org/glm-5.2"`) {
			t.Fatalf("body model not stripped: %s", out)
		}
		if strings.Contains(string(out), "workers-ai/") {
			t.Fatalf("workers-ai/ still present: %s", out)
		}
	})

	t.Run("leaves bare @cf/ alone", func(t *testing.T) {
		body := []byte(`{"model":"@cf/zai-org/glm-5.2","messages":[]}`)
		out, wire := cloudflarePrepareBody(body, cfg)
		if wire != "@cf/zai-org/glm-5.2" {
			t.Fatalf("wire model = %q", wire)
		}
		if string(out) != string(body) {
			t.Fatalf("body should be unchanged: %s", out)
		}
	})

	t.Run("leaves non-cf model alone", func(t *testing.T) {
		body := []byte(`{"model":"anthropic/claude-sonnet-4","messages":[]}`)
		out, wire := cloudflarePrepareBody(body, cfg)
		if wire != "anthropic/claude-sonnet-4" {
			t.Fatalf("wire model = %q", wire)
		}
		if string(out) != string(body) {
			t.Fatalf("body should be unchanged: %s", out)
		}
	})

	t.Run("no-op for non-cloudflare upstream", func(t *testing.T) {
		other := ProviderConfig{UpstreamBaseURL: "https://example.com/v1"}
		body := []byte(`{"model":"workers-ai/@cf/foo","messages":[]}`)
		out, wire := cloudflarePrepareBody(body, other)
		if wire != "" {
			t.Fatalf("wire model = %q, want empty for non-cloudflare", wire)
		}
		if string(out) != string(body) {
			t.Fatalf("body should be unchanged: %s", out)
		}
	})
}

func TestInjectsGatewayHeaderForCFModels(t *testing.T) {
	cfg := ProviderConfig{
		UpstreamBaseURL: "https://api.cloudflare.com/client/v4/accounts/acct-123/ai/v1",
		Gateway:         "gw-456",
	}

	t.Run("cf model sets header", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream", nil)
		applyCloudflareGatewayHeader(req, cfg, "@cf/zai-org/glm-5.2")
		if got := req.Header.Get("cf-aig-gateway-id"); got != "gw-456" {
			t.Fatalf("cf-aig-gateway-id = %q, want gw-456", got)
		}
	})

	t.Run("non-cf model leaves header off", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, "https://upstream", nil)
		applyCloudflareGatewayHeader(req, cfg, "anthropic/claude-sonnet-4")
		if got := req.Header.Get("cf-aig-gateway-id"); got != "" {
			t.Fatalf("cf-aig-gateway-id = %q, want empty", got)
		}
	})

	t.Run("empty gateway leaves header off", func(t *testing.T) {
		noGW := cfg
		noGW.Gateway = ""
		req, _ := http.NewRequest(http.MethodPost, "https://upstream", nil)
		applyCloudflareGatewayHeader(req, noGW, "@cf/foo")
		if got := req.Header.Get("cf-aig-gateway-id"); got != "" {
			t.Fatalf("cf-aig-gateway-id = %q, want empty", got)
		}
	})
}

func TestMigrateConfigLeavesExistingProviderFileAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// both files exist; migration should NOT clobber the existing provider file
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"existing"}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := `{"host":"127.0.0.1","upstream_api_key":"newer","upstream_auth":"bearer"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
	p, err := loadProviderConfig("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamAPIKey != "existing" {
		t.Fatalf("existing provider file was clobbered: got %q", p.UpstreamAPIKey)
	}
}

func TestMigrateConfigIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	old := `{"host":"127.0.0.1","upstream_api_key":"k","upstream_auth":"bearer"}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		if _, err := loadConfig(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	p, err := loadProviderConfig("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamAPIKey != "k" {
		t.Fatalf("upstream_api_key after repeated calls = %q", p.UpstreamAPIKey)
	}
}

func TestSetupOpencodeGoWritesProviderFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	t.Setenv("CFGATE_CC_API_KEY", "")

	cmd := setupOpencodeGoCmd()
	cmd.SetArgs([]string{"--api-key", "test-cfgate-cc-key"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir, "opencode-go.json"))
	if err != nil {
		t.Fatalf("opencode-go.json not written: %v", err)
	}
	var p ProviderConfig
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.UpstreamAPIKey != "test-cfgate-cc-key" {
		t.Fatalf("upstream_api_key = %q, want test-cfgate-cc-key", p.UpstreamAPIKey)
	}
	if p.UpstreamAuth != "both" {
		t.Fatalf("upstream_auth = %q, want both (opencode-go sends Bearer + x-api-key)", p.UpstreamAuth)
	}

	// api_key must NOT land in config.json — that's the local proxy key,
	// not the upstream key. this is the whole point of the split.
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		b, _ := os.ReadFile(filepath.Join(dir, "config.json"))
		if strings.Contains(string(b), "test-cfgate-cc-key") {
			t.Fatalf("upstream key leaked into config.json:\n%s", string(b))
		}
	}
}

func TestApplyUpstreamAuthNoLongerFallsBackToLocalKey(t *testing.T) {
	// empty UpstreamAPIKey must NOT send any Authorization header. pre-refactor
	// this fell through to cfg.APIKey (ocgo-compat hack); post-refactor the
	// local key is structurally unreachable from applyUpstreamAuth because
	// the function only takes ProviderConfig.
	p := ProviderConfig{UpstreamAuth: "bearer"}

	req, _ := http.NewRequest(http.MethodPost, "http://example", nil)
	applyUpstreamAuth(req, p)

	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization should be empty when upstream key is empty, got %q", got)
	}

	// sanity: with the upstream key set, the bearer header is correct
	p.UpstreamAPIKey = "upstream-key"
	req2, _ := http.NewRequest(http.MethodPost, "http://example", nil)
	applyUpstreamAuth(req2, p)
	if got := req2.Header.Get("Authorization"); got != "Bearer upstream-key" {
		t.Fatalf("Authorization = %q, want Bearer upstream-key", got)
	}
}

func TestApplyUpstreamAuthBothMode(t *testing.T) {
	// opencode-go's /v1/chat/completions wants Bearer, /v1/messages wants
	// x-api-key. the "both" mode sends both so either endpoint accepts it.
	p := ProviderConfig{UpstreamAuth: "both", UpstreamAPIKey: "k"}
	req, _ := http.NewRequest(http.MethodPost, "http://example", nil)
	applyUpstreamAuth(req, p)
	if got := req.Header.Get("Authorization"); got != "Bearer k" {
		t.Fatalf("Authorization = %q, want Bearer k", got)
	}
	if got := req.Header.Get("x-api-key"); got != "k" {
		t.Fatalf("x-api-key = %q, want k", got)
	}
}

func TestLoadActiveProviderAppliesEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_base_url":"https://file.example/v1","upstream_api_key":"file-key","upstream_auth":"bearer"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFGATE_CC_UPSTREAM_BASE_URL", "https://env.example/v1")
	t.Setenv("CFGATE_CC_UPSTREAM_API_KEY", "env-key")

	p, err := loadActiveProvider("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if p.UpstreamBaseURL != "https://env.example/v1" {
		t.Fatalf("upstream_base_url = %q, want env override", p.UpstreamBaseURL)
	}
	if p.UpstreamAPIKey != "env-key" {
		t.Fatalf("upstream_api_key = %q, want env override", p.UpstreamAPIKey)
	}
}

func TestLoadActiveProviderOpencodeGoDefaultURL(t *testing.T) {
	// an opencode-go provider file with no upstream_base_url should still
	// resolve to a usable URL via the opencode-go default fallback.
	dir := t.TempDir()
	t.Setenv("CFGATE_CC_CONFIG_DIR", dir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode-go.json"), []byte(`{"upstream_api_key":"k"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CFGATE_CC_UPSTREAM_BASE_URL", "")

	p, err := loadActiveProvider("opencode-go")
	if err != nil {
		t.Fatal(err)
	}
	if got := openAIURL(p); got != "https://opencode.ai/zen/go/v1/chat/completions" {
		t.Fatalf("openAIURL = %q, want opencode-go default", got)
	}
}

func TestDebugLoggingProxyMessages(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "debug.log")
	t.Setenv("CFGATE_CC_DEBUG", logPath)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret-key" {
			t.Errorf("upstream Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	// enable debug + re-arm after the test so other tests stay quiet
	prev := debugEnabled
	debugEnabled = false
	t.Cleanup(func() {
		debugEnabled = prev
		log.SetOutput(os.Stderr)
	})
	setupDebug()

	cfg := ProviderConfig{Name: "opencode-go", UpstreamBaseURL: upstream.URL, UpstreamAPIKey: "secret-key", UpstreamAuth: "bearer"}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyMessages(w, r, cfg) }))
	defer proxy.Close()

	body := strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	debugLog := string(logBytes)
	for _, want := range []string{
		"[messages] incoming POST /v1/messages",
		"[upstream] POST " + upstream.URL + "/chat/completions",
		"[upstream] response 200 application/json",
		"[messages] client response: 200",
		"Authorization: [REDACTED]",
		`"content":"hi"`,
	} {
		if !strings.Contains(debugLog, want) {
			t.Errorf("debug log missing %q\n---\n%s\n---", want, debugLog)
		}
	}
	if strings.Contains(debugLog, "secret-key") {
		t.Errorf("debug log leaked upstream key\n---\n%s\n---", debugLog)
	}
}

func TestDebugLoggingStderr(t *testing.T) {
	t.Setenv("CFGATE_CC_DEBUG", "1")

	// capture stderr
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	prev := debugEnabled
	debugEnabled = false
	t.Cleanup(func() {
		debugEnabled = prev
		log.SetOutput(orig)
		os.Stderr = orig
	})
	setupDebug()
	if !debugEnabled {
		t.Fatal("setupDebug did not enable debug")
	}

	dlogf("hello from stderr")

	w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "hello from stderr") {
		t.Errorf("stderr capture missing log line: %q", string(out))
	}
	if !strings.Contains(string(out), "debug log: 1") {
		t.Errorf("stderr capture missing setup banner: %q", string(out))
	}
}

func TestDebugLoggingUnset(t *testing.T) {
	t.Setenv("CFGATE_CC_DEBUG", "")

	prev := debugEnabled
	debugEnabled = false
	t.Cleanup(func() { debugEnabled = prev })
	setupDebug()
	if debugEnabled {
		t.Fatal("setupDebug enabled debug with CFGATE_CC_DEBUG unset")
	}

	// capture stderr to confirm dlogf is a no-op
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	dlogf("this should be dropped")
	w.Close()
	out, _ := io.ReadAll(r)
	os.Stderr = orig
	if strings.Contains(string(out), "this should be dropped") {
		t.Errorf("debug log emitted with CFGATE_CC_DEBUG unset: %q", string(out))
	}
}

func TestDebugLoggingStreamSSE(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "debug.log")
	t.Setenv("CFGATE_CC_DEBUG", logPath)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		// 5 events so first 2 + last 2 are visible
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	prev := debugEnabled
	debugEnabled = false
	t.Cleanup(func() {
		debugEnabled = prev
		log.SetOutput(os.Stderr)
	})
	setupDebug()

	cfg := ProviderConfig{Name: "opencode-go", UpstreamBaseURL: upstream.URL, UpstreamAPIKey: "k", UpstreamAuth: "bearer"}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyMessages(w, r, cfg) }))
	defer proxy.Close()

	body := strings.NewReader(`{"model":"m","max_tokens":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	// drain the response so the proxy's streamReader hits eof and writes
	// the [messages] stream end line before we read the log. Close() on a
	// streaming body returns as soon as headers are in, which races the
	// server-side log write and flakes on fast runners.
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	debugLog := string(logBytes)
	for _, want := range []string{
		"[upstream] response 200 text/event-stream",
		"[messages] stream end: events=5",
		`event[first/1]: data: {"i":0}`,
		`event[last/5]: data: {"i":4}`,
	} {
		if !strings.Contains(debugLog, want) {
			t.Errorf("debug log missing %q\n---\n%s\n---", want, debugLog)
		}
	}
}

func ptr[T any](v T) *T { return &v }
