package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// resolvedInstanceName is the --name / CFGATE_CC_NAME value for the current
// command invocation. resolved once at command entry by resolveInstanceName
// and read by all path helpers so a named instance's state never bleeds
// into the global config dir. empty = back-compat mode, all state under
// configDir() directly.
var resolvedInstanceName string

// configFile / pidFile / activeProviderFile delegate to the per-instance
// helpers using the resolved instance name. kept as bare functions for
// the rest of main.go's call sites.
func configFile() string            { return instanceConfigFile(resolvedInstanceName) }
func pidFile() string               { return instancePidFile(resolvedInstanceName) }
func activeProviderFile() string    { return instanceActiveProviderFile(resolvedInstanceName) }
func portFile() string              { return instancePortFile(resolvedInstanceName) }
func logFile() string               { return instanceLogFile(resolvedInstanceName) }

// migrateSentinelFile marks that the legacy per-instance layout has been
// folded into the single config.json. presence = done.
func migrateSentinelFile() string {
	return filepath.Join(instanceDir(resolvedInstanceName), "migrated-v2")
}

// loadInstance returns the per-instance config. runs migrateToV2 first
// (idempotent, gated on migrateSentinelFile) so callers always see the
// new shape. pure read on success — port allocation and other side
// effects live in allocateInstancePort.
func loadInstance() (Instance, error) {
	if err := migrateToV2(); err != nil {
		return Instance{}, err
	}
	var inst Instance
	b, err := os.ReadFile(configFile())
	if errors.Is(err, os.ErrNotExist) {
		return defaultInstance(), nil
	}
	if err != nil {
		return inst, err
	}
	if err := json.Unmarshal(b, &inst); err != nil {
		return inst, fmt.Errorf("parse %s: %w", configFile(), err)
	}
	if inst.Host == "" {
		inst.Host = defaultHost
	}
	if inst.Port == 0 {
		inst.Port = defaultPort
	}
	return inst, nil
}

// saveInstance writes the per-instance config atomically-ish: marshal,
// write to config.json with 0600 (config holds secrets). replaces
// saveConfig and saveProviderConfig.
func saveInstance(inst Instance) error {
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configFile(), append(b, '\n'), 0600)
}

// defaultInstance returns the back-compat defaults (host 127.0.0.1,
// port defaultPort, empty provider + mapping).
func defaultInstance() Instance {
	return Instance{Host: defaultHost, Port: defaultPort}
}

// applyEnvOverrides overlays CFGATE_CC_UPSTREAM_* env vars onto the
// provider config. runs in the serve path so the fish-alias pattern
// (env-overrides-file) still works for the active provider. env beats
// file, always.
func applyEnvOverrides(p *ProviderConfig) {
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
}

// resolveInstanceName reads --name (or CFGATE_CC_NAME) from the command
// tree and stores it in resolvedInstanceName. called from PersistentPreRun
// so the rest of the command body sees a single source of truth.
func resolveInstanceName(cmd *cobra.Command) string {
	if cmd != nil {
		if f := cmd.Flags().Lookup("name"); f != nil {
			if v := strings.TrimSpace(f.Value.String()); v != "" {
				resolvedInstanceName = v
				return resolvedInstanceName
			}
		}
	}
	resolvedInstanceName = strings.TrimSpace(os.Getenv("CFGATE_CC_NAME"))
	return resolvedInstanceName
}

// autoInstanceName returns a stable, friendly name like "opencode-go-a3f2".
// used by `launch` when --name wasn't passed so the proxy / port files
// get a unique home without the user having to pick one.
func autoInstanceName(provider string) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return provider + "-auto"
	}
	return fmt.Sprintf("%s-%s", provider, hex.EncodeToString(b[:]))
}

// resolveInstanceBase returns the http base URL for the current instance.
// pure read on the port — port allocation is handled by allocateInstancePort
// in the start path. this function only formats the URL.
func resolveInstanceBase(inst Instance) (string, error) {
	return fmt.Sprintf("http://%s:%d", inst.Host, inst.Port), nil
}

