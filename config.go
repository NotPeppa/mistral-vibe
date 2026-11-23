package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
}

var (
	configMu    sync.RWMutex
	configCache struct {
		loaded bool
		value  pluginConfig
	}
)

// applyConfigYAML reads the config block CPA sends at registration. The block is
// a flat YAML mapping; only the scalar keys this plugin declares matter, so a
// line-oriented reader is enough and avoids a YAML dependency.
func applyConfigYAML(cfg *pluginConfig, raw []byte) {
	if cfg == nil || len(raw) == 0 {
		return
	}
	values := parseFlatYAML(string(raw))
	applyConfigMap(cfg, values)
}

func applyConfigMap(cfg *pluginConfig, values map[string]string) {
	if cfg == nil || len(values) == 0 {
		return
	}
	cfg.apiKey = firstNonEmpty(values["api_key"], values["apikey"], cfg.apiKey)
	cfg.baseURL = firstNonEmpty(values["base_url"], values["baseurl"], cfg.baseURL)
	cfg.planBase = firstNonEmpty(values["plan_base"], cfg.planBase)
	cfg.planPath = firstNonEmpty(values["plan_path"], cfg.planPath)
	cfg.model = firstNonEmpty(values["model"], cfg.model)
}

// applyConfigFile parses a fallback config file, supporting both JSON (such as
// ~/.vibe/cpa-mistral-vibe.json) and flat YAML.
func applyConfigFile(cfg *pluginConfig, raw []byte) {
	if cfg == nil || len(raw) == 0 {
		return
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var generic map[string]any
		if json.Unmarshal(trimmed, &generic) == nil {
			values := make(map[string]string, len(generic))
			for k, v := range generic {
				if s := str(v); s != "" {
					values[strings.ToLower(strings.TrimSpace(k))] = s
				}
			}
			applyConfigMap(cfg, values)
			return
		}
	}
	applyConfigYAML(cfg, raw)
}

var yamlLine = regexp.MustCompile(`^([ \t]*)([A-Za-z_][A-Za-z0-9_-]*)\s*:\s*(.*)$`)

// parseFlatYAML reads top-level scalars: `key: value`, with optional quotes and
// trailing comments. Nested blocks (e.g. `store:`) are skipped even when they
// contain keys matching top-level names.
func parseFlatYAML(text string) map[string]string {
	out := map[string]string{}
	nestedIndent := -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := leadingSpaces(line)
		if nestedIndent >= 0 {
			if indent > nestedIndent {
				continue
			}
			nestedIndent = -1
		}
		match := yamlLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		key := strings.ToLower(match[2])
		rawVal := strings.TrimSpace(match[3])
		if rawVal == "" || strings.HasPrefix(rawVal, "#") {
			// A key with no scalar value opens a nested block.
			nestedIndent = indent
			continue
		}
		value := parseYAMLScalar(rawVal)
		if value != "" && value != "{}" && value != "null" && value != "~" {
			out[key] = value
		}
	}
	return out
}

func leadingSpaces(line string) int {
	n := 0
	for _, r := range line {
		if r == ' ' {
			n++
		} else if r == '\t' {
			n += 2
		} else {
			break
		}
	}
	return n
}

// parseYAMLScalar parses a single-line YAML scalar value, respecting single and
// double quotes so `#` inside quotes is not mistaken for a trailing comment.
func parseYAMLScalar(raw string) string {
	if raw == "" {
		return ""
	}
	quote := raw[0]
	if quote == '"' || quote == '\'' {
		var b strings.Builder
		escaped := false
		for i := 1; i < len(raw); i++ {
			ch := raw[i]
			if quote == '"' && escaped {
				b.WriteByte(ch)
				escaped = false
				continue
			}
			if quote == '"' && ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				return b.String()
			}
			b.WriteByte(ch)
		}
		// Unterminated quote: strip the leading quote only.
		return strings.TrimSpace(raw[1:])
	}
	if index := strings.Index(raw, " #"); index >= 0 {
		raw = strings.TrimSpace(raw[:index])
	}
	if index := strings.Index(raw, "\t#"); index >= 0 {
		raw = strings.TrimSpace(raw[:index])
	}
	return strings.TrimSpace(raw)
}

func loadBaseConfig() pluginConfig {
	cfg := pluginConfig{
		apiKey:   envOr("VIBE_API_KEY", envOr("MISTRAL_API_KEY", "")),
		baseURL:  envOr("VIBE_API_BASE", ""),
		planBase: envOr("VIBE_PLAN_BASE", ""),
		planPath: envOr("VIBE_PLAN_PATH", ""),
		model:    envOr("VIBE_MODEL", ""),
	}
	if path := configPath(); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			applyConfigFile(&cfg, raw)
		}
	}
	return cfg
}

// currentConfig is the resolved configuration for this process.
func currentConfig() pluginConfig {
	configMu.RLock()
	if configCache.loaded {
		cfg := configCache.value
		configMu.RUnlock()
		return cfg
	}
	configMu.RUnlock()

	configMu.Lock()
	defer configMu.Unlock()
	if configCache.loaded {
		return configCache.value
	}
	cfg := loadBaseConfig()
	configCache.loaded = true
	configCache.value = cfg
	return cfg
}

// configure refreshes the cached config from a registration/reconfigure
// payload. Starting from the base (env + file) config ensures that removing or
// clearing a key in CPA's config.yaml takes effect on reconfigure.
func configure(raw []byte) {
	cfg := loadBaseConfig()
	applyConfigYAML(&cfg, raw)
	configMu.Lock()
	configCache.loaded = true
	configCache.value = cfg
	configMu.Unlock()
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

// baseUrl is the Mistral base the operator configured, or the default.
func (c pluginConfig) baseUrl() string {
	return firstNonEmpty(c.baseURL, defaultAPIBase)
}

// planBaseOrDefault is the console host the plan is read from.
func (c pluginConfig) planBaseOrDefault() string {
	return firstNonEmpty(c.planBase, defaultPlanBase)
}
