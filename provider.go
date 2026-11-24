package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

// fallbackModels is what the entry publishes when the upstream model list cannot
// be read. The real list comes from GET /v1/models with the plan's own key —
// Mistral serves dozens of models and they change without notice, so a
// hardcoded list would hide most of them.
var fallbackModels = []string{modelVibe, modelVibeTools, modelVibeFast}

const (
	modelVibe      = "mistral-vibe-cli-latest"
	modelVibeTools = "mistral-vibe-cli-with-tools"
	modelVibeFast  = "mistral-vibe-cli-fast"
)

// managementBaseFor prefers a validated base named by the request, then the
// environment, then CPA's discovered loopback listener.
func managementBaseFor(requested string) (string, error) {
	if base := strings.TrimSpace(requested); base != "" {
		clean := strings.TrimRight(base, "/")
		if !isAllowedManagementBase(clean) {
			return "", fmt.Errorf("management_base must be a loopback URL (or match VIBE_MANAGEMENT_BASE)")
		}
		return clean, nil
	}
	if base := strings.TrimSpace(os.Getenv("VIBE_MANAGEMENT_BASE")); base != "" {
		return strings.TrimRight(base, "/"), nil
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
	return "http://127.0.0.1:" + port, nil
}

// isAllowedManagementBase prevents forwarding CPA's management key to arbitrary
// remote hosts supplied by a request.
func isAllowedManagementBase(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	if isLoopbackOrigin(parsed.Hostname()) {
		return true
	}
	if envBase := strings.TrimSpace(os.Getenv("VIBE_MANAGEMENT_BASE")); envBase != "" {
		if envParsed, err := url.Parse(envBase); err == nil && strings.EqualFold(envParsed.Host, parsed.Host) {
			return true
		}
	}
	return false
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
	var candidates []string
	if cfgEnv := strings.TrimSpace(os.Getenv("CLI_PROXY_CONFIG")); cfgEnv != "" {
		candidates = append(candidates, cfgEnv)
	}
	if home := homeDir(); home != "" {
		candidates = append(candidates, filepath.Join(home, ".cli-proxy-api", "config.yaml"))
	}
	candidates = append(candidates, "config.yaml")
	for _, candidate := range candidates {
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
	portLine   = regexp.MustCompile(`(?m)^port:\s*["']?(\d{2,5})["']?(?:\s+#.*)?\s*$`)
	portNested = regexp.MustCompile(`(?m)^[ \t]+port:\s*["']?(\d{2,5})["']?(?:\s+#.*)?\s*$`)
)

// portFromYAML reads the top-level port, else the first nested one.
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

	modelIDs := listModels(ctx, mistralBase, key)
	if len(modelIDs) == 0 {
		modelIDs = fallbackModels
	}
	models := make([]map[string]string, 0, len(modelIDs))
	for _, id := range modelIDs {
		models = append(models, map[string]string{"name": id, "alias": id})
	}
	value := map[string]any{
		"name":            providerName,
		"base-url":        mistralBase + "/v1",
		"api-key-entries": []map[string]string{{"api-key": key}},
		"models":          models,
	}
	// The Vibe CLI presentation travels with the entry, because that is what
	// asks Mistral to book this traffic as Vibe CLI usage rather than as an
	// outside tool's.
	headers := currentConfig().providerHeaders()
	if err := checkHeaders(headers); err != nil {
		return connectResult{}, err
	}
	if len(headers) > 0 {
		value["headers"] = headers
	}

	if index := findProvider(entries, mistralBase); index >= 0 {
		// Preserve operator-configured fields (priority, prefix, headers, disabled)
		// from the existing/adopted entry while updating the managed fields.
		merged := map[string]any{}
		for k, v := range entries[index] {
			merged[k] = v
		}
		for k, v := range value {
			merged[k] = v
		}
		if err := patchCompatProviderAt(ctx, base, managementKey, index, merged); err != nil {
			return connectResult{}, err
		}
		return connectResult{Created: false, Updated: true, Name: providerName, BaseURL: mistralBase + "/v1", Models: modelIDs, Index: index}, nil
	}
	index, err := appendCompatProviderAt(ctx, base, managementKey, append(entries, value))
	if err != nil {
		return connectResult{}, err
	}
	return connectResult{Created: true, Updated: false, Name: providerName, BaseURL: mistralBase + "/v1", Models: modelIDs, Index: index}, nil
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

// appendCompatProviderAt writes the whole list, which is how a new entry is added.
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

func managementRequestAt(ctx context.Context, base, managementKey, method, path string, body []byte) ([]byte, error) {
	if managementKey == "" {
		return nil, fmt.Errorf("management key is required to change CPA's provider list")
	}
	targetBase, err := managementBaseFor(base)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, targetBase+path, bytes.NewReader(body))
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

// publishToConfigFile writes the Mistral Vibe entry into CPA's config.yaml.
//
// The plugin never sees CPA's management key, so it cannot use the management
// API. But the provider list lives in the same config file CPA watches, and a
// plugin runs inside that process with its working directory and auth dir, so
// the entry can be merged in directly; CPA reloads the file on change.
//
// The write is conservative: it preserves every other `openai-compatibility`
// provider and every other line, writes a one-time backup before its first edit,
// replaces atomically via a temp file, and does nothing when the entry is
// already up to date.
func publishToConfigFile(ctx context.Context, cred credential) {
	path := findConfigFile()
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}
	original, err := os.ReadFile(path)
	if err != nil || len(original) == 0 {
		return
	}
	key := firstNonEmpty(cred.APIKey, cred.PlanKey)
	modelCtx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()
	headers := currentConfig().providerHeaders()
	if err := checkHeaders(headers); err != nil {
		return
	}
	updated, changed := mergeCompatEntry(string(original), cred.apiBase(), key, listModels(modelCtx, cred.apiBase(), key), headers)
	if !changed {
		return
	}
	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0o600
	}
	bakPath := path + ".cpa-mistral-vibe.bak"
	if _, err := os.Stat(bakPath); os.IsNotExist(err) {
		if err := os.WriteFile(bakPath, original, perm); err != nil {
			return
		}
	}
	tmpPath := path + ".cpa-mistral-vibe.tmp"
	if err := os.WriteFile(tmpPath, []byte(updated), perm); err != nil {
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// findConfigFile looks where CPA keeps its config: next to the auth directory,
// then the process working directory, then the usual home location.
func findConfigFile() string {
	var candidates []string
	if authDir := strings.TrimSpace(os.Getenv("CLIPROXY_AUTH_DIR")); authDir != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(authDir), "config.yaml"))
	}
	if wd, err := os.Getwd(); err == nil {
		candidates = append(candidates, filepath.Join(wd, "config.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".cli-proxy-api", "config.yaml"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// compatEntryYAML renders a single list item under `openai-compatibility:`.
func compatEntryYAML(base, key string, modelIDs []string, headers map[string]string) string {
	cleanBase := normalizeAPIBase(base)
	var b strings.Builder
	fmt.Fprintf(&b, "  - name: %q\n", providerName)
	fmt.Fprintf(&b, "    base-url: %q\n", cleanBase+"/v1")
	b.WriteString("    api-key-entries:\n")
	fmt.Fprintf(&b, "      - api-key: %q\n", key)
	if len(headers) > 0 {
		b.WriteString("    headers:\n")
		for _, name := range sortedHeaderNames(headers) {
			fmt.Fprintf(&b, "      %s: %q\n", name, headers[name])
		}
	}
	b.WriteString("    models:\n")
	for _, id := range modelIDs {
		fmt.Fprintf(&b, "      - name: %q\n        alias: %q\n", id, id)
	}
	return b.String()
}

// sortedHeaderNames keeps the rendered entry stable, so a republish that
// changes nothing also writes nothing.
func sortedHeaderNames(headers map[string]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// compatBlock renders the full YAML block for this plugin's provider entry.
func compatBlock(base, key string, modelIDs []string, headers map[string]string) string {
	return "openai-compatibility:\n" + compatEntryYAML(base, key, modelIDs, headers)
}

var (
	yamlItemStart  = regexp.MustCompile(`^([ \t]*)-\s+`)
	yamlEntryName  = regexp.MustCompile(`(?i)^\s*(?:-\s*)?name:\s*(.+)$`)
	yamlEntryBase  = regexp.MustCompile(`(?i)^\s*(?:-\s*)?base-url:\s*(.+)$`)
	compatKeyStart = regexp.MustCompile(`^openai-compatibility\s*:(.*)$`)
)

// mergeCompatEntry returns the config with this plugin's provider entry present
// and carrying the given key, preserving every other `openai-compatibility`
// provider entry and every other top-level block in the file.
func mergeCompatEntry(config, base, key string, modelIDs []string, headers map[string]string) (string, bool) {
	if len(modelIDs) == 0 {
		modelIDs = fallbackModels
	}
	cleanBase := normalizeAPIBase(base)
	wantEntry := strings.TrimRight(compatEntryYAML(cleanBase, key, modelIDs, headers), "\n")
	wantEntryLines := strings.Split(wantEntry, "\n")

	lines := strings.Split(config, "\n")
	start := -1
	inlineRest := ""
	for i, line := range lines {
		if m := compatKeyStart.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			start = i
			inlineRest = strings.TrimSpace(m[1])
			break
		}
	}
	if start < 0 {
		trimmed := strings.TrimRight(config, "\n")
		if trimmed == "" {
			return compatBlock(cleanBase, key, modelIDs, headers), true
		}
		return trimmed + "\n" + compatBlock(cleanBase, key, modelIDs, headers), true
	}

	// The block runs until the next top-level key.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "#") {
			continue
		}
		end = i
		break
	}

	bodyLines := lines[start+1 : end]
	// Separate trailing blank lines that visually pad the next top-level key or EOF.
	trailingBlanks := 0
	for len(bodyLines) > trailingBlanks && strings.TrimSpace(bodyLines[len(bodyLines)-1-trailingBlanks]) == "" {
		trailingBlanks++
	}
	suffixBlankLines := bodyLines[len(bodyLines)-trailingBlanks:]
	contentLines := bodyLines[:len(bodyLines)-trailingBlanks]

	// Parse existing list items inside openai-compatibility: (unless it was inline `[]`/`null`).
	var prefixLines []string
	var items [][]string
	if inlineRest == "" || strings.HasPrefix(inlineRest, "#") {
		itemIndent := -1
		var current []string
		for _, line := range contentLines {
			raw := strings.TrimRight(line, "\r")
			if m := yamlItemStart.FindStringSubmatch(raw); m != nil {
				indent := leadingSpaces(m[1])
				if itemIndent < 0 {
					itemIndent = indent
				}
				if indent == itemIndent {
					if current != nil {
						items = append(items, current)
					}
					current = []string{line}
					continue
				}
			}
			if current == nil {
				prefixLines = append(prefixLines, line)
			} else {
				current = append(current, line)
			}
		}
		if current != nil {
			items = append(items, current)
		}
	}

	// Find an existing Mistral Vibe item (by name, else by host) and replace only it.
	targetHost := hostOf(cleanBase)
	matchIdx := -1
	for i, item := range items {
		if itemMatchesName(item, providerName) {
			matchIdx = i
			break
		}
	}
	if matchIdx < 0 && targetHost != "" {
		for i, item := range items {
			if itemMatchesHost(item, targetHost) {
				matchIdx = i
				break
			}
		}
	}

	if matchIdx >= 0 {
		items[matchIdx] = wantEntryLines
	} else {
		items = append(items, wantEntryLines)
	}

	outLines := make([]string, 0, len(lines)+len(wantEntryLines))
	outLines = append(outLines, lines[:start]...)
	outLines = append(outLines, "openai-compatibility:")
	outLines = append(outLines, prefixLines...)
	for _, item := range items {
		outLines = append(outLines, item...)
	}
	outLines = append(outLines, suffixBlankLines...)
	outLines = append(outLines, lines[end:]...)

	rebuilt := strings.Join(outLines, "\n")
	if !strings.HasSuffix(rebuilt, "\n") {
		rebuilt += "\n"
	}
	if rebuilt == config {
		return config, false
	}
	return rebuilt, true
}

func itemMatchesName(item []string, want string) bool {
	for _, line := range item {
		if m := yamlEntryName.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			if strings.EqualFold(parseYAMLScalar(strings.TrimSpace(m[1])), want) {
				return true
			}
		}
	}
	return false
}

func itemMatchesHost(item []string, wantHost string) bool {
	for _, line := range item {
		if m := yamlEntryBase.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			if hostOf(parseYAMLScalar(strings.TrimSpace(m[1]))) == wantHost {
				return true
			}
		}
	}
	return false
}

// listModels reads the model ids the key can use, from GET {base}/v1/models.
//
// Mistral serves far more than the three Vibe ids — chat, code, embed, OCR,
// audio, moderation — and the set changes without notice, so the provider entry
// publishes what the account actually sees instead of a fixed list. An empty
// result means the read failed and the caller falls back.
func listModels(ctx context.Context, mistralBase, key string) []string {
	body, err := managementGetAt(ctx, normalizeAPIBase(mistralBase)+"/v1/models", key)
	if err != nil {
		return nil
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	ids := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// managementGetAt fetches a URL with the given bearer token.
func managementGetAt(ctx context.Context, targetURL, key string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+stripBearerPrefix(key))
	request.Header.Set("Accept", "application/json")
	response, err := apiClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", targetURL, response.StatusCode)
	}
	return body, nil
}
