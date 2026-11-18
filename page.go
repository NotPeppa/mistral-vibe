package main

import (
	"context"
	_ "embed"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

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
		"plan": "—", "planTone": "", "planFoot": "未配置 key",
		"tier":   "—",
		"remain": "—", "unit": "",
		"spent": "—", "limit": "—",
		"barPct": "0", "barTone": "", "paygText": "",
		"resetDate": "—", "resetCountdown": "",
		"windows": "", "rows": "", "rowCount": "0 条",
		"todayRequests": "0", "todayInput": "0", "todayOutput": "0", "todayTotal": "0",
		"allRequests": "0", "allTotal": "0", "modelRows": "", "usageEmpty": "",
		"note":    "",
		"mgmtURL": "/v0/management/" + pluginID + "/",
		"keyHint": "配置里还没有 key：在 <code>plugins.configs." + pluginID + "</code> 下填 <code>api_key</code> 后重启 CPA。",
	}
	if cfg.apiKey != "" {
		view["keyHint"] = "余额读自 <code>plugins.configs." + pluginID + ".api_key</code>，不需要在这里再填一次。"
	}

	renderUsage(view)
	if cfg.apiKey == "" {
		return fillPage(view)
	}

	cred := credential{PlanKey: cfg.apiKey, APIKey: cfg.apiKey, APIBase: cfg.baseUrl(), BaseURL: cfg.planBaseOrDefault()}
	plan, err := cred.readPlan(ctx)
	if err != nil {
		view["note"] = `<div class="notice bad"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="9"/><path d="M12 8v5"/><path d="M12 16h.01"/></svg><span>读取失败：` + html.EscapeString(err.Error()) + `</span></div>`
		return fillPage(view)
	}

	view["plan"] = html.EscapeString(firstNonEmpty(plan.Plan, "—"))
	view["tier"] = html.EscapeString(firstNonEmpty(plan.Tier, "—"))
	view["planFoot"] = "Mistral Vibe token plan"
	if plan.Expires != "" {
		view["planFoot"] = "有效期至 " + html.EscapeString(shortDate(plan.Expires))
	}
	if plan.Payg {
		view["paygText"] = " · 超量按量计费"
	}
	view["barTone"], view["planTone"] = gaugeTone(plan)

	if plan.HasBalance {
		view["remain"] = trimFloat(plan.Balance)
		view["unit"] = html.EscapeString(plan.Unit)
		view["spent"] = trimFloat(plan.Spent)
		if plan.Limit > 0 {
			view["limit"] = trimFloat(plan.Limit) + " " + html.EscapeString(plan.Unit)
			view["barPct"] = trimFloat(plan.Spent / plan.Limit * 100)
		}
	}
	if plan.ResetAt != "" {
		view["resetDate"] = html.EscapeString(shortDate(plan.ResetAt))
		view["resetCountdown"] = html.EscapeString(countdown(plan.ResetAt))
	}

	var windows []string
	for _, w := range plan.Windows {
		windows = append(windows, html.EscapeString(w.Window)+" 剩余 "+trimFloat(w.Remaining*100)+"%")
	}
	view["windows"] = strings.Join(windows, " · ")

	view["rows"] = renderRows(plan)
	view["rowCount"] = "1 个凭据"
	return fillPage(view)
}

// gaugeTone maps the used share to the card and pill tones.
func gaugeTone(plan *planResponse) (bar, pill string) {
	if plan == nil || plan.Limit <= 0 {
		return "", "ok"
	}
	used := plan.Spent / plan.Limit * 100
	switch {
	case used >= 90:
		return " bad", "bad"
	case used >= 75:
		return " warn", "warn"
	default:
		return "", "ok"
	}
}

