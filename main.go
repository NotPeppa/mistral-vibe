// Package main implements the mistral-vibe provider for CLIProxyAPI (CPA).
//
// Native CPA plugin: a cgo shared library exporting cliproxy_plugin_init,
// speaking CPA's JSON-RPC envelope over the native ABI. Build:
//
//	go build -buildmode=c-shared -o cpa-mistral-vibe.dylib .
//
// Contract source of truth: sdk/pluginabi (ABI + method names, schema 10) and
// sdk/pluginapi (payload types). The plugin never imports CPA internals.
//
// Capabilities advertised:
//
//	plugin.register        metadata + capability flags
//	auth.identifier        provider key "mistral-vibe"
//	auth.parse             register a Vibe token-plan credential
//	auth.login.start/poll  browser sign-in handoff (stub: returns not-handled)
//	auth.refresh           re-read the plan, refresh metadata
//	quota.identifier       "mistral-vibe"
//	quota.describe         plan + balance capability
//	quota.fetch            normalized plan, balance and periodic allowance windows
//	management.register    JSON routes under /v0/management/mistral-vibe/
//
// Credential model (two keys, one record):
//
//	plan_key         x-api-key for console.vibe.mistral.ai — reads the plan
//	inference_key    Bearer for api.mistral.ai             — spends it
//
// Every field of the upstream plan payload is optional; unknown shapes degrade
// to "plan known, numbers unknown" instead of invented figures.
package main

