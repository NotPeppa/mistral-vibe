package main

import (
	"encoding/json"
	"strings"
	"time"
)

// Mistral's billing API is where a plan's month actually lives. magpie
// (github.com/yetone/magpie, internal/provider/mistral_plan.go) reads it the way
// Vibe's own CLI does, and this file follows the same contract:
//
//	GET api.mistral.ai/v1/billing/budget
//	GET api.mistral.ai/v1/billing/subscription
//	GET console.mistral.ai/api/vibe/whoami      (which budget the key spends)
//
// The budget belongs to the organization, so every key of one org is told the
// same numbers; which half a key spends is its kind, from whoami's plan_type:
// "chat" keys (Le Chat plans: Pro, Student, Team) spend vibe_budget, "api" keys
// (Studio keys) spend api_budget. Reading vibe_budget for an API key shows a
// month that key never spends — magpie hit exactly that bug (issue #5741).
const (
	billingBudgetPath       = "/v1/billing/budget"
	billingSubscriptionPath = "/v1/billing/subscription"
)

// planTitles are billing's plan names as Vibe titles them.
var planTitles = map[string]string{
	"FREE":       "Free",
	"INDIVIDUAL": "Pro",
	"EDU":        "Student",
	"TEAM":       "Team",
}

// billingBudget mirrors /v1/billing/budget.
type billingBudget struct {
	Vibe *billingDetail `json:"vibe_budget"`
	API  *billingDetail `json:"api_budget"`
}

type billingDetail struct {
	UsagePercentage float64 `json:"usage_percentage"`
	InitialBudget   float64 `json:"initial_budget"`
	Currency        string  `json:"currency"`
	ResetAt         string  `json:"reset_at"`
	PaygEnabled     *bool   `json:"payg_enabled"`
}

// billingSubscription mirrors /v1/billing/subscription.
type billingSubscription struct {
	PlanName        string `json:"plan_name"`
	PaygEnabled     *bool  `json:"payg_enabled"`
	VibePaygEnabled *bool  `json:"vibe_payg_enabled"`
}

// keyKind is whoami's plan_type, lower-cased.
func keyKind(raw map[string]any) string {
	return strings.ToLower(strings.TrimSpace(firstNonEmpty(
		str(raw["plan_type"]), str(raw["planType"]))))
}

// readBudget turns a billing answer into the plan's month for this key's kind.
// A budget billing does not name, or of zero, yields no window and no card.
func readBudget(raw []byte, kind string, payg *bool) (budgetWindow, bool, error) {
	var budget billingBudget
	if err := json.Unmarshal(raw, &budget); err != nil {
		return budgetWindow{}, false, err
	}
	detail := budget.Vibe
	name := "Month"
	switch kind {
	case "api":
		detail, name = budget.API, "API · Month"
	case "chat":
	default:
		// A key whose kind whoami doesn't tell has no card: the org budget
		// alone doesn't say which half the key spends.
		return budgetWindow{}, false, nil
	}
	if detail == nil || detail.InitialBudget <= 0 {
		return budgetWindow{}, false, nil
	}
	used := detail.UsagePercentage
	if used < 0 {
		used = 0
	}
	window := budgetWindow{
		Name:    name,
		UsedPct: min(100, used),
		Limit:   detail.InitialBudget,
		Spent:   detail.InitialBudget * used / 100,
		Unit:    strings.ToUpper(strings.TrimSpace(detail.Currency)),
		ResetAt: detail.ResetAt,
	}
	if detail.PaygEnabled != nil {
		payg = detail.PaygEnabled
	}
	window.Payg = payg != nil && *payg
	return window, true, nil
}

// budgetWindow is one plan month, as the quota card and the banner show it.
type budgetWindow struct {
	Name    string
	UsedPct float64
	Limit   float64
	Spent   float64
	Unit    string
	ResetAt string
	Payg    bool
}

// Remaining is what is left of the month's budget.
func (w budgetWindow) Remaining() float64 {
	left := w.Limit - w.Spent
	if left < 0 {
		return 0
	}
	return left
}

// resetTime parses the reset stamp, or returns the empty string.
func (w budgetWindow) resetTime() string {
	if _, err := time.Parse(time.RFC3339, w.ResetAt); err != nil {
		return ""
	}
	return w.ResetAt
}

// applyBudget makes the month's remaining budget the plan's balance.
func (p *planResponse) applyBudget(month budgetWindow) {
	if p == nil {
		return
	}
	p.Balance = month.Remaining()
	p.HasBalance = true
	p.Unit = month.Unit
	p.Payg = month.Payg
	p.Spent = month.Spent
	p.Limit = month.Limit
	p.ResetAt = month.ResetAt
	p.Windows = append(p.Windows, window{
		Group:     firstNonEmpty(p.Plan, providerID),
		Window:    month.Name,
		Remaining: clamp01(1 - month.UsedPct/100),
		Reset:     month.resetTime(),
	})
}
