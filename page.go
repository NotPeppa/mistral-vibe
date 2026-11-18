package main

import (
	"context"
	_ "embed"
	"html"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

//go:embed page.html
var pageHTML []byte

// pageHandler serves the plugin's own page.
//
// Resource routes carry no management authentication (CPA hands
// /v0/resource/plugins/<id>/* straight to the plugin), so the page cannot call
// the management routes and must not ask the operator for a management key just
// to read a balance. The balance is therefore read here, on the host side, and
// rendered into the HTML; the key stays in config.yaml and never reaches the
// browser.
type pageHandler struct{}

func (pageHandler) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead:
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       renderPage(ctx),
		}, nil
	}
	return pluginapi.ManagementResponse{StatusCode: http.StatusMethodNotAllowed}, nil
}

// renderPage fills the page's placeholders with the balance of the configured
// key. A read failure is shown on the page rather than failing the request, so
// the operator still sees why the numbers are missing.
func renderPage(ctx context.Context) []byte {
	cfg := currentConfig()
	view := map[string]string{
		"plan":    "—",
		"remain":  "—",
		"spent":   "—",
		"reset":   "—",
		"windows": "",
		"barPct":  "0",
		"barTone": "",
		"note":    "",
		"mgmtURL": "/v0/management/" + pluginID + "/",
		"keyHint": "配置里还没有 key：在 plugins.configs." + pluginID + " 下填 api_key 后重启。",
	}
	if cfg.apiKey != "" {
		view["keyHint"] = "余额读自 <code>plugins.configs." + pluginID + ".api_key</code>"
	}

	plan, err := credential{
		PlanKey: cfg.apiKey,
		APIKey:  cfg.apiKey,
		APIBase: cfg.baseUrl(),
		BaseURL: cfg.planBaseOrDefault(),
	}.readPlan(ctx)
	switch {
	case cfg.apiKey == "":
		view["note"] = ""
	case err != nil:
		view["note"] = `<div class="stat bad">读取失败：` + html.EscapeString(err.Error()) + `</div>`
	default:
		view["plan"] = html.EscapeString(firstNonEmpty(plan.Plan, "—"))
		if plan.HasBalance {
			view["remain"] = html.EscapeString(formatMoney(plan.Balance, plan.Unit))
			spent := formatMoney(plan.Spent, plan.Unit)
			if plan.Limit > 0 {
				spent += " / " + formatMoney(plan.Limit, plan.Unit)
			}
			view["spent"] = html.EscapeString(spent)
			if plan.Limit > 0 {
				pct := plan.Spent / plan.Limit * 100
				view["barPct"] = trimFloat(pct)
				switch {
				case pct >= 90:
					view["barTone"] = " bad"
				case pct >= 75:
					view["barTone"] = " warn"
				}
			}
		}
		if plan.ResetAt != "" {
			view["reset"] = html.EscapeString(shortDate(plan.ResetAt))
		}
		var parts []string
		for _, w := range plan.Windows {
			parts = append(parts, html.EscapeString(w.Window)+" · 剩余 "+trimFloat(w.Remaining*100)+"%")
		}
		view["windows"] = strings.Join(parts, "　")
	}

	out := string(pageHTML)
	for key, value := range view {
		out = strings.ReplaceAll(out, "{{"+key+"}}", value)
	}
	return []byte(out)
}

// formatMoney keeps two decimals and appends the unit.
func formatMoney(value float64, unit string) string {
	text := trimFloat(value)
	if unit != "" {
		text += " " + unit
	}
	return text
}

// shortDate keeps the date part of an RFC 3339 stamp.
func shortDate(stamp string) string {
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}