/*
#include "abi.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// buildVersion is overridden by -ldflags at release time; the constant is the
// version a plain `go build` reports.
var buildVersion = pluginVer

const (
	pluginName    = "Mistral Vibe Token Plan"
	pluginVer     = "0.1.0"
	pluginAuthor  = "NotPeppa"
	pluginRepo    = "https://github.com/NotPeppa/mistral-vibe"
	pluginID      = "mistral-vibe"
	providerID    = "mistral-vibe"
	schemaVersion = pluginabi.SchemaVersion

	// console.mistral.ai is the host that actually answers; console.vibe.* only
	// resolves through a different edge and refused the same request.
	defaultPlanBase = "https://console.mistral.ai"
	defaultAPIBase  = "https://api.mistral.ai"
	// planPath is what the plugin reads from the console. Verified live: the
	// only endpoint answering a personal key is /api/vibe/whoami.
	planPath     = "/api/vibe/whoami"
	maxBodyBytes = 1 << 20
	httpTimeout  = 12 * time.Second
)

// --------------------------------------------------------------------------- ABI exports

//export cpa_go_init
func cpa_go_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil {
		return 1
	}
	if uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	api.abi_version = C.uint32_t(pluginabi.ABIVersion)
	api.call = C.plugin_call_entry()
	api.free_buffer = C.plugin_free_entry()
	api.shutdown = C.plugin_shutdown_entry()
	return 0
}

// --------------------------------------------------------------------------- RPC dispatch

var descriptorOnce sync.Once
var descriptor []byte

// pluginDescriptor is the plugin.register result CPA reads: metadata, the
// capability flags it keys routing off, and the negotiated schema version.
// Capabilities are declared here as booleans because the host consumes them
// from the wire; the Go-side interface values above are what we implement.
// pluginDescriptor is the plugin.register result CPA reads.
//
// The wire schema is internal/pluginhost/rpc_schema.go's rpcRegistration:
// Metadata is pluginapi.Metadata, whose fields carry NO json tags, so CPA decodes
// "Name"/"Version"/"Author"/"GitHubRepository" — Go's default field names — not
// snake_case. Capabilities are booleans, one per capability CPA wires up.
// validPlugin then requires a name, a version, an author, a repository, and at
// least one capability, so all of them are spelled out here.
// pluginDescriptor is the plugin.register result CPA reads.
//
// The wire schema is internal/pluginhost/rpc_schema.go's rpcRegistration:
// Metadata is pluginapi.Metadata, whose fields carry NO json tags, so CPA decodes
// "Name"/"Version"/"Author"/"GitHubRepository" — Go's default field names — not
// snake_case. Capabilities are booleans, one per capability CPA wires up.
// validPlugin then requires a name, a version, an author, a repository, and at
// least one capability, so all of them are spelled out here.
//
// Version must satisfy ^[0-9][0-9A-Za-z.+-]*$: a leading "v" makes CPA reject the
// plugin outright, which is why the build strips it from a tag.
func pluginDescriptor() []byte {
	descriptorOnce.Do(func() {
		reg := map[string]any{
			"schema_version": schemaVersion,
			"metadata": map[string]any{
				"Name":             pluginName,
				"Version":          buildVersion,
				"Author":           pluginAuthor,
				"GitHubRepository": pluginRepo,
				"ConfigFields": []map[string]any{
					{"Name": "api_key", "Type": "string", "Default": "", "Description": "Mistral API key. The plugin publishes it to CPA's AI providers on load; leave it empty to enter the key on the plugin page instead."},
					{"Name": "base_url", "Type": "string", "Default": defaultAPIBase, "Description": "Mistral base URL for the provider entry."},
					{"Name": "plan_base", "Type": "string", "Default": defaultPlanBase, "Description": "Vibe console base URL used for the plan read."},
					{"Name": "model", "Type": "string", "Default": modelVibe, "Description": "Model shown on the balance banner."},
				},
			},
			"capabilities": map[string]any{
				"auth_provider":  true,
				"quota_provider": true,
				"management_api": true,
			},
		}
		descriptor, _ = json.Marshal(reg)
	})
	return descriptor
}

func handle(ctx context.Context, method string, body []byte) (json.RawMessage, error) {
	a := agent{}
	switch method {
	case pluginabi.MethodPluginRegister:
		configure(registrationConfig(body))
		publishConfiguredKey(ctx)
		return json.RawMessage(pluginDescriptor()), nil
	case pluginabi.MethodPluginReconfigure:
		// CPA revalidates the answer, so reconfigure repeats the descriptor.
		configure(registrationConfig(body))
		publishConfiguredKey(ctx)
		return json.RawMessage(pluginDescriptor()), nil
	case pluginabi.MethodPluginQuiesce:
		return marshalOK(map[string]bool{"ok": true})
	case pluginabi.MethodPluginShutdown:
		return marshalOK(map[string]bool{"ok": true})

	case pluginabi.MethodAuthIdentifier:
		return marshalOK(map[string]string{"identifier": providerID})
	case pluginabi.MethodAuthParse:
		var req pluginapi.AuthParseRequest
		if err := tolerantInto(body, &req); err != nil {
			return nil, fmt.Errorf("decode auth.parse: %w", err)
		}
		resp, err := a.ParseAuth(ctx, req)
		return marshalErr(resp, err)
	case pluginabi.MethodAuthLoginStart:
		var req pluginapi.AuthLoginStartRequest
		_ = tolerantInto(body, &req)
		resp, err := a.StartLogin(ctx, req)
		return marshalErr(resp, err)
	case pluginabi.MethodAuthLoginPoll:
		var req pluginapi.AuthLoginPollRequest
		_ = tolerantInto(body, &req)
		resp, err := a.PollLogin(ctx, req)
		return marshalErr(resp, err)
	case pluginabi.MethodAuthRefresh:
		var req pluginapi.AuthRefreshRequest
		if err := tolerantInto(body, &req); err != nil {
			return nil, fmt.Errorf("decode auth.refresh: %w", err)
		}
		resp, err := a.RefreshAuth(ctx, req)
		return marshalErr(resp, err)

	case pluginabi.MethodQuotaIdentifier:
		return marshalOK(map[string]string{"identifier": providerID})
	case pluginabi.MethodQuotaDescribe:
		var req pluginapi.QuotaDescribeRequest
		_ = tolerantInto(body, &req)
		resp, err := a.DescribeQuota(ctx, req)
		return marshalErr(resp, err)
	case pluginabi.MethodQuotaFetch:
		var req pluginapi.QuotaFetchRequest
		if err := tolerantInto(body, &req); err != nil {
			return nil, fmt.Errorf("decode quota.fetch: %w", err)
		}
		resp, err := a.FetchQuota(ctx, req)
		return marshalErr(resp, err)
	case pluginabi.MethodQuotaReset:
		return marshalOK(pluginapi.QuotaResetResponse{Success: false, Message: "mistral-vibe quota is provider-managed"})

	case pluginabi.MethodManagementRegister:
		var req pluginapi.ManagementRegistrationRequest
		_ = tolerantInto(body, &req)
		resp, err := a.RegisterManagement(ctx, req)
		if err != nil {
			return nil, err
		}
		return registrationWire(resp)
	case pluginabi.MethodManagementHandle:
		var req pluginapi.ManagementRequest
		if err := tolerantInto(body, &req); err != nil {
			return nil, fmt.Errorf("decode management.handle: %w", err)
		}
		resp, err := manager{}.HandleManagement(ctx, req)
		return marshalErr(resp, err)
	}
	return nil, fmt.Errorf("unsupported method %q", method)
}

func marshalOK(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func marshalErr(v any, err error) (json.RawMessage, error) {
	if err != nil {
		return nil, err
	}
	return marshalOK(v)
}

// --------------------------------------------------------------------------- agent: capabilities

type agent struct{}

var (
	_ pluginapi.AuthProvider  = agent{}
	_ pluginapi.QuotaProvider = agent{}
	_ pluginapi.ManagementAPI = agent{}
)

func (agent) Identifier() string { return providerID }

// ParseAuth registers a token-plan credential. CPA calls this with the raw auth
// file; a seed file with no key is accepted so the operator can fill it in.
// ParseAuth claims the auth files the management UI uploads.
//
// CPA's uploader builds a generic Auth first and only then lets plugins see the
// file, so a provider type CPA does not know never reaches the plugin unless the
// file also carries a marker CPA copies into Auth.Metadata. `base_url` is one:
// every Mistral credential needs it anyway, and an API key is required for the
// executor to work at all — so "has a Mistral key" is the claim, and the two
// accepted `type` values come along for clarity.
func (agent) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	if !claimsMistralFile(req) {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	cred := credentialFromJSON(req.RawJSON)
	return pluginapi.AuthParseResponse{Handled: true, Auth: cred.toAuthData(req.FileName)}, nil
}

// claimsMistralFile decides whether this file is ours.
func claimsMistralFile(req pluginapi.AuthParseRequest) bool {
	if req.Provider != "" && (strings.EqualFold(req.Provider, providerID) || strings.EqualFold(req.Provider, pluginID)) {
		return true
	}
	// A different known provider keeps its file.
	if req.Provider != "" && knownExternalProvider(req.Provider) {
		return false
	}
	cred := credentialFromJSON(req.RawJSON)
	if cred.PlanKey != "" && (cred.BaseURL != "" || cred.APIBase != "") {
		return true
	}
	return cred.PlanKey != "" && req.Provider == pluginID
}

// knownExternalProvider lists the provider types CPA already owns, so this
// plugin never steals their auth files.
func knownExternalProvider(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "gemini", "gemini-cli", "claude", "codex", "xai", "vertex", "antigravity",
		"kimi", "devin", "meta", "qwen", "iflow", "openai-compatibility":
		return true
	}
	return false
}

func (agent) StartLogin(_ context.Context, _ pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	// Vibe's browser sign-in exchanges a process id for a key on the console
	// host; wiring it up needs the console's sign-in gateway, so the plugin
	// asks the operator to paste a key instead of half-implementing the flow.
	return pluginapi.AuthLoginStartResponse{
		Provider: providerID,
		Metadata: map[string]any{
			"hint": "paste a Vibe plan key (x-api-key) into this auth file's plan_key field",
		},
	}, nil
}

func (agent) PollLogin(_ context.Context, _ pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	return pluginapi.AuthLoginPollResponse{Status: pluginapi.AuthLoginStatusError, Message: "manual key entry only"}, nil
}

// RefreshAuth re-reads the plan so the stored plan label and limits stay fresh.
// A failed refresh returns the stored state rather than failing the credential.
func (agent) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	cred := credentialFromParts(req.StorageJSON, req.Metadata, req.Attributes)
	if cred.Empty() {
		return pluginapi.AuthRefreshResponse{}, nil
	}
	if plan, err := cred.readPlan(ctx); err == nil && plan != nil {
		cred.absorb(plan)
	}
	return pluginapi.AuthRefreshResponse{
		Auth:             cred.toAuthData(""),
		NextRefreshAfter: time.Now().Add(6 * time.Hour).UTC(),
	}, nil
}

func (agent) DescribeQuota(_ context.Context, _ pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{providerID},
		DisplayName:        pluginName,
		SupportsReset:      false,
	}, nil
}

// FetchQuota is what CPA's management UI renders as the balance card.
func (agent) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if req.Provider != "" && !strings.EqualFold(req.Provider, providerID) {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("unsupported provider %q", req.Provider)
	}
	cred := credentialFromParts(req.StorageJSON, req.Metadata, req.Attributes)
	if cred.Empty() {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("credential carries no mistral-vibe plan key")
	}
	plan, err := cred.readPlan(ctx)
	if err != nil {
		return pluginapi.QuotaFetchResponse{}, err
	}
	out := plan.toQuota()
	if out.Subscription == nil && len(out.Summary) == 0 && len(out.Groups) == 0 {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("plan endpoint returned an unrecognized shape")
	}
	return out, nil
}

func (agent) ResetQuota(_ context.Context, _ pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: false, Message: "mistral-vibe quota is provider-managed"}, nil
}

func (agent) RegisterManagement(_ context.Context, req pluginapi.ManagementRegistrationRequest) (pluginapi.ManagementRegistrationResponse, error) {
	// The response crosses the ABI as JSON, so it must contain no interface
	// values: ManagementRoute.Handler is a pluginapi.ManagementHandler, which
	// encoding/json cannot marshal. Routes are declared by path only (CPA routes
	// every plugin route back to management.handle), and the resource page is
	// declared by path too.
	reg := map[string]any{
		"routes": []map[string]any{
			{"Method": http.MethodGet, "Path": "/" + pluginID + "/connect", "Description": "Add the plan's key to CPA's AI providers as an OpenAI-compatible entry."},
			{"Method": http.MethodPost, "Path": "/" + pluginID + "/connect", "Description": "Add or update the Mistral Vibe entry in CPA's AI providers."},
			{"Method": http.MethodGet, "Path": "/" + pluginID + "/balance", "Description": "The plan's month: budget, spent, remaining and reset time."},
			{"Method": http.MethodGet, "Path": "/" + pluginID + "/plan", "Description": "Raw plan read: whoami plus billing."},
			{"Method": http.MethodGet, "Path": "/" + pluginID + "/render", "Description": "mistral-vibe CLI styled banner for the plan balance."},
		},
		"resources": []map[string]any{
			{"Path": "/page", "Menu": "Mistral Vibe", "Description": "Plan balance and the AI-provider connect form."},
		},
	}
	raw, err := json.Marshal(reg)
	if err != nil {
		return pluginapi.ManagementRegistrationResponse{}, err
	}
	var resp pluginapi.ManagementRegistrationResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return pluginapi.ManagementRegistrationResponse{}, err
	}
	return resp, nil
}

// --------------------------------------------------------------------------- management routes

type manager struct{}

func (manager) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	// The resource page and the JSON routes share this handler; the path decides
	// which one answers.
	if strings.Contains(req.Path, "/v0/resource/") {
		return pageHandler{}.HandleManagement(ctx, req)
	}
	// Headers and Query arrive as http.Header and url.Values, but the tolerant
	// decoder hands them over as plain maps, so both shapes are read.
	header := func(name string) string {
		if value := req.Headers.Get(name); value != "" {
			return value
		}
		for key, values := range req.Headers {
			if strings.EqualFold(key, name) && len(values) > 0 {
				return values[0]
			}
		}
		return ""
	}
	query := func(name string) string {
		if value := req.Query.Get(name); value != "" {
			return value
		}
		for key, values := range req.Query {
			if strings.EqualFold(key, name) && len(values) > 0 {
				return values[0]
			}
		}
		return ""
	}
	cred := credential{
		PlanKey: strings.TrimSpace(firstNonEmpty(header("X-Vibe-Plan-Key"), query("plan_key"))),
		APIKey:  strings.TrimSpace(firstNonEmpty(header("X-Vibe-Api-Key"), query("api_key"))),
		BaseURL: strings.TrimSpace(firstNonEmpty(header("X-Vibe-Base-Url"), query("base_url"))),
		Label:   query("label"),
	}
	// A POST body may carry the credential instead of headers, which is what the
	// plugin page sends: the key never has to travel in a URL or a header there.
	if body := credentialFromJSON(req.Body); !body.Empty() {
		cred.PlanKey = firstNonEmpty(cred.PlanKey, body.PlanKey)
		cred.APIKey = firstNonEmpty(cred.APIKey, body.APIKey)
		cred.BaseURL = firstNonEmpty(cred.BaseURL, body.BaseURL)
		cred.APIBase = firstNonEmpty(cred.APIBase, body.APIBase)
	}
	if cred.PlanKey == "" {
		cred.PlanKey = strings.TrimPrefix(strings.TrimSpace(header("Authorization")), "Bearer ")
	}
	if cred.Empty() {
		return jsonRoute(http.StatusBadRequest, map[string]any{
			"error": "provide plan_key in the JSON body, an X-Vibe-Plan-Key header, or ?plan_key=",
		})
	}
	// The render route reads through the same path as quota.fetch so the month
	// and the rate limits land in the banner too.
	if base := strings.TrimSpace(firstNonEmpty(header("X-Vibe-Api-Base"), query("api_base"))); base != "" {
		cred.APIBase = base
	}
	// connect: publish the key into CPA's own AI-provider list, then let CPA
	// serve the traffic natively. The plan is a bonus, not a prerequisite.
	if strings.HasSuffix(req.Path, "/connect") {
		managementKey := firstNonEmpty(header("X-Management-Key"), strings.TrimPrefix(header("Authorization"), "Bearer "), query("management_key"))
		var requested struct {
			ManagementBase string `json:"management_base"`
		}
		_ = json.Unmarshal(req.Body, &requested)
		result, err := connectProviderAt(ctx, firstNonEmpty(requested.ManagementBase, header("X-Management-Base"), query("management_base")), managementKey, cred)
		if err != nil {
			return jsonRoute(http.StatusBadGateway, map[string]any{"error": err.Error()})
		}
		return jsonRoute(http.StatusOK, map[string]any{"ok": true, "provider": result})
	}
	plan, err := cred.readPlan(ctx)
	if err != nil {
		return jsonRoute(http.StatusBadGateway, map[string]any{"error": err.Error()})
	}
	switch {
	case strings.HasSuffix(req.Path, "/balance"):
		return jsonRoute(http.StatusOK, plan.balanceView())
	case strings.HasSuffix(req.Path, "/render"):
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte(renderCLI(plan, query("model"))),
		}, nil
	default:
		return jsonRoute(http.StatusOK, map[string]any{"ok": true, "plan": plan, "raw": plan.raw})
	}
}

func jsonRoute(code int, body any) (pluginapi.ManagementResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return pluginapi.ManagementResponse{StatusCode: http.StatusInternalServerError}, err
	}
	return pluginapi.ManagementResponse{
		StatusCode: code,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}, nil
}

// --------------------------------------------------------------------------- credential

type credential struct {
	PlanKey string  `json:"plan_key,omitempty"`
	APIKey  string  `json:"inference_key,omitempty"`
	BaseURL string  `json:"plan_base,omitempty"`
	APIBase string  `json:"api_base,omitempty"`
	Label   string  `json:"label,omitempty"`
	Plan    string  `json:"plan,omitempty"`
	Tier    string  `json:"tier,omitempty"`
	Balance float64 `json:"balance,omitempty"`
	HasBal  bool    `json:"has_balance,omitempty"`
	Unit    string  `json:"currency,omitempty"`
	Expires string  `json:"expires_at,omitempty"`
	Checked string  `json:"checked_at,omitempty"`
}

func (c credential) Empty() bool { return c.PlanKey == "" && c.APIKey == "" }

func credentialFromJSON(raw []byte) credential {
	var cred credential
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cred)
		var generic map[string]any
		if json.Unmarshal(raw, &generic) == nil {
			// The UI upload and hand-written seeds call the endpoint base_url;
			// the struct calls it plan_base/api_base.
			base := strings.TrimSpace(str(generic["base_url"]))
			if base != "" {
				cred.APIBase = firstNonEmpty(cred.APIBase, base)
				if cred.BaseURL == "" {
					cred.BaseURL = base
				}
			}
			cred.Label = firstNonEmpty(cred.Label, str(generic["label"]), str(generic["email"]))
		}
	}
	return cred.normalized()
}

func credentialFromParts(storage []byte, meta map[string]any, attrs map[string]string) credential {
	cred := credentialFromJSON(storage)
	if meta != nil {
		if raw, err := json.Marshal(meta); err == nil {
			var fromMeta credential
			if json.Unmarshal(raw, &fromMeta) == nil {
				fromMeta = fromMeta.normalized()
				cred.PlanKey = firstNonEmpty(cred.PlanKey, fromMeta.PlanKey)
				cred.APIKey = firstNonEmpty(cred.APIKey, fromMeta.APIKey)
				cred.BaseURL = firstNonEmpty(cred.BaseURL, fromMeta.BaseURL)
				cred.APIBase = firstNonEmpty(cred.APIBase, fromMeta.APIBase)
				cred.Plan = firstNonEmpty(cred.Plan, fromMeta.Plan)
				cred.Tier = firstNonEmpty(cred.Tier, fromMeta.Tier)
				if !cred.HasBal && fromMeta.HasBal {
					cred.Balance, cred.HasBal = fromMeta.Balance, true
				}
				cred.Unit = firstNonEmpty(cred.Unit, fromMeta.Unit)
				cred.Expires = firstNonEmpty(cred.Expires, fromMeta.Expires)
			}
		}
	}
	if attrs != nil {
		cred.BaseURL = firstNonEmpty(cred.BaseURL, attrs["plan_base"], attrs["base_url"])
		cred.APIBase = firstNonEmpty(cred.APIBase, attrs["api_base"])
		cred.Label = firstNonEmpty(cred.Label, attrs["label"], attrs["email"], attrs["name"])
	}
	return cred.normalized()
}

// normalized accepts the snake_case spelling this plugin writes plus the
// camelCase spellings operators tend to paste in by hand.
func (c credential) normalized() credential {
	if c.PlanKey == "" {
		c.PlanKey = ""
	}
	return c
}

func (c credential) toAuthData(fileName string) pluginapi.AuthData {
	// Only non-secret facts go in Metadata: CPA renders it in the UI and writes
	// it to the auth file. The keys live in StorageJSON, which the credential
	// reader consults first.
	meta := map[string]any{}
	if c.Plan != "" {
		meta["plan"] = c.Plan
	}
	if c.Tier != "" {
		meta["tier"] = c.Tier
	}
	if c.HasBal {
		meta["balance"] = c.Balance
		meta["currency"] = c.Unit
	}
	if c.Checked != "" {
		meta["checked_at"] = c.Checked
	}
	if c.Expires != "" {
		meta["expires_at"] = c.Expires
	}
	attrs := map[string]string{"provider": providerID}
	if c.BaseURL != "" && c.BaseURL != defaultPlanBase {
		attrs["plan_base"] = c.BaseURL
	}
	if c.APIBase != "" && c.APIBase != defaultAPIBase {
		attrs["api_base"] = c.APIBase
	}
	if c.Label != "" {
		attrs["label"] = c.Label
	}
	storage, _ := json.Marshal(map[string]any{
		"type":          providerID,
		"plan_key":      c.PlanKey,
		"inference_key": c.APIKey,
		"plan_base":     c.BaseURL,
		"api_base":      c.APIBase,
		"label":         c.Label,
		"plan":          c.Plan,
	})
	return pluginapi.AuthData{
		Provider:    providerID,
		ID:          storageID(c, fileName),
		FileName:    fileName,
		Label:       firstNonEmpty(c.Label, c.Plan, providerID),
		StorageJSON: storage,
		Metadata:    meta,
		Attributes:  attrs,
	}
}

func storageID(c credential, fileName string) string {
	if fileName != "" {
		return strings.TrimSuffix(fileName, ".json")
	}
	seed := firstNonEmpty(c.Label, c.PlanKey, providerID)
	sum := 0
	for _, r := range seed {
		sum = (sum*31 + int(r)) & 0x7fffffff
	}
	return fmt.Sprintf("%s-%06x", providerID, sum&0xffffff)
}

func (c *credential) absorb(plan *planResponse) {
	if plan == nil {
		return
	}
	c.Plan = firstNonEmpty(plan.Plan, c.Plan)
	c.Tier = firstNonEmpty(plan.Tier, c.Tier)
	if plan.HasBalance {
		c.Balance, c.HasBal = plan.Balance, true
		c.Unit = firstNonEmpty(plan.Unit, c.Unit)
	}
	c.Expires = firstNonEmpty(plan.Expires, c.Expires)
	c.Checked = time.Now().UTC().Format(time.RFC3339)
}

func (c credential) planBase() string {
	return normalizeBase(firstNonEmpty(c.BaseURL, defaultPlanBase))
}
func (c credential) apiBase() string { return normalizeBase(firstNonEmpty(c.APIBase, defaultAPIBase)) }

func normalizeBase(base string) string {
	base = strings.TrimSpace(base)
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "https://" + base
	}
	return strings.TrimRight(base, "/")
}

// --------------------------------------------------------------------------- plan read

type planResponse struct {
	Plan       string   `json:"plan,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	Balance    float64  `json:"balance"`
	HasBalance bool     `json:"has_balance"`
	Unit       string   `json:"currency,omitempty"`
	Windows    []window `json:"windows,omitempty"`
	Expires    string   `json:"expires_at,omitempty"`
	// Month of the plan, from billing: spent/limit/reset, pay-as-you-go on.
	Spent   float64 `json:"spent,omitempty"`
	Limit   float64 `json:"limit,omitempty"`
	ResetAt string  `json:"reset_at,omitempty"`
	Payg    bool    `json:"payg,omitempty"`
	raw     map[string]any
}

type window struct {
	Group     string  `json:"group,omitempty"`
	Window    string  `json:"window"`
	Remaining float64 `json:"remainingFraction"`
	Reset     string  `json:"resetTime,omitempty"`
}

var planPaths = []string{"/api/vibe/plan", "/api/vibe/subscription", "/api/vibe/whoami"}
var planHeaderSets = [][]string{{"x-api-key"}, {"Authorization"}}

func (c credential) readPlan(ctx context.Context) (*planResponse, error) {
	if c.PlanKey == "" {
		if c.Plan != "" || c.HasBal {
			return &planResponse{Plan: c.Plan, Tier: c.Tier, Balance: c.Balance, HasBalance: c.HasBal, Unit: c.Unit}, nil
		}
		return nil, fmt.Errorf("credential has no plan_key")
	}
	client := &http.Client{Timeout: httpTimeout, Transport: planTransport()}
	var lastErr error
	var live *rateLimits
	for _, path := range planPaths {
		for _, headers := range planHeaderSets {
			raw, limited, err := c.getJSON(ctx, client, c.planBase()+path, headers)
			if limited != nil {
				live = limited
			}
			if err != nil {
				lastErr = err
				continue
			}
			plan := shapePlan(raw)
			if live != nil {
				plan.attachRateLimits(live)
			}
			// whoami's plan_type says which budget this key spends; billing tells
			// the month. Without a kind the org budget would be ambiguous.
			if kind := keyKind(raw); kind != "" {
				c.attachBudget(ctx, client, plan, kind)
			}
			return plan, nil
		}
	}
	if c.Plan != "" || c.HasBal {
		// Never fail a credential that already has stored numbers.
		return &planResponse{Plan: c.Plan, Tier: c.Tier, Balance: c.Balance, HasBalance: c.HasBal, Unit: c.Unit, Expires: c.Expires}, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("plan endpoint unreachable")
	}
	return nil, lastErr
}

// attachBudget reads the plan's month from Mistral's billing API, the way
// magpie and Vibe's own CLI do, and makes it the plan's balance.
func (c credential) attachBudget(ctx context.Context, client *http.Client, plan *planResponse, kind string) {
	// One key reaches all three: whoami, billing and inference. A credential
	// that names a separate inference key still bills on its plan key.
	key := firstNonEmpty(c.APIKey, c.PlanKey)
	if key == "" {
		return
	}
	budget, err := c.getJSONBody(ctx, client, c.billingURL(billingBudgetPath), key)
	if err != nil {
		return
	}
	window, ok, err := readBudget(budget, kind, nil)
	if err != nil || !ok {
		return
	}
	var sub billingSubscription
	if body, err := c.getJSONBody(ctx, client, c.billingURL(billingSubscriptionPath), key); err == nil {
		if json.Unmarshal(body, &sub) == nil {
			payg := sub.VibePaygEnabled
			if kind == "api" {
				payg = sub.PaygEnabled
			}
			if again, okAgain, errAgain := readBudget(budget, kind, payg); errAgain == nil && okAgain {
				window = again
			}
			// Keep whoami's own plan name and add billing's title beside it:
			// "EDU" is the id, "Student" is what the app shows.
			if title := planTitles[strings.ToUpper(strings.TrimSpace(sub.PlanName))]; title != "" {
				if plan.Plan == "" || strings.EqualFold(plan.Plan, sub.PlanName) {
					plan.Plan = title
				} else {
					plan.Tier = firstNonEmpty(plan.Tier, title)
				}
			}
		}
	}
	plan.applyBudget(window)
}

// billingURL sits next to the inference host, so the same base that serves
// /v1/chat/completions serves /v1/billing/*.
func (c credential) billingURL(path string) string {
	return c.apiBase() + path
}

// getJSONBody fetches a billing document with the key as a bearer token.
func (c credential) getJSONBody(ctx context.Context, client *http.Client, url, key string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(key, "Bearer "))
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	return body, nil
}

// probeRateLimits spends one token to read the live allowance. Mistral returns
// x-ratelimit-* only on inference answers, and that is the only balance a
// personal token-plan key can see.
func (c credential) probeRateLimits(ctx context.Context, client *http.Client) *rateLimits {
	body := `{"model":"mistral-vibe-cli-fast","messages":[{"role":"user","content":"."}],"max_tokens":1}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiBase()+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	return rateLimitsFrom(resp.Header)
}

func (c credential) getJSON(ctx context.Context, client *http.Client, url string, headers []string) (map[string]any, *rateLimits, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range headers {
		value := c.PlanKey
		if strings.EqualFold(h, "Authorization") {
			value = "Bearer " + strings.TrimPrefix(value, "Bearer ")
		}
		req.Header.Set(h, value)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, nil, err
	}
	// Rate-limit headers arrive even on 4xx, and are the only live allowance
	// signal Mistral exposes to a personal key.
	limited := rateLimitsFrom(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return nil, limited, fmt.Errorf("%s %s: status %d", url, headers[0], resp.StatusCode)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, limited, fmt.Errorf("%s: not JSON", url)
	}
	return raw, limited, nil
}

// shapePlan normalizes the plan payload. Field names cover what the Vibe CLI
// itself reads (plan.kind / plan.title, budget.usagePercentage, paygEnabled)
// plus the token-plan spellings flat-fee proxies commonly use.
func shapePlan(raw map[string]any) *planResponse {
	out := &planResponse{raw: raw}
	planObj := asMap(raw["plan"])
	// whoami: {"plan_type":"API","plan_name":"FREE"}; richer payloads carry plan.title
	out.Plan = firstNonEmpty(str(planObj["title"]), str(raw["plan_title"]), str(raw["planTitle"]),
		str(raw["plan_name"]), str(raw["planName"]), str(raw["plan"]), str(planObj["kind"]))
	out.Tier = firstNonEmpty(str(planObj["tier"]), str(raw["tier"]), str(raw["tier_name"]),
		str(raw["tierName"]), str(raw["plan_type"]), str(raw["planType"]))

	budget := firstMap(raw["budget"], raw["limits"], raw["allowance"])
	if pct, ok := firstNum(budget, "usagePercentage", "usage_percentage", "usedPercent", "used_percent"); ok {
		remaining := clamp01(1 - pct/100.0)
		reset := firstNonEmpty(str(budget["resetAt"]), str(budget["reset_at"]), str(budget["period_end"]))
		out.Windows = append(out.Windows, window{Group: firstNonEmpty(out.Plan, providerID), Window: "monthly", Remaining: remaining, Reset: reset})
		if total, okTotal := firstNum(budget, "total", "total_amount", "limit"); okTotal && total > 0 {
			out.Balance, out.HasBalance = total*(1-pct/100.0), true
			out.Unit = firstNonEmpty(str(budget["currency"]), str(budget["unit"]), "USD")
		}
	}

	for _, key := range []string{"balance_infos", "balanceInfos", "balances"} {
		for _, item := range asList(raw[key]) {
			m := asMap(item)
			if m == nil {
				continue
			}
			if v, ok := firstNum(m, "total_balance", "totalBalance", "balance", "remaining", "credits"); ok {
				out.Balance, out.HasBalance = v, true
				out.Unit = firstNonEmpty(str(m["currency"]), str(m["unit"]), out.Unit)
			}
		}
	}
	if !out.HasBalance {
		for _, m := range []map[string]any{raw, asMap(raw["usage"]), asMap(raw["quota"]), budget} {
			if m == nil {
				continue
			}
			if v, ok := firstNum(m, "tokens_remaining", "tokensRemaining", "remaining_tokens", "tokens_left", "credits_remaining", "balance", "remaining"); ok {
				out.Balance, out.HasBalance = v, true
				out.Unit = firstNonEmpty(str(m["unit"]), str(m["currency"]), out.Unit, "tokens")
				break
			}
		}
	}

	windows := append(asList(raw["windows"]), asList(asMap(raw["quota"])["windows"])...)
	if len(windows) == 0 {
		windows = asList(raw["limits"])
	}
	for _, item := range windows {
		m := asMap(item)
		if m == nil {
			continue
		}
		if remaining, ok := windowFraction(m); ok {
			out.Windows = append(out.Windows, window{
				Group:     firstNonEmpty(str(m["group"]), str(m["name"]), out.Plan, providerID),
				Window:    firstNonEmpty(str(m["window"]), str(m["period"]), str(m["kind"]), "window"),
				Remaining: remaining,
				Reset:     firstNonEmpty(str(m["reset_at"]), str(m["resetAt"]), str(m["reset_time"]), str(m["resetTime"])),
			})
		}
	}
	out.Expires = firstNonEmpty(str(raw["expires_at"]), str(raw["expiresAt"]), str(raw["valid_until"]), str(raw["validUntil"]))
	sort.SliceStable(out.Windows, func(i, j int) bool { return out.Windows[i].Window < out.Windows[j].Window })
	return out
}

func windowFraction(m map[string]any) (float64, bool) {
	if v, ok := firstNum(m, "remainingFraction", "remaining_fraction", "left_fraction"); ok {
		return clamp01(v), true
	}
	if v, ok := firstNum(m, "remaining_percent", "remainingPercent"); ok {
		return clamp01(v / 100.0), true
	}
	if rem, okRem := firstNum(m, "remaining_amount", "remaining_tokens", "remaining"); okRem {
		if total, okTotal := firstNum(m, "total_amount", "total_tokens", "total", "limit"); okTotal && total > 0 {
			return clamp01(rem / total), true
		}
		if rem > 0 && rem <= 1 {
			return rem, true
		}
	}
	if pct, ok := firstNum(m, "used_percent", "usedPercent", "usagePercentage", "usage_percentage"); ok {
		return clamp01(1 - pct/100.0), true
	}
	return 0, false
}

func (p *planResponse) toQuota() pluginapi.QuotaFetchResponse {
	resp := pluginapi.QuotaFetchResponse{}
	if p == nil {
		return resp
	}
	if p.Plan != "" || p.Tier != "" {
		resp.Subscription = &pluginapi.QuotaSubscription{Plan: p.Plan, TierName: p.Tier}
	}
	if p.HasBalance {
		format := "number"
		if isCurrency(p.Unit) {
			format = "currency"
		}
		resp.Summary = append(resp.Summary, pluginapi.QuotaMetric{
			Key:      "balance",
			Label:    "Remaining balance",
			Value:    p.Balance,
			Unit:     p.Unit,
			Format:   format,
			Currency: p.Unit,
		})
	}
	groups := map[string][]pluginapi.QuotaBucket{}
	order := []string{}
	for _, w := range p.Windows {
		name := firstNonEmpty(w.Group, providerID)
		if _, seen := groups[name]; !seen {
			order = append(order, name)
		}
		groups[name] = append(groups[name], pluginapi.QuotaBucket{
			Window:            w.Window,
			RemainingFraction: w.Remaining,
			ResetTime:         w.Reset,
		})
	}
	for _, name := range order {
		resp.Groups = append(resp.Groups, pluginapi.QuotaGroup{DisplayName: name, Buckets: groups[name]})
	}
	return resp
}

// renderCLI prints the same footer gauge the Vibe CLI draws: an eighth-cell bar,
// a percentage floored so 100% always means "spent", and PAYG when overage bills.
func renderCLI(p *planResponse, model string) string {
	eighths := []string{"▏", "▎", "▍", "▌", "▋", "▊", "▉", "█"}
	bar := func(pct float64) string {
		steps := 48
		step := int(pct/100.0*float64(steps) + 0.5)
		switch {
		case pct >= 100:
			step = steps
		case pct > 0 && step < 1:
			step = 1
		case pct > 0 && step > steps-1:
			step = steps - 1
		}
		out := strings.Repeat(eighths[7], step/8)
		if rest := step % 8; rest > 0 {
			out += eighths[rest-1]
		}
		return out + strings.Repeat(" ", 6-max(0, len([]rune(out))))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Mistral Vibe token plan\n")
	if model != "" {
		fmt.Fprintf(&b, "model   %s\n", model)
	}
	if p != nil {
		fmt.Fprintf(&b, "plan    %s\n", firstNonEmpty(p.Plan, "unknown"))
		if p.Tier != "" {
			fmt.Fprintf(&b, "tier    %s\n", p.Tier)
		}
		if p.HasBalance {
			fmt.Fprintf(&b, "balance %s %s\n", trimFloat(p.Balance), p.Unit)
		}
		for _, w := range p.Windows {
			used := (1 - w.Remaining) * 100
			label := firstNonEmpty(w.Window, "window")
			// Sub-percent use would render as "0%" and hide a live signal; show
			// one decimal below 1% so a fresh allowance still reads as measured.
			amount := fmt.Sprintf("%d%%", int(used))
			if used > 0 && used < 1 {
				amount = fmt.Sprintf("%.1f%%", used)
			}
			fmt.Fprintf(&b, "usage   %-16s %s %s used%s\n", label, bar(used), amount, resetSuffix(w.Reset))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func resetSuffix(reset string) string {
	if reset == "" {
		return ""
	}
	return " · resets " + reset
}

func trimFloat(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}

func isCurrency(unit string) bool {
	switch strings.ToUpper(unit) {
	case "USD", "CNY", "EUR", "GBP", "JPY", "RMB":
		return true
	}
	return false
}

// --------------------------------------------------------------------------- helpers

func firstNum(m map[string]any, keys ...string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			if f, okNum := num(v); okNum {
				return f, true
			}
		}
	}
	return 0, false
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case float32:
		return float64(t), true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(t), "%f", &f); err == nil {
			return f, true
		}
	}
	return 0, false
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func firstMap(values ...any) map[string]any {
	for _, v := range values {
		if m := asMap(v); m != nil {
			return m
		}
	}
	return nil
}

func asList(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func main() {}

// --------------------------------------------------------------------------- ABI entry points

//export cpa_go_call
func cpa_go_call(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) C.int {
	return call(method, request, length, response)
}

// call is the JSON-RPC entry the host reaches through cliproxy_plugin_call.
func call(method *C.char, request *C.uint8_t, length C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	name := ""
	if method != nil {
		name = C.GoString(method)
	}
	var body []byte
	if request != nil && length > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(length))
	}
	result, err := handle(context.Background(), name, body)
	if err != nil {
		envelope, _ := json.Marshal(pluginabi.Envelope{
			OK:    false,
			Error: &pluginabi.Error{Code: "plugin_error", Message: err.Error()},
		})
		return writeBuffer(envelope, response)
	}
	envelope, errMarshal := json.Marshal(pluginabi.Envelope{OK: true, Result: result})
	if errMarshal != nil {
		return 1
	}
	return writeBuffer(envelope, response)
}

//export cpa_go_free
func cpa_go_free(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cpa_go_shutdown
func cpa_go_shutdown() {}

func writeBuffer(payload []byte, response *C.cliproxy_buffer) C.int {
	if response == nil || len(payload) == 0 {
		return 0
	}
	mem := C.CBytes(payload)
	if mem == nil {
		return 1
	}
	response.ptr = mem
	response.len = C.size_t(len(payload))
	return 0
}

// balanceView is the shape the plugin page and `/balance` both render.
func (p *planResponse) balanceView() map[string]any {
	if p == nil {
		return map[string]any{"ok": false}
	}
	view := map[string]any{
		"ok":    true,
		"plan":  p.Plan,
		"tier":  p.Tier,
		"offer": providerName,
	}
	if p.HasBalance {
		view["balance"] = p.Balance
		view["currency"] = p.Unit
		view["spent"] = p.Spent
		view["limit"] = p.Limit
	}
	if p.ResetAt != "" {
		view["reset_at"] = p.ResetAt
	}
	if p.Payg {
		view["payg"] = true
	}
	windows := make([]map[string]any, 0, len(p.Windows))
	for _, w := range p.Windows {
		windows = append(windows, map[string]any{
			"window":    w.Window,
			"remaining": w.Remaining,
			"reset":     w.Reset,
		})
	}
	view["windows"] = windows
	return view
}

// registrationWire renders a management registration without its handlers.
// CPA's rpc adapter assigns route.Handler itself after decoding; sending one
// makes its decoder fail with "cannot unmarshal object into
// Go struct field ManagementRoute.Handler".
func registrationWire(resp pluginapi.ManagementRegistrationResponse) (json.RawMessage, error) {
	type route struct {
		Method      string
		Path        string
		Menu        string
		Description string
	}
	out := struct {
		Routes    []route
		Resources []route
	}{}
	for _, item := range resp.Routes {
		out.Routes = append(out.Routes, route{Method: item.Method, Path: item.Path, Menu: item.Menu, Description: item.Description})
	}
	for _, item := range resp.Resources {
		out.Resources = append(out.Resources, route{Path: item.Path, Menu: item.Menu, Description: item.Description})
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// registrationConfig pulls config_yaml out of a plugin.register/reconfigure
// request: the raw YAML CPA keeps under plugins.configs.<pluginID>.
//
// ConfigYAML is a []byte on the wire, so encoding/json renders it as base64 and
// decoding it straight into a []byte is what gives the text back.
func registrationConfig(body []byte) []byte {
	var envelope struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	return envelope.ConfigYAML
}

// publishConfiguredKey makes a key from config.yaml usable without any page or
// curl. It cannot call CPA's management API — the plugin never sees the
// management key — so it publishes through the path CPA already watches: the
// `openai-compatibility` list is config, and the config file is readable.
//
// Where that file is follows CPA's own rules: the auth directory's parent, or
// the working directory of the CPA process.
func publishConfiguredKey(ctx context.Context) {
	cfg := currentConfig()
	key := strings.TrimSpace(cfg.apiKey)
	if key == "" {
		return
	}
	cred := credential{PlanKey: key, APIKey: key, APIBase: firstNonEmpty(cfg.baseURL, defaultAPIBase)}
	publishToConfigFile(cred)
}

// managementBase is the loopback address of the CPA that hosts this plugin.
func (c pluginConfig) managementBase() string {
	return ""
}
