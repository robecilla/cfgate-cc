package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// legacyCloudflareURLPrefix is the old compat endpoint shape that
// migrateConfigIfNeeded rewrites. kept here so migrateToV2 can fold the
// old cloudflare.json's URL into the new shape at the same time.
const legacyCloudflareURLPrefix = "https://gateway.ai.cloudflare.com/v1/"

// migrateToV2 runs the one-shot v1→v2 cutover: reads the old layout
// (config.json + per-provider file + active-provider + model-mapping.json)
// and produces a single config.json. old files are deleted. idempotent
// via the migrateSentinelFile presence check.
//
// aborts (without touching the filesystem) if the old model-mapping.json
// had more than one provider's mappings — we don't know which to keep.
func migrateToV2() error {
	if _, err := os.Stat(migrateSentinelFile()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	dir := instanceDir(resolvedInstanceName)
	legacyProviderFile, legacyProviderName, hasProvider := findLegacyProviderFile(dir)
	legacyMappingPath := filepath.Join(dir, "model-mapping.json")
	legacyActiveProviderPath := filepath.Join(dir, "active-provider")
	legacyConfigPath := filepath.Join(dir, "config.json")

	// nothing to migrate: already on v2 layout, OR the dir is empty.
	if !hasProvider {
		if _, err := os.Stat(legacyMappingPath); errors.Is(err, os.ErrNotExist) {
			if _, err := os.Stat(legacyConfigPath); errors.Is(err, os.ErrNotExist) {
				// empty dir → leave it. write the sentinel so we don't
				// stat the whole world on every subsequent load.
				return writeMigrateSentinel()
			}
		}
	}

	inst, err := readLegacyConfig(legacyConfigPath)
	if err != nil {
		return err
	}

	if hasProvider {
		var raw map[string]json.RawMessage
		b, rerr := os.ReadFile(legacyProviderFile)
		if rerr != nil {
			return rerr
		}
		if err := json.Unmarshal(b, &raw); err != nil {
			return fmt.Errorf("parse %s: %w", legacyProviderFile, err)
		}
		var p ProviderConfig
		if err := json.Unmarshal(b, &p); err != nil {
			return fmt.Errorf("parse %s: %w", legacyProviderFile, err)
		}
		if p.UpstreamBaseURL != "" && strings.HasPrefix(p.UpstreamBaseURL, legacyCloudflareURLPrefix) {
			// fold the legacy /compat/v1 URL into the new shape: account
			// + gateway live on CloudflareOptions, base URL becomes the
			// /ai/v1 REST URL.
			rest := strings.TrimPrefix(p.UpstreamBaseURL, legacyCloudflareURLPrefix)
			parts := strings.SplitN(rest, "/", 3)
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				p.UpstreamBaseURL = buildCloudflareURL(parts[0])
				if p.Cloudflare == nil {
					p.Cloudflare = &CloudflareOptions{}
				}
				p.Cloudflare.Gateway = parts[1]
				if p.Cloudflare.Account == "" {
					p.Cloudflare.Account = parts[0]
				}
			}
		}
		_ = raw
		p.Name = legacyProviderName
		inst.Provider = p
	} else if inst.Provider.Name == "" {
		// old config.json had upstream_* fields (pre-split era). pull
		// them out into the new provider section. providerForUpstreamURL
		// guesses "opencode-go" vs "cloudflare" from the URL pattern.
		if legacy, ok := readLegacyFlatConfig(legacyConfigPath); ok {
			inst.Provider = legacy
		}
	}

	// model mapping: single-provider case, just lift it. multi-provider
	// case → abort with a clear error so the user can pick one.
	if _, err := os.Stat(legacyMappingPath); err == nil {
		providers, perr := readLegacyMappingProviders(legacyMappingPath)
		if perr != nil {
			return perr
		}
		switch len(providers) {
		case 0:
			// empty file — nothing to lift
		case 1:
			for _, m := range providers {
				inst.ModelMapping = m
			}
		default:
			names := make([]string, 0, len(providers))
			for n := range providers {
				names = append(names, n)
			}
			return fmt.Errorf("multi-provider model-mapping.json found at %s (providers: %s); consolidate to one provider per instance before upgrading. run `cfgate-cc mapping --provider <name> <tool> set ...` to move entries, then delete the rest", legacyMappingPath, strings.Join(names, ", "))
		}
	}

	if err := saveInstance(inst); err != nil {
		return err
	}

	// delete legacy files. ignore-not-exist so a partial state doesn't
	// block the upgrade.
	if hasProvider {
		_ = os.Remove(legacyProviderFile)
	}
	_ = os.Remove(legacyMappingPath)
	_ = os.Remove(legacyActiveProviderPath)
	// strip upstream_* fields from a still-present legacy config.json so
	// the next loadInstance unmarshal doesn't carry junk in Provider.
	if cleaned, err := stripLegacyUpstreamFields(legacyConfigPath); err == nil && cleaned {
		// config.json still exists; that's fine, loadInstance reads it.
	}

	return writeMigrateSentinel()
}

