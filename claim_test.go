package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// What the management UI uploads for this plugin: the operator picks a name
// ending in .json and pastes a Mistral key plus the endpoint it belongs to.
const uiUpload = `{
  "type": "mistral-vibe",
  "plan_key": "mstrl_test_plan",
  "inference_key": "mstrl_test_inference",
  "base_url": "https://api.mistral.ai",
  "label": "work"
}`

func TestParseAuthClaimsUIUpload(t *testing.T) {
	resp, err := agent{}.ParseAuth(nil, pluginapi.AuthParseRequest{
		Provider: "mistral-vibe",
		FileName: "vibe-work.json",
		RawJSON:  []byte(uiUpload),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Handled {
		t.Fatal("an uploaded mistral-vibe file must be claimed")
	}
	if resp.Auth.Provider != providerID {
		t.Errorf("provider = %q", resp.Auth.Provider)
	}
	var stored map[string]string
	if err := json.Unmarshal(resp.Auth.StorageJSON, &stored); err != nil {
		t.Fatalf("storage: %v", err)
	}
	if stored["plan_key"] != "mstrl_test_plan" || stored["inference_key"] != "mstrl_test_inference" {
		t.Fatalf("keys not kept in storage: %v", stored)
	}
	// Metadata is shown in the UI and written to disk, so no key may appear there.
	for _, value := range resp.Auth.Metadata {
		if text, ok := value.(string); ok && len(text) > 8 && (text == "mstrl_test_plan" || text == "mstrl_test_inference") {
			t.Fatalf("a key leaked into Metadata: %v", resp.Auth.Metadata)
		}
	}
}

func TestParseAuthClaimsWhenTypeIsUnknownButKeyIsPresent(t *testing.T) {
	// CPA's uploader synthesizes a generic Auth before plugins see the file, so
	// the provider can arrive as an empty or unknown type. The key and its
	// endpoint are what identify the file.
	resp, err := agent{}.ParseAuth(nil, pluginapi.AuthParseRequest{
		Provider: "",
		FileName: "pasted.json",
		RawJSON:  []byte(uiUpload),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Handled {
		t.Fatal("a file with a Mistral key and endpoint must be claimed")
	}
}

func TestParseAuthLeavesOtherProvidersAlone(t *testing.T) {
	for _, provider := range []string{"gemini", "claude", "codex", "xai", "openai-compatibility"} {
		body := `{"type":"` + provider + `","access_token":"x","base_url":"https://example.test"}`
		resp, err := agent{}.ParseAuth(nil, pluginapi.AuthParseRequest{
			Provider: provider,
			FileName: provider + ".json",
			RawJSON:  []byte(body),
		})
		if err != nil {
			t.Fatalf("%s: %v", provider, err)
		}
		if resp.Handled {
			t.Errorf("%s must not be claimed by this plugin", provider)
		}
	}
}

func TestParseAuthRejectsAnUnrelatedFile(t *testing.T) {
	resp, err := agent{}.ParseAuth(nil, pluginapi.AuthParseRequest{
		Provider: "something-else",
		FileName: "x.json",
		RawJSON:  []byte(`{"type":"something-else","token":"abc"}`),
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Handled {
		t.Fatal("a file with no Mistral material must be left alone")
	}
}

func TestCredentialFromStorageRecoversBothKeys(t *testing.T) {
	// Round trip: what ParseAuth stores must be what the executor reads back.
	resp, _ := agent{}.ParseAuth(nil, pluginapi.AuthParseRequest{
		Provider: "mistral-vibe",
		FileName: "vibe-work.json",
		RawJSON:  []byte(uiUpload),
	})
	cred := credentialFromParts(resp.Auth.StorageJSON, resp.Auth.Metadata, resp.Auth.Attributes)
	if cred.PlanKey != "mstrl_test_plan" {
		t.Errorf("plan key lost: %+v", cred)
	}
	if cred.APIKey != "mstrl_test_inference" {
		t.Errorf("inference key lost: %+v", cred)
	}
	if cred.BaseURL != "https://api.mistral.ai" && cred.APIBase != "https://api.mistral.ai" {
		t.Errorf("base url lost: %+v", cred)
	}
}

func TestBalanceRouteFallsBackToTheConfiguredKey(t *testing.T) {
	// With no key in the request, the route must use plugins.configs.<id>.api_key
	// — that is what makes the page need no input at all.
	configure([]byte("enabled: true\napi_key: \"mstrl_configured\"\nbase_url: \"https://api.mistral.ai\"\n"))
	defer configure(nil)

	cred := credential{}
	cfg := currentConfig()
	if cfg.apiKey != "mstrl_configured" {
		t.Fatalf("configured key not read: %q", cfg.apiKey)
	}
	// The fallback the route performs.
	cred.PlanKey = cfg.apiKey
	cred.APIKey = cfg.apiKey
	cred.APIBase = cfg.baseUrl()
	cred.BaseURL = cfg.planBaseOrDefault()

	if cred.PlanKey != "mstrl_configured" {
		t.Fatalf("plan key = %q", cred.PlanKey)
	}
	// The plan lives on the console; the inference base must not be reused for it.
	if cred.planBase() != "https://console.mistral.ai" {
		t.Fatalf("planBase = %q", cred.planBase())
	}
	if cred.apiBase() != "https://api.mistral.ai" {
		t.Fatalf("apiBase = %q", cred.apiBase())
	}
}

func TestManagementKeyIsNeverUsedAsAMistralKey(t *testing.T) {
	// `Authorization` on a management route carries CPA's own key. Treating it as
	// a Mistral key sent the management secret upstream and came back 401.
	req := pluginapi.ManagementRequest{
		Method:  "GET",
		Path:    "/v0/management/mistral-vibe/balance",
		Headers: map[string][]string{"Authorization": {"Bearer cpa-management-key"}},
		Query:   map[string][]string{},
	}
	resp, err := manager{}.HandleManagement(context.Background(), req)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if strings.Contains(string(resp.Body), "cpa-management-key") {
		t.Fatalf("the management key leaked into an upstream call: %s", resp.Body)
	}
}
