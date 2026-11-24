package main

import (
	"encoding/json"
	"strings"
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
	month, ok, err := readBudget([]byte(realBudget), scopeVibe, nil)
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
	month, ok, err := readBudget([]byte(realBudget), scopeAPI, nil)
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	// The Studio half: 15 USD, not the org's 150 USD Vibe month.
	if month.Limit != 15 || month.Name != "API · Month" {
		t.Fatalf("month = %+v", month)
	}
}

func TestReadBudgetUnknownKindHasNoCard(t *testing.T) {
	if _, ok, err := readBudget([]byte(realBudget), scopeUnknown, nil); ok || err != nil {
		t.Fatalf("ok=%v err=%v: an unknown kind must not claim a budget", ok, err)
	}
}

func TestReadBudgetZeroBudgetHasNoCard(t *testing.T) {
	body := `{"vibe_budget":{"usage_percentage":10,"initial_budget":0,"currency":"USD"}}`
	if _, ok, err := readBudget([]byte(body), scopeVibe, nil); ok || err != nil {
		t.Fatalf("ok=%v err=%v: a zero budget has no card", ok, err)
	}
}

func TestReadBudgetHonoursPayg(t *testing.T) {
	on := true
	body := `{"vibe_budget":{"usage_percentage":120,"initial_budget":150,"currency":"USD","payg_enabled":true}}`
	month, ok, err := readBudget([]byte(body), scopeVibe, nil)
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
	plan.applyBudget(budgetSegment{Scope: "vibe", Billing: true, Window: budgetWindow{
		Name: "Month", UsedPct: 5.6684, Limit: 150, Spent: 8.5, Unit: "USD", ResetAt: "2026-12-01T00:00:00Z"}})
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

// The 2026-12-01 cycle: a harness request bills the API half, so the balance is
// the 15 USD API allowance, and the untouched Vibe allowance is still shown.
func TestHarnessTrafficBillsAPIBudgetAndShowsVibeBesideIt(t *testing.T) {
	segments, err := readBudgets([]byte(realBudget), scopeAPI, scopeVibe, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %+v", segments)
	}
	if !segments[0].Billing || segments[0].Window.Limit != 15 || segments[0].Name != "API allowance" {
		t.Fatalf("billing segment = %+v", segments[0])
	}
	if segments[1].Window.Limit != 150 || segments[1].Scope != "vibe" {
		t.Fatalf("vibe segment = %+v", segments[1])
	}

	plan := &planResponse{Plan: "Student", Tier: "CHAT"}
	plan.applyBudget(segments...)
	if plan.BilledScope != "api" || plan.Limit != 15 {
		t.Fatalf("plan = %+v", plan)
	}
	if len(plan.Windows) != 2 || plan.Windows[0].Window != "API · Month" || plan.Windows[1].Window != "Month" {
		t.Fatalf("windows = %+v", plan.Windows)
	}
}

func TestReadBudgetsDoesNotRepeatOneHalf(t *testing.T) {
	segments, err := readBudgets([]byte(realBudget), scopeAPI, scopeAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("segments = %+v: one half must be one card", segments)
	}
}

func TestResolveScope(t *testing.T) {
	cases := []struct {
		name                  string
		override, split, kind string
		outsideAPI            bool
		want                  billingScope
	}{
		{"config override wins", "vibe", "api", "chat", true, scopeVibe},
		{"mistral's split beats the kind", "", "api", "chat", false, scopeAPI},
		{"mistral's split says vibe", "", "vibe", "api", true, scopeVibe},
		{"chat key through a harness bills api", "", "", "CHAT", true, scopeAPI},
		{"chat key pre-rollout bills vibe", "", "", "CHAT", false, scopeVibe},
		{"api key always bills api", "", "", "API", false, scopeAPI},
		{"unknown kind has no card", "", "", "", true, scopeUnknown},
	}
	for _, c := range cases {
		if got := resolveScope(c.override, c.split, c.kind, c.outsideAPI); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The point of impersonating the CLI: when Mistral answers that the key is
// scoped to CLI usage, a chat key through this plugin reports the Vibe month
// again — the 150 USD half — and the banner says why.
func TestCLIScopingPutsTheVibeAllowanceBack(t *testing.T) {
	scope := resolveScope("", "cli", "chat", true)
	if scope != scopeVibe {
		t.Fatalf("scope = %q, a cli scoping answer must spend the Vibe allowance", scope)
	}
	segments, err := readBudgets([]byte(realBudget), scope, scopeVibe, nil)
	if err != nil {
		t.Fatal(err)
	}
	plan := &planResponse{Plan: "Student", Tier: "CHAT"}
	plan.applyBudget(segments...)
	if plan.BilledScope != "vibe" || plan.Limit != 150 {
		t.Fatalf("plan = %+v", plan)
	}
	if plan.Balance < 141 || plan.Balance > 142 {
		t.Fatalf("balance = %v, want the Vibe month's remainder", plan.Balance)
	}
	banner := renderCLI(plan, "mistral-vibe-cli-latest")
	if !strings.Contains(banner, "billing Vibe allowance (Mistral products)") {
		t.Fatalf("banner:\n%s", banner)
	}
}

func TestScopeEvidenceNeverOverclaims(t *testing.T) {
	chatPlan := func(scope billingScope) *planResponse {
		plan := &planResponse{Plan: "Student", Tier: "CHAT"}
		segments, _ := readBudgets([]byte(realBudget), scope, scopeVibe, nil)
		plan.applyBudget(segments...)
		return plan
	}
	setAmbient(ambientNote{Mode: "vibe_cli", Scope: "vibe"})
	if got := ambientSummary(chatPlan(scopeVibe)); !strings.Contains(got, "确认走 Vibe 额度") {
		t.Fatalf("vibe case = %q", got)
	}
	setAmbient(ambientNote{Mode: "vibe_cli", Scope: "api"})
	got := ambientSummary(chatPlan(scopeAPI))
	if !strings.Contains(got, "仍按 API 额度") {
		t.Fatalf("api case = %q: the page must not claim a success the billing read denies", got)
	}
	setAmbient(ambientNote{})
}
