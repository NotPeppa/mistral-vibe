package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Plan B: the plugin does not proxy traffic. It writes the plan's key into CPA's
// own AI-provider list as an `openai-compatibility` entry, and CPA then serves
// Mistral natively — tools, streaming, images and all, with no translation.
//
// This file owns that write and the reads that make it idempotent: a second
// connect updates the entry it created instead of adding another.

// providerName is the entry label the CPA panel shows.
const providerName = "Mistral Vibe"

// compatModels are the model ids Mistral serves for a token plan; the entry
// publishes them so clients can pick one.
var compatModels = []string{modelVibe, modelVibeTools, modelVibeFast}

const (
	modelVibe      = "mistral-vibe-cli-latest"
	modelVibeTools = "mistral-vibe-cli-with-tools"
	modelVibeFast  = "mistral-vibe-cli-fast"
)

// managementBase is CPA's own management API. The plugin runs inside CPA, so it
// talks to the loopback listener; VIBE_MANAGEMENT_BASE overrides it.
func managementBase() string {
	return managementBaseFor("")
}

// managementBaseFor prefers a base named by the request, then the environment.
func managementBaseFor(requested string) string {
	if base := strings.TrimSpace(requested); base != "" {
		return strings.TrimRight(base, "/")
	}
	if base := strings.TrimSpace(os.Getenv("VIBE_MANAGEMENT_BASE")); base != "" {
		return strings.TrimRight(base, "/")
	}
	port := strings.TrimSpace(os.Getenv("VIBE_MANAGEMENT_PORT"))
	if port == "" {
		// CPA may listen anywhere; discover its own listener rather than guess
		// 8317 and fail on every deployment that moved.
		port = discoverManagementPort()
	}
	if port == "" {
		port = "8317"
	}
	return "http://127.0.0.1:" + port
}

// discoverManagementPort finds the port CPA is listening on. The plugin runs
// inside CPA, so its own process has the listener; the auth dir's config.yaml
// names the port when it is not the default.
func discoverManagementPort() string {
	for _, key := range []string{"CLIPROXY_PORT", "CPA_PORT"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	for _, candidate := range []string{
		filepath.Join(os.Getenv("CLI_PROXY_CONFIG"), ""),
		filepath.Join(homeDir(), ".cli-proxy-api", "config.yaml"),
		"config.yaml",
	} {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if port := portFromYAML(string(raw)); port != "" {
			return port
		}
	}
	return ""
}

var (
	portLine   = regexp.MustCompile(`(?m)^\s*port:\s*["']?(\d{2,5})["']?\s*$`)
	portNested = regexp.MustCompile(`(?m)^\s{2,}port:\s*["']?(\d{2,5})["']?\s*$`)
)

// portFromYAML reads the top-level port, else the first server-level one.
func portFromYAML(text string) string {
	if match := portLine.FindStringSubmatch(text); len(match) == 2 {
		return match[1]
	}
	if match := portNested.FindStringSubmatch(text); len(match) == 2 {
		return match[1]
	}
	return ""
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

// compatEntry is the subset of an openai-compatibility entry this plugin owns.
type compatEntry struct {
	Name          string `json:"name"`
	BaseURL       string `json:"base-url"`
	APIKeyEntries []struct {
		APIKey string `json:"api-key"`
	} `json:"api-key-entries"`
	Models []struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	} `json:"models"`
}

// connectResult is what POST /mistral-vibe/connect answers.
type connectResult struct {
	Created bool     `json:"created"`
	Updated bool     `json:"updated"`
	Name    string   `json:"name"`
	BaseURL string   `json:"base_url"`
	Models  []string `json:"models"`
	Index   int      `json:"index"`
}

// connectProvider upserts the Mistral Vibe entry in CPA's provider list.
func connectProvider(ctx context.Context, managementKey string, cred credential) (connectResult, error) {
	return connectProviderAt(ctx, "", managementKey, cred)
}

// connectProviderAt is connectProvider for a management API at a known base.
func connectProviderAt(ctx context.Context, base, managementKey string, cred credential) (connectResult, error) {
	key := firstNonEmpty(cred.APIKey, cred.PlanKey)
	if key == "" {
		return connectResult{}, fmt.Errorf("no Mistral key to publish")
	}
	mistralBase := cred.apiBase()
	entries, err := listCompatProvidersAt(ctx, base, managementKey)
	if err != nil {
		return connectResult{}, err
	}

	models := make([]map[string]string, 0, len(compatModels))
	for _, id := range compatModels {
		models = append(models, map[string]string{"name": id, "alias": id})
	}
	value := map[string]any{
		"name":            providerName,
		"base-url":        mistralBase + "/v1",
		"api-key-entries": []map[string]string{{"api-key": key}},
		"models":          models,
	}

	if index := findProvider(entries, mistralBase); index >= 0 {
		if err := patchCompatProviderAt(ctx, base, managementKey, index, value); err != nil {
			return connectResult{}, err
		}
		return connectResult{Created: false, Updated: true, Name: providerName, BaseURL: mistralBase + "/v1", Models: compatModels, Index: index}, nil
	}
	index, err := appendCompatProviderAt(ctx, base, managementKey, append(entries, value))
	if err != nil {
		return connectResult{}, err
	}
	return connectResult{Created: true, Updated: false, Name: providerName, BaseURL: mistralBase + "/v1", Models: compatModels, Index: index}, nil
}

// findProvider picks the entry this plugin owns: the one named like it, else one
// already pointing at the same Mistral host (so a hand-added entry is adopted
// rather than duplicated).
func findProvider(entries []map[string]any, base string) int {
	for index, entry := range entries {
		if name, _ := entry["name"].(string); strings.EqualFold(strings.TrimSpace(name), providerName) {
			return index
		}
	}
	host := hostOf(base)
	for index, entry := range entries {
		if candidate, _ := entry["base-url"].(string); host != "" && hostOf(candidate) == host {
			return index
		}
	}
	return -1
}

var hostPattern = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://([^/]+)`)

// hostOf strips the scheme and path from a base URL.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if match := hostPattern.FindStringSubmatch(raw); len(match) == 2 {
		return strings.ToLower(match[1])
	}
	return ""
}

