package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func withLedgerDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VIBE_PLUGIN_STATE_DIR", dir)
	ledgerMu.Lock()
	ledger = nil
	ledgerMu.Unlock()
	t.Cleanup(func() {
		ledgerMu.Lock()
		ledger = nil
		ledgerMu.Unlock()
	})
	return dir
}

func record(model string, in, out, cached int64, failed bool) pluginapi.UsageRecord {
	return pluginapi.UsageRecord{
		Model:  model,
		Failed: failed,
		Detail: pluginapi.UsageDetail{
			InputTokens:  in,
			OutputTokens: out,
			CachedTokens: cached,
			TotalTokens:  in + out,
		},
	}
}

func TestUsageCountsEveryRecord(t *testing.T) {
	withLedgerDir(t)
	recordUsage(record("mistral-vibe-cli-latest", 100, 20, 40, false))
	recordUsage(record("mistral-vibe-cli-latest", 50, 5, 0, false))
	recordUsage(record("codestral-latest", 10, 2, 0, true))

	snap := snapshotUsage()
	if snap.All.Requests != 3 || snap.All.Failed != 1 {
		t.Fatalf("all = %+v", snap.All)
	}
	if snap.All.Input != 160 || snap.All.Output != 27 {
		t.Fatalf("tokens = %+v", snap.All)
	}
	if snap.All.Total != 187 {
		t.Fatalf("total = %d", snap.All.Total)
	}
	// The per-model split keeps the busiest first.
	if len(snap.Models) != 2 || snap.Models[0].Model != "mistral-vibe-cli-latest" {
		t.Fatalf("models = %+v", snap.Models)
	}
	if snap.Models[0].Requests != 2 || snap.Models[0].Total != 175 {
		t.Fatalf("first model = %+v", snap.Models[0])
	}
}

func TestUsagePersistsAcrossRestart(t *testing.T) {
	dir := withLedgerDir(t)
	recordUsage(record("mistral-vibe-cli-latest", 10, 5, 0, false))
	flushLedger()

	if _, err := os.Stat(filepath.Join(dir, ledgerFileName)); err != nil {
		t.Fatalf("ledger not written: %v", err)
	}

	// Simulate a restart: drop the in-memory ledger.
	ledgerMu.Lock()
	ledger = nil
	ledgerMu.Unlock()

	snap := snapshotUsage()
	if snap.All.Requests != 1 || snap.All.Total != 15 {
		t.Fatalf("after reload = %+v", snap.All)
	}
}

func TestUsageResetClearsTheLedger(t *testing.T) {
	withLedgerDir(t)
	recordUsage(record("m", 10, 5, 0, false))
	resetUsage()
	if snap := snapshotUsage(); snap.All.Requests != 0 || len(snap.Models) != 0 {
		t.Fatalf("after reset = %+v", snap)
	}
}

func TestUsageKeepsRecentCapped(t *testing.T) {
	withLedgerDir(t)
	for i := 0; i < ledgerRecentCap+50; i++ {
		recordUsage(record("m", 1, 1, 0, false))
	}
	ledgerMu.Lock()
	len5 := len(currentLedger().Recent)
	ledgerMu.Unlock()
	if len5 != ledgerRecentCap {
		t.Fatalf("recent len = %d, want %d", len5, ledgerRecentCap)
	}
	// The snapshot only exposes the head of the list.
	if snap := snapshotUsage(); len(snap.Recent) > 20 {
		t.Fatalf("snapshot recent = %d", len(snap.Recent))
	}
}

func TestUsageIgnoresEmptyRecords(t *testing.T) {
	withLedgerDir(t)
	recordUsage(pluginapi.UsageRecord{})
	if snap := snapshotUsage(); snap.All.Requests != 0 {
		t.Fatalf("empty record counted: %+v", snap.All)
	}
}

func TestUsageTracksCachedAndReasoning(t *testing.T) {
	withLedgerDir(t)
	recordUsage(pluginapi.UsageRecord{
		Model: "mistral-vibe-cli-latest",
		Detail: pluginapi.UsageDetail{
			InputTokens: 1000, OutputTokens: 200,
			CachedTokens: 800, CacheReadTokens: 800, CacheCreationTokens: 50,
			ReasoningTokens: 120, TotalTokens: 1200,
		},
	})
	snap := snapshotUsage()
	if snap.All.Cached != 800 || snap.All.CacheRead != 800 || snap.All.CacheCreate != 50 {
		t.Fatalf("cache counters = %+v", snap.All)
	}
	if snap.All.Reasoning != 120 {
		t.Fatalf("reasoning = %d", snap.All.Reasoning)
	}
	if got := cacheRate(snap.All); got != "80% 命中" {
		t.Fatalf("cacheRate = %q", got)
	}
}

func TestCacheRateSaysNothingWhenThereIsNone(t *testing.T) {
	if got := cacheRate(usageTotals{Input: 100}); got != "本次无缓存命中" {
		t.Fatalf("got %q", got)
	}
	if got := cacheRate(usageTotals{}); got != "—" {
		t.Fatalf("got %q", got)
	}
}
