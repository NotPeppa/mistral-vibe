package main

import (
	"net/http"
	"strconv"
	"strings"
)

// rateLimits is the live allowance Mistral returns on every inference answer:
//
//	x-ratelimit-limit-tokens-minute:     1000000
//	x-ratelimit-remaining-tokens-minute: 999962
//	x-ratelimit-tokens-query-cost:       19
//	x-ratelimit-limit-req-minute:        2000
//	x-ratelimit-remaining-req-minute:    1999
//
// It is the only real number a personal plan key can see — plan endpoints are
// 401 and billing endpoints are 404 — so the plugin surfaces it as the balance.
type rateLimits struct {
	LimitTokens     float64
	RemainingTokens float64
	LimitRequests   float64
	RemainingReqs   float64
	QueryCost       float64
	HasTokens       bool
	HasRequests     bool
}

func rateLimitsFrom(header http.Header) *rateLimits {
	if header == nil {
		return nil
	}
	read := func(name string) (float64, bool) {
		raw := strings.TrimSpace(header.Get(name))
		if raw == "" {
			return 0, false
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	out := &rateLimits{}
	out.LimitTokens, out.HasTokens = read("x-ratelimit-limit-tokens-minute")
	if remaining, ok := read("x-ratelimit-remaining-tokens-minute"); ok {
		out.RemainingTokens, out.HasTokens = remaining, true
	}
	out.LimitRequests, out.HasRequests = read("x-ratelimit-limit-req-minute")
	if remaining, ok := read("x-ratelimit-remaining-req-minute"); ok {
		out.RemainingReqs, out.HasRequests = remaining, true
	}
	out.QueryCost, _ = read("x-ratelimit-tokens-query-cost")
	if !out.HasTokens && !out.HasRequests {
		return nil
	}
	return out
}

// attachRateLimits turns the headers into a balance metric and a window, so the
// quota card and the CLI banner show a real remaining number.
func (p *planResponse) attachRateLimits(limits *rateLimits) {
	if p == nil || limits == nil {
		return
	}
	if limits.HasTokens && limits.LimitTokens > 0 {
		fraction := clamp01(limits.RemainingTokens / limits.LimitTokens)
		p.Balance = limits.RemainingTokens
		p.HasBalance = true
		p.Unit = "tokens/min"
		p.Windows = append(p.Windows, window{
			Group:     firstNonEmpty(p.Plan, providerID),
			Window:    "tokens/minute",
			Remaining: fraction,
		})
	}
	if limits.HasRequests && limits.LimitRequests > 0 {
		p.Windows = append(p.Windows, window{
			Group:     firstNonEmpty(p.Plan, providerID),
			Window:    "requests/minute",
			Remaining: clamp01(limits.RemainingReqs / limits.LimitRequests),
		})
	}
}
