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
// same numbers; which half a key spends is its kind, from whoami's plan_type.
//
// Who spends which half (announced to take effect on the 2026-12-01 cycle):
//
//	chat keys (Le Chat plans: Pro, Student, Team)
//	  inside Mistral products (Vibe CLI, Le Chat)  -> vibe_budget
//	  through outside tools and harnesses (CPA, Claude Code, ...) -> api_budget
//	api keys (console-created Studio keys)         -> api_budget
//
// So a key reaching Mistral through this plugin no longer spends the dedicated
// Vibe allowance; it spends the general API allowance. Reading vibe_budget for
// a request that bills api_budget shows a month that request never spends —
// magpie hit exactly that bug (issue #5741) by ignoring the key kind, and the
// SCOPING_CLI rollout adds a second axis on top.
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
	// ScopingCLI carries X-Cli-Scoping / scope_cli from the Vibe CLI flip
	// rollout. When Mistral answers a usage split it wins: an "api" split is a
	// definite api_budget answer even for a chat key.
	ScopingCLI string `json:"scoping_cli,omitempty"`
}

// budgetSegment is one half of the organization's month: the Vibe allowance or
// the general API allowance. Both are rendered; only the billing one is the
// balance, so a change of routing on Mistral's side cannot hide the allowance
// the key is spending.
type budgetSegment struct {
	Name    string
	Scope   string
	Window  budgetWindow
	Billing bool
}

// keyKind is whoami's plan_type, lower-cased.
func keyKind(raw map[string]any) string {
	return strings.ToLower(strings.TrimSpace(firstNonEmpty(
		str(raw["plan_type"]), str(raw["planType"]))))
}

// billingScope is the half of the organization's month a request spends.
type billingScope string

const (
	scopeVibe    billingScope = "vibe" // the plan's dedicated Vibe allowance
	scopeAPI     billingScope = "api"  // the plan's general API allowance
	scopeUnknown billingScope = ""
)

// parseScope reads a scope off a config value, a header or a whoami/split
// answer. An empty or unrecognized value is scopeUnknown.
func parseScope(raw string) billingScope {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	// "cli" is the switch the Vibe CLI itself sends (X-Cli-Scoping: cli) and the
	// answer /v1/billing/subscription echoes back: traffic booked as CLI usage,
	// which is the dedicated Vibe allowance.
	case "vibe", "chat", "plan", "cli", "vibe_cli", "vibe-cli", "vibe_budget", "vibe-budget":
		return scopeVibe
	case "api", "studio", "api_budget", "api-budget", "general":
		return scopeAPI
	}
	return scopeUnknown
}

// resolveScope decides which half this plugin's traffic spends.
//
// whoami's plan_type alone answers what a key spends inside Mistral products,
// but not what it spends through a third-party harness: the SCOPING_CLI rollout
// moves every outside request onto the API allowance from the 2026-12-01 cycle.
// The order is therefore configuration, then Mistral's own split, then the kind
// with the outside-tool default:
//
//	scope = "vibe" | "api"      operator override, for a contract that differs
//	split (X-Cli-Scoping)       Mistral answered the question itself
//	api key                     always the API allowance
//	chat key                    API allowance (outside tool) or vibe when that
//	                            default is turned off
func resolveScope(override, split, kind string, outsideAPI bool) billingScope {
	if scope := parseScope(override); scope != scopeUnknown {
		return scope
	}
	if scope := parseScope(split); scope != scopeUnknown {
		return scope
	}
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "api":
		return scopeAPI
	case "chat":
		if outsideAPI {
			return scopeAPI
		}
		return scopeVibe
	}
	// A key whose kind whoami doesn't tell has no card: the org budget alone
	// doesn't say which half the key spends.
	return scopeUnknown
}

// readBudget turns a billing answer into the plan's month for the scope the
// request bills. A budget billing does not name, or of zero, yields no window.
func readBudget(raw []byte, scope billingScope, payg *bool) (budgetWindow, bool, error) {
	if scope == scopeUnknown {
		return budgetWindow{}, false, nil
	}
	segments, err := readBudgets(raw, scope, scopeUnknown, payg)
	if err != nil || len(segments) == 0 {
		return budgetWindow{}, false, err
	}
	return segments[0].Window, true, nil
}

// readBudgets reads the billing month for the billing scope, and optionally the
// month of a second scope whose card is shown beside it (scopeAPI doubling as
// "no second card"). The billing scope comes first.
func readBudgets(raw []byte, scope, beside billingScope, payg *bool) ([]budgetSegment, error) {
	var budget billingBudget
	if err := json.Unmarshal(raw, &budget); err != nil {
		return nil, err
	}
	var out []budgetSegment
	for _, s := range []billingScope{scope, beside} {
		if s == scopeUnknown {
			continue
		}
		if len(out) > 0 && billingScope(out[0].Scope) == s {
			continue
		}
		detail, name, shown := budgetScope(s, budget)
		if detail == nil || detail.InitialBudget <= 0 {
			continue
		}
		used := detail.UsagePercentage
		if used < 0 {
			used = 0
		}
		local := payg
		if detail.PaygEnabled != nil {
			local = detail.PaygEnabled
		}
		out = append(out, budgetSegment{
			Name:    shown,
			Scope:   string(s),
			Billing: s == scope,
			Window: budgetWindow{
				Name:    name,
				UsedPct: min(100, used),
				Limit:   detail.InitialBudget,
				Spent:   detail.InitialBudget * used / 100,
				Unit:    strings.ToUpper(strings.TrimSpace(detail.Currency)),
				ResetAt: detail.ResetAt,
				Payg:    local != nil && *local,
			},
		})
	}
	return out, nil
}

// budgetScope maps a half to its billing detail, the window name the CLI shows
// and the scope label the page shows.
func budgetScope(s billingScope, budget billingBudget) (*billingDetail, string, string) {
	switch s {
	case scopeAPI:
		return budget.API, "API · Month", "API allowance"
	case scopeVibe:
		return budget.Vibe, "Month", "Vibe allowance"
	}
	return nil, "", ""
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

// applyBudget makes the billing half's remaining budget the plan's balance.
// Every segment gets its own window, so a Vibe allowance the key no longer
// spends through this plugin still shows up beside the API allowance it does.
func (p *planResponse) applyBudget(segments ...budgetSegment) {
	if p == nil || len(segments) == 0 {
		return
	}
	for _, seg := range segments {
		month := seg.Window
		if seg.Billing {
			p.Balance = month.Remaining()
			p.HasBalance = true
			p.Unit = month.Unit
			p.Payg = month.Payg
			p.Spent = month.Spent
			p.Limit = month.Limit
			p.ResetAt = month.ResetAt
			p.BilledScope = seg.Scope
		}
		p.Windows = append(p.Windows, window{
			Group:     firstNonEmpty(p.Plan, providerID),
			Window:    month.Name,
			Remaining: clamp01(1 - month.UsedPct/100),
			Reset:     month.resetTime(),
			Scope:     seg.Scope,
		})
	}
}
