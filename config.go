package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// pluginConfig mirrors the ConfigFields the descriptor publishes.
//
// CPA hands every plugin its own config block as YAML in `plugin.register`
// (rpcLifecycleRequest.ConfigYAML), so these values come from
// plugins.configs.cpa-mistral-vibe in config.yaml with no file on disk. The
// environment and a small JSON file remain as fallbacks for local runs.
type pluginConfig struct {
	apiKey   string
	baseURL  string
	planBase string
	planPath string
	model    string
	timeout  time.Duration
}

var configCache struct {
	loaded bool
	value  pluginConfig
}

// applyConfigYAML reads the config block CPA sends at registration. The block is
// a flat YAML mapping; only the scalar keys this plugin declares matter, so a
// line-oriented reader is enough and avoids a YAML dependency.
func applyConfigYAML(cfg *pluginConfig, raw []byte) {
	if cfg == nil || len(raw) == 0 {
		return
	}
	values := parseFlatYAML(string(raw))
	cfg.apiKey = firstNonEmpty(values["api_key"], values["apikey"], cfg.apiKey)
	cfg.baseURL = firstNonEmpty(values["base_url"], values["baseurl"], cfg.baseURL)
	cfg.planBase = firstNonEmpty(values["plan_base"], cfg.planBase)
	cfg.planPath = firstNonEmpty(values["plan_path"], cfg.planPath)
	cfg.model = firstNonEmpty(values["model"], cfg.model)
}

var yamlLine = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*)$`)

// parseFlatYAML reads top-level scalars: `key: value`, with optional quotes and
// trailing comments. Nested blocks (`store:`) are skipped.
func parseFlatYAML(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		match := yamlLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key := strings.ToLower(match[1])
		value := strings.TrimSpace(match[2])
		if value == "" {
			continue
		}
		if index := strings.Index(value, " #"); index >= 0 {
			value = strings.TrimSpace(value[:index])
		}
		value = strings.Trim(value, `"'`)
		if value != "" && value != "{}" && value != "null" {
			out[key] = value
		}
	}
	return out
}

// currentConfig is the resolved configuration for this process.
func currentConfig() pluginConfig {
	if configCache.loaded {
		return configCache.value
	}
	cfg := pluginConfig{
		apiKey:   envOr("VIBE_API_KEY", envOr("MISTRAL_API_KEY", "")),
		baseURL:  envOr("VIBE_API_BASE", ""),
		planBase: envOr("VIBE_PLAN_BASE", ""),
		planPath: envOr("VIBE_PLAN_PATH", ""),
		model:    envOr("VIBE_MODEL", ""),
		timeout:  httpTimeout,
	}
	if path := configPath(); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			applyConfigYAML(&cfg, raw)
		}
	}
	configCache.loaded = true
	configCache.value = cfg
	return cfg
}

// configure refreshes the cached config from a registration payload.
func configure(raw []byte) {
	cfg := currentConfig()
	applyConfigYAML(&cfg, raw)
	configCache.loaded = true
	configCache.value = cfg
}

func configPath() string {
	for _, key := range []string{"CLIPROXY_PLUGIN_CONFIG", "VIBE_PLUGIN_CONFIG"} {
		if path := strings.TrimSpace(os.Getenv(key)); path != "" {
			return path
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".vibe", "cpa-mistral-vibe.json"),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// jsonUnmarshalOK is a small helper for the fallback config file.
func jsonUnmarshalOK(raw []byte, target any) bool {
	return json.Unmarshal(raw, target) == nil
}