// renderRows draws one table row per plan window, the shape the Kiro usage page
// uses: pool, share, used, remaining, reset.
func renderRows(plan *planResponse) string {
	if plan == nil || len(plan.Windows) == 0 {
		return `<div class="row"><span class="muted">没有上报额度窗口</span><span></span><span></span><span></span><span></span><span></span><span></span></div>`
	}
	var b strings.Builder
	for _, w := range plan.Windows {
		used := clamp01(1 - w.Remaining)
		tone := ""
		if used >= 0.9 {
			tone = " bad"
		} else if used >= 0.75 {
			tone = " warn"
		}
		fmt.Fprintf(&b, `<div class="row">
  <div class="name"><span class="dot"></span><span class="mono">%s</span></div>
  <div class="mono">%s</div>
  <div class="muted">%s</div>
  <div><div class="thin-bar%s" style="width:76px"><i style="width:%s%%"></i></div></div>
  <div class="num">%s%%</div>
  <div class="num">%s%%</div>
  <div class="muted">%s</div>
</div>`,
			html.EscapeString(firstNonEmpty(plan.Plan, pluginID)),
			html.EscapeString(firstNonEmpty(plan.Tier, "—")),
			html.EscapeString(firstNonEmpty(w.Window, "window")),
			tone, trimFloat(used*100),
			trimFloat(used*100), trimFloat(w.Remaining*100),
			html.EscapeString(firstNonEmpty(shortDate(w.Reset), "—")),
		)
	}
	return b.String()
}

// countdown says how long is left, or nothing when the stamp is unreadable.
func countdown(stamp string) string {
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	left := time.Until(t)
	if left <= 0 {
		return "已重置"
	}
	days := int(left.Hours()) / 24
	hours := int(left.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%d 天 %d 小时后", days, hours)
	}
	return fmt.Sprintf("%d 小时后", hours)
}

func fillPage(view map[string]string) []byte {
	out := string(pageHTML)
	for key, value := range view {
		out = strings.ReplaceAll(out, "{{"+key+"}}", value)
	}
	return []byte(out)
}

// shortDate keeps the date part of an RFC 3339 stamp.
func shortDate(stamp string) string {
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return stamp
}

// renderUsage fills the token-usage section from the ledger CPA feeds.
func renderUsage(view map[string]string) {
	usage := snapshotUsage()
	view["todayRequests"] = formatCount(float64(usage.Today.Requests))
	view["todayInput"] = formatCount(float64(usage.Today.Input))
	view["todayOutput"] = formatCount(float64(usage.Today.Output))
	view["todayTotal"] = formatCount(float64(usage.Today.Total))
	view["allRequests"] = formatCount(float64(usage.All.Requests))
	view["allTotal"] = formatCount(float64(usage.All.Total))
	if usage.All.Requests == 0 {
		view["usageEmpty"] = `<p class="sub" style="margin:0">还没有记录到请求。CPA 在每次调用完成后回调一次，这里会累积 token 数。</p>`
		view["modelRows"] = ""
		return
	}
	view["usageEmpty"] = ""
	var b strings.Builder
	for _, item := range usage.Models {
		share := 0.0
		if usage.All.Total > 0 {
			share = float64(item.Total) / float64(usage.All.Total) * 100
		}
		failPill := ""
		if item.Failed > 0 {
			failPill = fmt.Sprintf(`<span class="pill bad" style="margin-left:8px">%d 失败</span>`, item.Failed)
		}
		fmt.Fprintf(&b, `<div class="row">
  <div class="name"><span class="dot"></span><span class="mono">%s</span></div>
  <div class="num">%s</div>
  <div class="num">%s</div>
  <div class="num">%s</div>
  <div><div class="thin-bar" style="width:76px"><i style="width:%s%%"></i></div></div>
  <div class="num">%s%%%s</div>
  <div class="num muted">%s</div>
</div>`,
			html.EscapeString(item.Model),
			formatCount(float64(item.Requests)),
			formatCount(float64(item.Input)),
			formatCount(float64(item.Output)),
			trimFloat(share), trimFloat(share), failPill,
			formatCount(float64(item.Total)),
		)
	}
	view["modelRows"] = b.String()
}

// formatCount renders a token count the way the CLI does: 8.35k, 1.2M.
func formatCount(value float64) string {
	switch {
	case value >= 1e9:
		return trimFloat(value/1e9) + "B"
	case value >= 1e6:
		return trimFloat(value/1e6) + "M"
	case value >= 1e4:
		return trimFloat(value/1e3) + "k"
	}
	return trimFloat(value)
}
