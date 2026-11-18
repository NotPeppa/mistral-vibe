package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeManagement stands in for CPA's own management API.
type fakeManagement struct {
	entries []map[string]any
	patched map[string]any
	putBody []byte
}

func (f *fakeManagement) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/openai-compatibility", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"openai-compatibility": f.entries})
		case http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &f.patched)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case http.MethodPut:
			f.putBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func withManagementBase(t *testing.T, url string) {
	t.Helper()
	t.Setenv("VIBE_MANAGEMENT_BASE", url)
}

func TestConnectCreatesTheProvider(t *testing.T) {
	fake := &fakeManagement{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	withManagementBase(t, server.URL)

	result, err := connectProvider(context.Background(), "mgmt-key", credential{
		PlanKey: "mstrl_test", APIKey: "mstrl_test", APIBase: "https://api.mistral.ai",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !result.Created || result.Updated {
		t.Fatalf("result = %+v", result)
	}
	if result.Name != providerName || result.BaseURL != "https://api.mistral.ai/v1" {
		t.Fatalf("result = %+v", result)
	}
	var written []map[string]any
	if err := json.Unmarshal(fake.putBody, &written); err != nil {
		t.Fatalf("put body: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected one entry, got %d", len(written))
	}
	entry := written[0]
	if entry["name"] != providerName || entry["base-url"] != "https://api.mistral.ai/v1" {
		t.Fatalf("entry = %v", entry)
	}
	keys, _ := entry["api-key-entries"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys = %v", keys)
	}
	if first, _ := keys[0].(map[string]any); first["api-key"] != "mstrl_test" {
		t.Fatalf("key = %v", keys[0])
	}
	models, _ := entry["models"].([]any)
	// No /v1/models on the fake, so the fallback list is published.
	if len(models) != len(fallbackModels) {
		t.Fatalf("models = %v", models)
	}
}

func TestConnectUpdatesInsteadOfDuplicating(t *testing.T) {
	fake := &fakeManagement{entries: []map[string]any{{
		"name":       providerName,
		"base-url":   "https://api.mistral.ai/v1",
		"models":     []any{},
		"auth_index": "should-be-dropped",
	}}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	withManagementBase(t, server.URL)

	result, err := connectProvider(context.Background(), "mgmt-key", credential{
		PlanKey: "mstrl_second", APIKey: "mstrl_second", APIBase: "https://api.mistral.ai",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !result.Updated || result.Created {
		t.Fatalf("result = %+v", result)
	}
	if fake.putBody != nil {
		t.Fatalf("an update must not PUT the whole list")
	}
	if fake.patched == nil {
		t.Fatal("expected a PATCH")
	}
	value, _ := fake.patched["value"].(map[string]any)
	if value["name"] != providerName {
		t.Fatalf("patched value = %v", value)
	}
}

func TestConnectAdoptsAHandAddedEntry(t *testing.T) {
	// The operator already added Mistral by hand: adopt it, do not duplicate.
	fake := &fakeManagement{entries: []map[string]any{{
		"name":     "My Mistral",
		"base-url": "https://api.mistral.ai/v1",
	}}}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	withManagementBase(t, server.URL)

	result, err := connectProvider(context.Background(), "mgmt-key", credential{
		PlanKey: "mstrl_x", APIKey: "mstrl_x", APIBase: "https://api.mistral.ai",
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !result.Updated || fake.putBody != nil {
		t.Fatalf("expected an in-place update, got %+v (put=%v)", result, fake.putBody)
	}
}

func TestConnectRequiresAManagementKey(t *testing.T) {
	_, err := connectProvider(context.Background(), "", credential{PlanKey: "k", APIKey: "k"})
	if err == nil || !strings.Contains(err.Error(), "management key") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectRequiresAKey(t *testing.T) {
	_, err := connectProvider(context.Background(), "mgmt", credential{})
	if err == nil {
		t.Fatal("expected an error without a Mistral key")
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"https://api.mistral.ai":     "api.mistral.ai",
		"https://api.mistral.ai/v1":  "api.mistral.ai",
		"http://127.0.0.1:8317/v1":   "127.0.0.1:8317",
		"https://console.mistral.ai": "console.mistral.ai",
		"":                           "",
	}
	for input, want := range cases {
		if got := hostOf(input); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", input, got, want)
		}
	}
	if got := hostOf("https://api.mistral.ai/v1"); got == hostOf("https://console.mistral.ai") {
		t.Error("different hosts must not match")
	}
}

func TestPageIsServed(t *testing.T) {
	response, err := pageHandler{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	body := string(response.Body)
	if !strings.Contains(body, "Mistral Vibe") {
		t.Fatalf("page body looks wrong: %.120s", body)
	}
	// The page must never contain a key or a management key.
	if strings.Contains(body, "mstrl_") {
		t.Fatal("the page must not embed credentials")
	}
}

func TestResourcePathServesThePage(t *testing.T) {
	resp, err := manager{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  "GET",
		Path:    "/v0/resource/plugins/cpa-mistral-vibe/page",
		Headers: map[string][]string{},
		Query:   map[string][]string{},
	})
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(resp.Body), "Mistral Vibe") {
		t.Fatalf("status=%d body=%.80s", resp.StatusCode, resp.Body)
	}
}

func TestManagementPathStaysJSON(t *testing.T) {
	resp, err := manager{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method:  "GET",
		Path:    "/v0/management/mistral-vibe/connect",
		Headers: map[string][]string{},
		Query:   map[string][]string{},
	})
	if err != nil {
		t.Fatalf("management: %v", err)
	}
	// Without a Mistral key anywhere the route says so; with one configured it
	// gets past that and stops at the missing management key.
	body := string(resp.Body)
	if !strings.Contains(body, "plan_key") && !strings.Contains(body, "management key") && !strings.Contains(body, "Mistral key") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

func TestSchemaVersionIsOldEnoughForShippedCPA(t *testing.T) {
	// CPA rejects a plugin newer than itself:
	//   "plugin.register failed: plugin schema version 10 is not supported"
	// 8.0.4 tops out at 6, so the plugin must not claim more.
	if schemaVersion > 6 {
		t.Fatalf("schema version %d breaks CPA 8.0.4, which supports up to 6", schemaVersion)
	}
	// And the descriptor must report exactly what we declared.
	var reg struct {
		SchemaVersion uint32 `json:"schema_version"`
	}
	if err := json.Unmarshal(pluginDescriptor(), &reg); err != nil {
		t.Fatal(err)
	}
	if reg.SchemaVersion != schemaVersion {
		t.Fatalf("descriptor says %d, package says %d", reg.SchemaVersion, schemaVersion)
	}
}

func TestConnectPublishesTheUpstreamModelList(t *testing.T) {
	// A stand-in for CPA's management API plus Mistral's /v1/models.
	var written []byte
	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/openai-compatibility", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"openai-compatibility":[]}`))
		case http.MethodPut:
			written, _ = io.ReadAll(r.Body)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"mistral-large-latest"},{"id":"codestral-latest"},
			{"id":"voxtral-mini-latest"},{"id":"mistral-large-latest"}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	withManagementBase(t, server.URL)

	result, err := connectProvider(context.Background(), "mgmt-key", credential{
		PlanKey: "mstrl_k", APIKey: "mstrl_k", APIBase: server.URL,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if len(result.Models) != 3 {
		t.Fatalf("models = %v (duplicates must collapse)", result.Models)
	}
	if !strings.Contains(string(written), "codestral-latest") {
		t.Fatalf("upstream models not published:\n%s", written)
	}
	if strings.Contains(string(written), "mistral-vibe-cli-fast") {
		t.Fatalf("fallback leaked into a successful read:\n%s", written)
	}
}
