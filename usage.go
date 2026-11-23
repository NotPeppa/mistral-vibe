package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Token accounting.
//
// CPA calls UsagePlugin.HandleUsage once per completed request with the token
// counters the upstream reported. The plugin keeps a rolling ledger on disk and
// renders it beside the plan's money balance, so the page answers "how many
// tokens have I used" as well as "how much of the month is left".
//
// The ledger is deliberately small: totals, a per-model split, and the last few
// hundred requests. Anything larger belongs in a database, not in a plugin.

const (
	// ledgerFileName is written next to the plugin's other state.
	ledgerFileName = "usage.json"
	// ledgerRecentCap bounds the retained recent requests.
	ledgerRecentCap = 200
	// usageRecentShown is how many of them the snapshot exposes.
	usageRecentShown = 20
	// ledgerFlushEvery writes the file at most this often, so a burst of requests
	// does not rewrite it on every call.
	ledgerFlushEvery = 3 * time.Second
)

// usageTotals is one bucket of counters.
type usageTotals struct {
	Requests    int64 `json:"requests"`
	Failed      int64 `json:"failed"`
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	Reasoning   int64 `json:"reasoning"`
	Cached      int64 `json:"cached"`
	CacheRead   int64 `json:"cache_read"`
	CacheCreate int64 `json:"cache_create"`
	Total       int64 `json:"total"`
}

func effectiveTotal(d pluginapi.UsageDetail) int64 {
	if d.TotalTokens != 0 {
		return d.TotalTokens
	}
	return d.InputTokens + d.OutputTokens + d.ReasoningTokens
}

// add folds one record in.
func (t *usageTotals) add(record pluginapi.UsageRecord) {
	t.Requests++
	if record.Failed {
		t.Failed++
	}
	t.Input += record.Detail.InputTokens
	t.Output += record.Detail.OutputTokens
	t.Reasoning += record.Detail.ReasoningTokens
	t.Cached += record.Detail.CachedTokens
	t.CacheRead += record.Detail.CacheReadTokens
	t.CacheCreate += record.Detail.CacheCreationTokens
	t.Total += effectiveTotal(record.Detail)
}

// usageEntry is one retained request.
type usageEntry struct {
	At     string `json:"at"`
	Model  string `json:"model"`
	Input  int64  `json:"input"`
	Output int64  `json:"output"`
	Cached int64  `json:"cached"`
	Total  int64  `json:"total"`
	Failed bool   `json:"failed,omitempty"`
}

// usageLedger is the whole persisted state.
type usageLedger struct {
	All     usageTotals             `json:"all"`
	Today   usageTotals             `json:"today"`
	Day     string                  `json:"day"`
	Models  map[string]*usageTotals `json:"models"`
	Recent  []usageEntry            `json:"recent"`
	Updated string                  `json:"updated,omitempty"`
}

var (
	ledgerMu    sync.Mutex
	ledger      *usageLedger
	ledgerDirty bool
	ledgerSaved time.Time
)

// ledgerPath is next to the plugin's other state: the config file's directory,
// else the working directory.
func ledgerPath() string {
	if dir := os.Getenv("VIBE_PLUGIN_STATE_DIR"); dir != "" {
		return filepath.Join(dir, ledgerFileName)
	}
	if path := configPath(); path != "" {
		return filepath.Join(filepath.Dir(path), ledgerFileName)
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, ledgerFileName)
	}
	return ledgerFileName
}

func currentLedger() *usageLedger {
	if ledger != nil {
		return ledger
	}
	ledger = &usageLedger{Models: map[string]*usageTotals{}}
	if raw, err := os.ReadFile(ledgerPath()); err == nil {
		var loaded usageLedger
		if json.Unmarshal(raw, &loaded) == nil {
			if loaded.Models == nil {
				loaded.Models = map[string]*usageTotals{}
			}
			ledger = &loaded
		}
	}
	rollDay(ledger, time.Now())
	return ledger
}

// rollDay resets the daily bucket when the date changed.
func rollDay(l *usageLedger, now time.Time) bool {
	day := now.Format("2006-01-02")
	if l.Day == day {
		return false
	}
	l.Day = day
	l.Today = usageTotals{}
	return true
}