// allocateInstancePort picks a free port for a named instance, writes it
// to the `port` file, and updates the instance's Port field in-place.
// ponytail: port scan uses net.Listen("tcp", ":0") — go's stdlib idiom
// for "ask the kernel for a free port". no new dep. closes the listener
// immediately so the port is free for the subprocess to bind. if 100
// consecutive ports are taken, surface that as an error instead of
// silently grabbing something weird.
func allocateInstancePort(inst *Instance) (int, error) {
	if resolvedInstanceName == "" {
		return inst.Port, nil
	}
	// reuse a previously allocated port if the file still says it's
	// free. otherwise scan.
	if existing := readInstancePort(instanceDir(resolvedInstanceName)); existing != 0 {
		if isPortFree(inst.Host, existing) {
			inst.Port = existing
			return existing, nil
		}
	}
	start := inst.Port + 1
	if start <= inst.Port {
		start = inst.Port + 1
	}
	for p := start; p < start+100; p++ {
		if isPortFree(inst.Host, p) {
			inst.Port = p
			if err := saveInstancePort(p); err != nil {
				return 0, err
			}
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d..%d for instance %q", start, start+100, resolvedInstanceName)
}

// isPortFree binds to 127.0.0.1:<port> specifically; a wildcard bind
// could succeed against an interface-bound process and still collide on
// the loopback the proxy actually uses. ponytail: this is a probe; the
// kernel can hand the port to someone else between the close here and
// the ListenAndServe later. for a developer-only tool the race window
// is fine — move to SO_REUSEADDR / explicit reservation if it ever
// matters in practice.
func isPortFree(host string, port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
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

// --- codex-side helper files (configDir → ~/.codex, not cfgate-cc's dir) ---

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

// ensureCodexConfig writes the codex profile and model catalog for the
// active instance. called from `launch codex` so the codex CLI picks up
// the cfgate-cc base URL and the per-instance model list on first run.
func ensureCodexConfig(base string, inst Instance) error {
	path := codexConfigFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := writeCodexModelCatalog(codexModelCatalogFile(), inst); err != nil {
		return err
	}
	return writeCodexProfile(path, strings.TrimRight(base, "/")+"/v1/", resolvedInstanceName)
}

// writeCodexProfile writes the per-instance cfgate-cc-launch[-<name>].config.toml
// and updates the user's root ~/.codex/config.toml (creating it if missing)
// to reference the profile's model_provider. the legacy [profiles.X] section
// is stripped so older codex versions don't double-register.
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
		`name = "Upstream",`,
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

// stripLegacyCodexProfile removes the legacy cfgate-cc codex profile
// artifacts from a user's root ~/.codex/config.toml. the pre-0.81 codex
// CLI registered cfgate-cc via a bare `profile = "cfgate-cc-launch"`
// top-level line plus `[profiles.cfgate-cc-launch]` and
// `[model_providers.cfgate-cc-launch]` sections; the v2 cutover writes
// the new profile via writeCodexProfile, so anything left over is
// either a leftover or user-edited, and we want it gone.
//
// ponytail: a user could in theory have `profile = "cfgate-cc-launch"`
// for their own reasons (very unlikely — the cfgate-cc-launch name
// belongs to us). if that ever becomes a complaint, gate the bare-line
// strip on the presence of a [profiles.cfgate-cc-launch] block.
func stripLegacyCodexProfile(text, instanceName string) string {
	target := codexProfileNameFor(instanceName)
	var out []string
	lines := strings.Split(text, "\n")
	skipUntil := -1
	for i, line := range lines {
		if i <= skipUntil {
			continue
		}
		trimmed := strings.TrimSpace(line)
		// drop bare `profile = "<target>"` top-level lines
		if trimmed == fmt.Sprintf("profile = %q", target) {
			continue
		}
		// drop [profiles.<target>] and [profiles.<target>.*] blocks
		// AND [model_providers.<target>] and its subsections — both
		// predate the per-instance profile file and would otherwise
		// double-register.
		if strings.HasPrefix(trimmed, "[profiles."+target+"]") || strings.HasPrefix(trimmed, "[profiles."+target+".") ||
			strings.HasPrefix(trimmed, "[model_providers."+target+"]") || strings.HasPrefix(trimmed, "[model_providers."+target+".") {
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
					skipUntil = j - 1
					break
				}
				skipUntil = j
			}
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// writeCodexModelCatalog emits the codex-compatible model catalog from
// the instance's mapping. the codex CLI reads this file to know which
// upstream models are available and how to display them.
func writeCodexModelCatalog(path string, inst Instance) error {
	providerName := inst.Provider.Name
	mappings := loadModelMappings(inst)
	providerIDs, err := providerKnownModelIDs(providerName, inst.Provider)
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
			"context_window":                   meta.ContextWindow,
			"input_modalities":                 modelInputModalities(target),
			"truncation_policy":                "disabled",
			"supports_image_detail_original":   false,
			"default_reasoning_level":          meta.DefaultReasoningLevel,
			"supported_reasoning_levels":       meta.SupportedReasoning,
			"supports_search_tool":             supportsSearchTool(target),
			"shell_type":                       "shell_command",
			"visibility":                       "list",
			"supported_in_api":                 true,
			"priority":                         i,
		})
	}
	for i, id := range providerIDs {
		addModel(id, id, "upstream "+id, i)
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
