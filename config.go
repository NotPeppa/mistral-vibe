package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pluginConfig mirrors the ConfigFields the descriptor publishes, loaded from
// CPA's plugin config file or the matching environment variable.
type pluginConfig struct {
	apiBase  string
	planBase string
	planPath string
	timeout  time.Duration
}

var configCache struct {
	loaded bool
	value  pluginConfig
}

// vibeConfig resolves one setting: file first, then the environment.
func vibeConfig(fileKey, envKey string) string {
	cfg := loadConfig()
	switch fileKey {
	case "api_base":
		return cfg.apiBase
	case "plan_base":
		return cfg.planBase
	case "plan_path":
		return cfg.planPath
	}
	if value := os.Getenv(envKey); value != "" {
		return value
	}
	return ""
}

func loadConfig() pluginConfig {
	if configCache.loaded {
		return configCache.value
	}
	cfg := pluginConfig{
		apiBase:  envOr("VIBE_API_BASE", ""),
		planBase: envOr("VIBE_PLAN_BASE", ""),
		planPath: envOr("VIBE_PLAN_PATH", ""),
		timeout:  httpTimeout,
	}
	// CPA hands plugins their config as plugins.configs.<pluginID>; the file
	// path is discoverable through the same env the host exports.
	if path := firstNonEmpty(os.Getenv("CLIPROXY_PLUGIN_CONFIG"), configPath()); path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			var file struct {
				APIBase  string `json:"api_base"`
				PlanBase string `json:"plan_base"`
				PlanPath string `json:"plan_path"`
			}
			if json.Unmarshal(raw, &file) == nil {
				cfg.apiBase = firstNonEmpty(file.APIBase, cfg.apiBase)
				cfg.planBase = firstNonEmpty(file.PlanBase, cfg.planBase)
				cfg.planPath = firstNonEmpty(file.PlanPath, cfg.planPath)
			}
		}
	}
	configCache.loaded = true
	configCache.value = cfg
	return cfg
}

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	candidates := []string{
		filepath.Join(home, ".cli-proxy-api", "plugins.configs", pluginID+".json"),
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