// isMistralUsage filters CPA's global per-request callbacks so non-Mistral
// traffic (Claude, Gemini, OpenAI, etc.) does not pollute the Mistral Vibe
// token ledger.
func isMistralUsage(record pluginapi.UsageRecord) bool {
	prov := strings.ToLower(strings.TrimSpace(record.Provider))
	authType := strings.ToLower(strings.TrimSpace(record.AuthType))
	base := strings.TrimSpace(record.BaseURL)
	// Records without routing metadata (synthetic/unit test inputs) pass through.
	if prov == "" && base == "" && authType == "" {
		return true
	}
	if strings.Contains(prov, "mistral") || strings.Contains(authType, "mistral") {
		return true
	}
	if base != "" {
		host := hostOf(base)
		if host != "" {
			if host == hostOf(defaultAPIBase) || strings.HasSuffix(host, ".mistral.ai") || host == "mistral.ai" {
				return true
			}
			if cfgHost := hostOf(currentConfig().baseUrl()); cfgHost != "" && host == cfgHost {
				return true
			}
			return false
		}
	}
	if prov == "openai-compatibility" || authType == "openai-compatibility" {
		model := strings.ToLower(firstNonEmpty(record.ResponseModel, record.Model, record.Alias))
		for _, prefix := range []string{"mistral", "codestral", "devstral", "ministral", "pixtral", "voxtral", "magistral", "open-mistral", "open-mixtral"} {
			if strings.Contains(model, prefix) {
				return true
			}
		}
		return false
	}
	return false
}

// recordUsage folds one CPA usage record into the ledger.
func recordUsage(record pluginapi.UsageRecord) {
	model := firstNonEmpty(record.ResponseModel, record.Model, record.Alias)
	if model == "" && !record.Failed && effectiveTotal(record.Detail) == 0 &&
		record.Detail.InputTokens == 0 && record.Detail.OutputTokens == 0 && record.Detail.ReasoningTokens == 0 {
		return
	}
	if !isMistralUsage(record) {
		return
	}
	ledgerMu.Lock()
	defer ledgerMu.Unlock()

	l := currentLedger()
	now := time.Now()
	rollDay(l, now)

	l.All.add(record)
	l.Today.add(record)
	if model != "" {
		bucket := l.Models[model]
		if bucket == nil {
			bucket = &usageTotals{}
			l.Models[model] = bucket
		}
		bucket.add(record)
	}

	entry := usageEntry{
		At:     now.Format(time.RFC3339),
		Model:  model,
		Input:  record.Detail.InputTokens,
		Output: record.Detail.OutputTokens,
		Cached: record.Detail.CachedTokens,
		Total:  effectiveTotal(record.Detail),
		Failed: record.Failed,
	}
	if len(l.Recent) < ledgerRecentCap {
		l.Recent = append(l.Recent, usageEntry{})
	}
	copy(l.Recent[1:], l.Recent)
	l.Recent[0] = entry
	l.Updated = now.Format(time.RFC3339)

	ledgerDirty = true
	if now.Sub(ledgerSaved) >= ledgerFlushEvery {
		flushLedgerLocked()
	}
}

// flushLedger writes the ledger if it changed. Called periodically on record
// updates and on quiesce/shutdown; the write is atomic through a temp file.
func flushLedger() {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	flushLedgerLocked()
}

func flushLedgerLocked() {
	if !ledgerDirty || ledger == nil {
		return
	}
	raw, err := json.Marshal(ledger)
	if err != nil {
		return
	}
	path := ledgerPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	ledgerDirty = false
	ledgerSaved = time.Now()
}

// usageSnapshot is a copy the page can render without holding the lock.
type usageSnapshot struct {
	All     usageTotals
	Today   usageTotals
	Models  []modelUsage
	Recent  []usageEntry
	Updated string
	Day     string
}

type modelUsage struct {
	Model string
	usageTotals
}

func snapshotUsage() usageSnapshot {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	l := currentLedger()
	if rollDay(l, time.Now()) {
		ledgerDirty = true
	}

	out := usageSnapshot{All: l.All, Today: l.Today, Updated: l.Updated, Day: l.Day}
	for model, totals := range l.Models {
		out.Models = append(out.Models, modelUsage{Model: model, usageTotals: *totals})
	}
	sort.Slice(out.Models, func(i, j int) bool {
		if out.Models[i].Total != out.Models[j].Total {
			return out.Models[i].Total > out.Models[j].Total
		}
		return out.Models[i].Model < out.Models[j].Model
	})
	head := l.Recent
	if len(head) > usageRecentShown {
		head = head[:usageRecentShown]
	}
	out.Recent = append([]usageEntry(nil), head...)
	return out
}

// --------------------------------------------------------------------------- the capability

// HandleUsage is CPA's per-request callback. It must never block the host: the
// ledger update is a few counters under a mutex.
func (agent) HandleUsage(_ context.Context, record pluginapi.UsageRecord) {
	recordUsage(record)
}

// resetUsage clears the ledger, for POST /mistral-vibe/usage/reset.
func resetUsage() {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	ledger = &usageLedger{Models: map[string]*usageTotals{}, Day: time.Now().Format("2006-01-02")}
	ledgerDirty = true
	flushLedgerLocked()
}
