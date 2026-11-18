package main

import (
	"encoding/json"
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
