package main

import (
	"strings"
	"sync"
)

// A harness can ask Mistral to account its traffic the way the Vibe CLI does,
// and that is the whole point of this file: from the 2026-12-01 cycle, whatever
// arrives from a non-Mistral product is billed to the plan's general API
// allowance instead of the dedicated Vibe allowance. The rule is decided by
// how the request presents itself (client name, user agent, the CLI's scoping
// switch), not by which process opened the socket.
//
// So the plugin writes the Vibe CLI's own presentation onto the provider entry
// it publishes into CPA: the CLI's name and user agent, and the CLI-impersonation
// switch alone. Then CPA's forwarding of this entry looks like Vibe CLI traffic,
// which is the half the plan's Vibe allowance pays for.
//
// Deliberately not touched: the CLI's internal cookie/session path, the
// console's plan endpoints and the account's own organization id. Those are
// account plumbing, not request presentation — they identify a session rather
// than a client, and shipping them would break the moment the CLI rotates them.
const (
	// ambientClient mimics the Vibe CLI's own client label.
	ambientClient = "vibe-cli"
	// ambientUserAgent is the CLI's user agent for inference calls.
	ambientUserAgent = "mistral-vibe-cli/0.1"

	// scopeHeader is the CLI's own switch: "cli" asks Mistral to treat the
	// traffic as Vibe CLI usage. billingSubscription.ScopingCLI reads the same
	// field back off /v1/billing/subscription.
	scopeHeader = "X-Cli-Scoping"
	scopeCLI    = "cli"
)

// defaultAmbientHeaders is what the published provider entry carries when the
// operator leaves the header list empty.
func defaultAmbientHeaders() map[string]string {
	return map[string]string{
		"User-Agent":       ambientUserAgent,
		"X-Mistral-Client": ambientClient,
		scopeHeader:        scopeCLI,
		"Accept":           "application/json",
		"Accept-Encoding":  "identity",
	}
}

// ambientHeaders resolves the headers the provider entry should carry.
//
// vibe (default): the Vibe CLI's presentation above, keeping any scope header
// the operator named themselves.
// passthrough:   nothing at all — the request reaches Mistral with CPA's own
// user agent, which is what an account that must not look like the CLI wants.
func ambientHeaders(mode string, extra map[string]string) map[string]string {
	out := map[string]string{}
	if strings.EqualFold(strings.TrimSpace(mode), "passthrough") {
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for k, v := range defaultAmbientHeaders() {
		out[k] = v
	}
	for k, v := range extra {
		if v == "" {
			// An empty value removes a default header — matched by name so the
			// operator's spelling ("accept-encoding:") beats the default.
			for have := range out {
				if strings.EqualFold(have, k) {
					delete(out, have)
				}
			}
			continue
		}
		out[canonicalHeaderKey(out, k)] = v
	}
	return out
}

// canonicalHeaderKey keeps one spelling per header regardless of case, so
// "user-agent" does not end up beside "User-Agent".
func canonicalHeaderKey(existing map[string]string, key string) string {
	for have := range existing {
		if strings.EqualFold(have, key) {
			return have
		}
	}
	return key
}

// checkHeaders validates an "Header: value" list from config.yaml. A malformed
// header is reported instead of silently dropping the impersonation.
func checkHeaders(headers map[string]string) error {
	for key := range headers {
		if key == "" || strings.ContainsAny(key, " :\r\n") {
			return &headerError{key}
		}
	}
	return nil
}

type headerError struct{ key string }

func (e *headerError) Error() string {
	return "invalid header name in headers: " + quoteOrEmpty(e.key)
}

// quoteOrEmpty keeps the error readable for a name that is empty or has spaces.
func quoteOrEmpty(s string) string {
	if s == "" {
		return `""`
	}
	return `"` + s + `"`
}

// ambientMu guards the last word Mistral gave about how it books this entry.
var ambientMu sync.RWMutex

// ambientNote is what the page and the CLI banner say about the cookie the
// plugin is sending. It is a claim about local behaviour, not a promise about
// billing: only Mistral decides which allowance pays, and the scope read back
// from billing is the evidence.
type ambientNote struct {
	Mode     string
	Scope    string
	Evidence string
}

var lastAmbient ambientNote

func setAmbient(note ambientNote) {
	ambientMu.Lock()
	lastAmbient = note
	ambientMu.Unlock()
}

func ambientState() ambientNote {
	ambientMu.RLock()
	defer ambientMu.RUnlock()
	return lastAmbient
}
