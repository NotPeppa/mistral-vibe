package main

import (
	"encoding/json"
	"testing"
)

func jsonUnmarshal(raw []byte, target any) error { return json.Unmarshal(raw, target) }

// Captured live from api.mistral.ai/v1/billing/budget (Student plan).
const realBudget = `{
  "vibe_budget": {"usage_percentage":5.668426738666667,"initial_budget":150.0,
                  "currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},
  "api_budget":  {"usage_percentage":0.001918,"initial_budget":15.0,
                  "currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},
  "usage_percentage":0.001918,"initial_budget":15.0,"currency":"USD",
  "reset_at":"2026-12-01T00:00:00Z"}`

func TestReadBudgetVibeKeySpendsVibeBudget(t *testing.T) {
	month, ok, err := readBudget([]byte(realBudget), "chat", nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if month.Limit != 150 || month.Unit != "USD" {
		t.Fatalf("month = %+v", month)
	}
	if month.Spent < 8.4 || month.Spent > 8.6 {
		t.Fatalf("spent = %v, want ~8.5", month.Spent)
	}
	if left := month.Remaining(); left < 141 || left > 142 {
		t.Fatalf("remaining = %v", left)
	}
	if month.ResetAt != "2026-12-01T00:00:00Z" {
		t.Fatalf("reset = %q", month.ResetAt)
	}
}

func TestReadBudgetAPIKeySpendsAPIBudget(t *testing.T) {
	month, ok, err := readBudget([]byte(realBudget), "api", nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// The Studio half: 15 USD, not the org's 150 USD Vibe month.
	if month.Limit != 15 || month.Name != "API · Month" {
		t.Fatalf("month = %+v", month)
	}
}

func TestReadBudgetUnknownKindHasNoCard(t *testing.T) {
	if _, ok, err := readBudget([]byte(realBudget), "", nil); ok || err != nil {
		t.Fatalf("ok=%v err=%v: an unknown kind must not claim a budget", ok, err)
	}
}

func TestReadBudgetZeroBudgetHasNoCard(t *testing.T) {
	body := `{"vibe_budget":{"usage_percentage":10,"initial_budget":0,"currency":"USD"}}`
	if _, ok, err := readBudget([]byte(body), "chat", nil); ok || err != nil {
		t.Fatalf("ok=%v err=%v: a zero budget has no card", ok, err)
	}
}

func TestReadBudgetHonoursPayg(t *testing.T) {
	on := true
	body := `{"vibe_budget":{"usage_percentage":120,"initial_budget":150,"currency":"USD","payg_enabled":true}}`
	month, ok, err := readBudget([]byte(body), "chat", nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if !month.Payg {
		t.Fatal("payg must come from the budget block")
	}
	if month.UsedPct != 100 {
		t.Fatalf("used must clamp to 100 for display: %v", month.UsedPct)
	}
	_ = on
}

func TestApplyBudgetBecomesTheBalance(t *testing.T) {
	plan := &planResponse{Plan: "Student", Tier: "CHAT"}
	plan.applyBudget(budgetWindow{Name: "Month", UsedPct: 5.6684, Limit: 150, Spent: 8.5, Unit: "USD", ResetAt: "2026-12-01T00:00:00Z"})
	if !plan.HasBalance || plan.Unit != "USD" {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Balance < 141 || plan.Balance > 142 {
		t.Fatalf("balance = %v", plan.Balance)
	}
	quota := plan.toQuota()
	if len(quota.Summary) != 1 || quota.Summary[0].Currency != "USD" || quota.Summary[0].Format != "currency" {
		t.Fatalf("summary = %+v", quota.Summary)
	}
	if len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].ResetTime != "2026-12-01T00:00:00Z" {
		t.Fatalf("groups = %+v", quota.Groups)
	}
}

func TestKeyKindFromWhoami(t *testing.T) {
	for raw, want := range map[string]string{
		`{"plan_type":"CHAT"}`: "chat",
		`{"plan_type":"API"}`:  "api",
		`{"planType":"chat"}`:  "chat",
		`{}`:                   "",
	} {
		var payload map[string]any
		_ = unmarshalForTest(raw, &payload)
		if got := keyKind(payload); got != want {
			t.Errorf("keyKind(%s) = %q, want %q", raw, got, want)
		}
	}
}

func unmarshalForTest(raw string, target any) error {
	return jsonUnmarshal([]byte(raw), target)
}