func writeMigrateSentinel() error {
	if err := os.MkdirAll(instanceDir(resolvedInstanceName), 0755); err != nil {
		return err
	}
	return os.WriteFile(migrateSentinelFile(), []byte("migrated to single-file config at "+time.Now().UTC().Format(time.RFC3339)+"\n"), 0644)
}

// findLegacyProviderFile scans the instance dir for <name>.json files
// matching a known provider. the old layout had one such file (or zero).
func findLegacyProviderFile(dir string) (path string, name string, found bool) {
	for _, n := range knownProviders {
		cand := filepath.Join(dir, n+".json")
		if _, err := os.Stat(cand); err == nil {
			return cand, n, true
		}
	}
	return "", "", false
}

// readLegacyConfig reads config.json if it exists, returning a default
// Instance otherwise. if config.json has flat upstream_* fields, those
// are returned separately by readLegacyFlatConfig.
func readLegacyConfig(path string) (Instance, error) {
	inst := defaultInstance()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return inst, nil
	}
	if err != nil {
		return inst, err
	}
	if err := json.Unmarshal(b, &inst); err != nil {
		return inst, fmt.Errorf("parse %s: %w", path, err)
	}
	if inst.Host == "" {
		inst.Host = defaultHost
	}
	if inst.Port == 0 {
		inst.Port = defaultPort
	}
	return inst, nil
}

// readLegacyFlatConfig returns the upstream_* fields from a pre-split
// config.json as a ProviderConfig. bool return is "found any upstream
// field" so the caller knows whether to overwrite an already-populated
// Provider section.
func readLegacyFlatConfig(path string) (ProviderConfig, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return ProviderConfig{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return ProviderConfig{}, false
	}
	has := false
	for _, k := range []string{"upstream_base_url", "upstream_api_key", "upstream_auth", "upstream_auth_hdr", "upstream_extra_hdr", "endpoint_overrides"} {
		if _, ok := raw[k]; ok {
			has = true
			break
		}
	}
	if !has {
		return ProviderConfig{}, false
	}
	var p ProviderConfig
	_ = json.Unmarshal(b, &p)
	if p.Name == "" {
		p.Name = providerForUpstreamURL(p.UpstreamBaseURL)
	}
	return p, true
}

// readLegacyMappingProviders returns a map of provider-name → tool-mapping.
// the on-disk shape was {<provider>: {<tool>: {<source>: <target>}}}. the
// legacy tool-scoped shape {<tool>: {...}} is detected and lifted into
// the opencode-go bucket, matching the v1 migration behaviour.
func readLegacyMappingProviders(path string) (map[string]map[string]map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := map[string]map[string]map[string]string{}
	if isLegacyToolScoped(raw) {
		// lift into opencode-go; v1 used opencode-go as the implicit
		// default for the legacy shape.
		var flat map[string]map[string]string
		if err := json.Unmarshal(b, &flat); err != nil {
			return nil, fmt.Errorf("parse legacy %s: %w", path, err)
		}
		out["opencode-go"] = flat
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

func isLegacyToolScoped(raw map[string]json.RawMessage) bool {
	for _, tool := range []string{"claude", "codex"} {
		if _, ok := raw[tool]; ok {
			return true
		}
	}
	return false
}

// stripLegacyUpstreamFields rewrites a legacy config.json in-place to
// remove the upstream_* fields that have been migrated. returns true
// if the file was rewritten.
func stripLegacyUpstreamFields(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return false, nil
	}
	changed := false
	for _, k := range []string{"upstream_base_url", "upstream_api_key", "upstream_auth", "upstream_auth_hdr", "upstream_extra_hdr", "endpoint_overrides"} {
		if _, ok := raw[k]; ok {
			delete(raw, k)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0600)
}
