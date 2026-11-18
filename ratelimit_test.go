package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestRateLimitsFromRealHeaders(t *testing.T) {
	header := http.Header{}
	header.Set("x-ratelimit-limit-tokens-minute", "1000000")
	header.Set("x-ratelimit-remaining-tokens-minute", "999962")
	header.Set("x-ratelimit-tokens-query-cost", "19")
	header.Set("x-ratelimit-limit-req-minute", "2000")
	header.Set("x-ratelimit-remaining-req-minute", "1999")

	limits := rateLimitsFrom(header)
	if limits == nil {
		t.Fatal("expected limits")
	}
	if limits.LimitTokens != 1000000 || limits.RemainingTokens != 999962 || limits.QueryCost != 19 {
		t.Fatalf("tokens: %+v", limits)
	}
	if limits.RemainingReqs != 1999 {
		t.Fatalf("reqs: %+v", limits)
	}
}

func TestRateLimitsFromEmptyHeadersIsNil(t *testing.T) {
	if limits := rateLimitsFrom(http.Header{}); limits != nil {
		t.Fatalf("expected nil, got %+v", limits)
	}
}

func TestAttachRateLimitsBecomesBalance(t *testing.T) {
	plan := &planResponse{Plan: "EDU"}
	plan.attachRateLimits(&rateLimits{
		LimitTokens: 1000000, RemainingTokens: 999962, HasTokens: true,
		LimitRequests: 2000, RemainingReqs: 1999, HasRequests: true,
	})
	if !plan.HasBalance || plan.Balance != 999962 || plan.Unit != "tokens/min" {
		t.Fatalf("balance: %+v", plan)
	}
	if len(plan.Windows) != 2 {
		t.Fatalf("windows: %+v", plan.Windows)
	}
	quota := plan.toQuota()
	if len(quota.Summary) != 1 || quota.Summary[0].Value != 999962 {
		t.Fatalf("quota summary: %+v", quota.Summary)
	}
	if quota.Summary[0].Format == "currency" {
		t.Fatalf("token counts must not be marked as currency")
	}
}

func TestAttachRateLimitsZeroLimitDoesNotDivide(t *testing.T) {
	plan := &planResponse{Plan: "EDU"}
	plan.attachRateLimits(&rateLimits{LimitTokens: 0, RemainingTokens: 5, HasTokens: true})
	if plan.HasBalance {
		t.Fatalf("a zero limit must not produce a balance: %+v", plan)
	}
}

func TestRenderLabelsWindowsAndSubPercent(t *testing.T) {
	plan := &planResponse{Plan: "EDU", Tier: "CHAT", Balance: 499962, HasBalance: true, Unit: "tokens/min"}
	plan.attachRateLimits(&rateLimits{
		LimitTokens: 500000, RemainingTokens: 499962, HasTokens: true,
		LimitRequests: 1000, RemainingReqs: 999, HasRequests: true,
	})
	out := renderCLI(plan, "mistral-vibe-cli-latest")
	for _, want := range []string{"plan    EDU", "tier    CHAT", "balance 499962 tokens/min", "tokens/minute", "requests/minute", "0.1% used"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestLocalOriginsBypassTheProxy(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "192.168.1.10", "10.0.0.1", "172.16.0.1", "myhost.local"} {
		if !isLocalOrigin(host) {
			t.Errorf("%s should be treated as local", host)
		}
	}
	for _, host := range []string{"api.mistral.ai", "console.mistral.ai", "8.8.8.8"} {
		if isLocalOrigin(host) {
			t.Errorf("%s should go through the proxy", host)
		}
	}
}