// --------------------------------------------------------------------------- management calls

// listCompatProviders reads the provider list CPA keeps, in the shape the PATCH
// and PUT endpoints accept back.
func listCompatProviders(ctx context.Context, managementKey string) ([]map[string]any, error) {
	return listCompatProvidersAt(ctx, "", managementKey)
}

func listCompatProvidersAt(ctx context.Context, base, managementKey string) ([]map[string]any, error) {
	body, err := managementRequestAt(ctx, base, managementKey, http.MethodGet, "/v0/management/openai-compatibility", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Items []map[string]any `json:"openai-compatibility"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode provider list: %w", err)
	}
	out := make([]map[string]any, 0, len(envelope.Items))
	for _, item := range envelope.Items {
		out = append(out, sanitizeEntry(item))
	}
	return out, nil
}

// sanitizeEntry keeps only what the write endpoints understand; the read
// endpoint adds auth indexes and derived fields that would be rejected back.
func sanitizeEntry(entry map[string]any) map[string]any {
	clean := map[string]any{}
	for _, field := range []string{"name", "priority", "prefix", "disabled", "base-url", "api-key-entries", "models", "headers"} {
		if value, ok := entry[field]; ok {
			clean[field] = value
		}
	}
	return clean
}

func patchCompatProviderAt(ctx context.Context, base, managementKey string, index int, value map[string]any) error {
	payload, err := json.Marshal(map[string]any{"index": index, "value": value})
	if err != nil {
		return err
	}
	_, err = managementRequestAt(ctx, base, managementKey, http.MethodPatch, "/v0/management/openai-compatibility", payload)
	return err
}

// appendCompatProvider writes the whole list, which is how a new entry is added.
func appendCompatProviderAt(ctx context.Context, base, managementKey string, entries []map[string]any) (int, error) {
	payload, err := json.Marshal(entries)
	if err != nil {
		return -1, err
	}
	if _, err := managementRequestAt(ctx, base, managementKey, http.MethodPut, "/v0/management/openai-compatibility", payload); err != nil {
		return -1, err
	}
	return len(entries) - 1, nil
}

func managementGet(ctx context.Context, managementKey, path string) ([]byte, error) {
	return managementRequest(ctx, managementKey, http.MethodGet, path, nil)
}

func managementRequest(ctx context.Context, managementKey, method, path string, body []byte) ([]byte, error) {
	return managementRequestAt(ctx, "", managementKey, method, path, body)
}

func managementRequestAt(ctx context.Context, base, managementKey, method, path string, body []byte) ([]byte, error) {
	if managementKey == "" {
		return nil, fmt.Errorf("management key is required to change CPA's provider list")
	}
	request, err := http.NewRequestWithContext(ctx, method, managementBaseFor(base)+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+managementKey)
	request.Header.Set("X-Management-Key", managementKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := apiClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if response.StatusCode >= 400 {
		return nil, fmt.Errorf("management %s %s: %d %s", method, path, response.StatusCode, truncateText(strings.TrimSpace(string(raw)), 200))
	}
	return raw, nil
}

func truncateText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
