package main

import (
	"encoding/base64"
	"testing"
)

func TestParseFlatYAMLReadsTheConfigBlock(t *testing.T) {
	// What CPA sends for plugins.configs.cpa-mistral-vibe.
	block := `enabled: true
priority: 0
api_key: mstrl_unit_test_key
base_url: https://api.mistral.ai
model: mistral-vibe-cli-latest
store:
  version: 0.1.0
`
	values := parseFlatYAML(block)
	if values["api_key"] != "mstrl_unit_test_key" {
		t.Fatalf("api_key = %q", values["api_key"])
	}
	if values["base_url"] != "https://api.mistral.ai" {
		t.Fatalf("base_url = %q", values["base_url"])
	}
	if values["model"] != "mistral-vibe-cli-latest" {
		t.Fatalf("model = %q", values["model"])
	}
	if values["enabled"] != "true" || values["priority"] != "0" {
		t.Fatalf("scalars = %v", values)
	}
}

func TestParseFlatYAMLHandlesQuotesAndComments(t *testing.T) {
	block := `  api_key: "mstrl_quoted"   # the key
  base_url: 'https://example.test'
`
	values := parseFlatYAML(block)
	if values["api_key"] != "mstrl_quoted" {
		t.Fatalf("api_key = %q", values["api_key"])
	}
	if values["base_url"] != "https://example.test" {
		t.Fatalf("base_url = %q", values["base_url"])
	}
}

func TestApplyConfigYAMLOverrides(t *testing.T) {
	cfg := pluginConfig{apiKey: "from-env", model: "old"}
	applyConfigYAML(&cfg, []byte("api_key: from-yaml\nmodel: new-model\n"))
	if cfg.apiKey != "from-yaml" || cfg.model != "new-model" {
		t.Fatalf("cfg = %+v", cfg)
	}
	// An empty block changes nothing.
	applyConfigYAML(&cfg, nil)
	if cfg.apiKey != "from-yaml" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestRegistrationConfigDecodesBase64(t *testing.T) {
	// ConfigYAML is []byte on the wire, so CPA sends it base64-encoded.
	yaml := "enabled: true\napi_key: mstrl_from_cpa\n"
	body := []byte(`{"config_yaml":"` + base64.StdEncoding.EncodeToString([]byte(yaml)) + `","schema_version":10}`)
	if got := string(registrationConfig(body)); got != yaml {
		t.Fatalf("got %q, want %q", got, yaml)
	}
	if got := registrationConfig([]byte(`{}`)); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}
