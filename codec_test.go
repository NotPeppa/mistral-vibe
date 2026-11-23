package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestStorageBytesAcceptsRawObject(t *testing.T) {
	raw := map[string]any{"plan_key": "sk-1", "plan_base": "http://x"}
	got := storageBytes(raw)
	if string(got) != `{"plan_base":"http://x","plan_key":"sk-1"}` {
		t.Fatalf("got %q", got)
	}
}

func TestStorageBytesAcceptsBase64(t *testing.T) {
	if got := storageBytes("eyJhIjoxfQ=="); string(got) != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestStorageBytesAcceptsRawJSONString(t *testing.T) {
	if got := storageBytes(`{"a":1}`); string(got) != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestTolerantIntoMapsSnakeCaseOntoStruct(t *testing.T) {
	body := []byte(`{"provider":"mistral-vibe","file_name":"v.json","raw_json":{"plan_key":"sk-x"}}`)
	var req pluginapi.AuthParseRequest
	if err := tolerantInto(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Provider != "mistral-vibe" || req.FileName != "v.json" {
		t.Fatalf("got %+v", req)
	}
	if string(req.RawJSON) != `{"plan_key":"sk-x"}` {
		t.Fatalf("raw json re-encoded wrong: %q", req.RawJSON)
	}
}

func TestTolerantIntoAcceptsQuotaBucketNumbers(t *testing.T) {
	body := []byte(`{"provider":"mistral-vibe","storage_json":{"plan_key":"sk-x"},"host_callback_id":"c1"}`)
	var req pluginapi.QuotaFetchRequest
	if err := tolerantInto(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Provider != "mistral-vibe" || string(req.StorageJSON) != `{"plan_key":"sk-x"}` {
		t.Fatalf("got %+v", req)
	}
}

func TestPascal(t *testing.T) {
	cases := map[string]string{"plan_key": "PlanKey", "planKey": "PlanKey", "StorageJSON": "StorageJSON", "storage_json": "StorageJSON"}
	for in, want := range cases {
		if got := pascal(in); got != want {
			t.Errorf("pascal(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShapePlanReadsVibeBudget(t *testing.T) {
	var raw map[string]any
	_ = json.Unmarshal([]byte(`{"plan":{"title":"Vibe Pro"},"budget":{"usagePercentage":38,"total":100,"currency":"USD"},"windows":[{"group":"claude","window":"5h","remainingFraction":0.62}]}`), &raw)
	p := shapePlan(raw)
	if p.Plan != "Vibe Pro" || !p.HasBalance || p.Balance != 62 {
		t.Fatalf("got %+v", p)
	}
	if len(p.Windows) != 2 {
		t.Fatalf("windows = %+v", p.Windows)
	}
}

func TestShapePlanUnknownShapeIsHonest(t *testing.T) {
	p := shapePlan(map[string]any{"something": "else"})
	if p.HasBalance || p.Plan != "" || len(p.Windows) != 0 {
		t.Fatalf("got %+v", p)
	}
}

func TestToQuotaMarksCurrency(t *testing.T) {
	p := &planResponse{Plan: "Vibe Pro", Balance: 12.5, HasBalance: true, Unit: "USD",
		Windows: []window{{Group: "claude", Window: "5h", Remaining: 0.5}}}
	q := p.toQuota()
	if q.Subscription == nil || q.Subscription.Plan != "Vibe Pro" {
		t.Fatalf("subscription: %+v", q.Subscription)
	}
	if len(q.Summary) != 1 || q.Summary[0].Format != "currency" || q.Summary[0].Currency != "USD" {
		t.Fatalf("summary: %+v", q.Summary)
	}
	if len(q.Groups) != 1 || q.Groups[0].Buckets[0].RemainingFraction != 0.5 {
		t.Fatalf("groups: %+v", q.Groups)
	}
}

func TestRenderCLIBarShape(t *testing.T) {
	out := renderCLI(&planResponse{Plan: "Vibe Pro", Balance: 8.5, HasBalance: true, Unit: "USD",
		Windows: []window{{Window: "5h", Remaining: 0.75}}}, "mistral-large-latest")
	if out == "" {
		t.Fatal("empty render")
	}
	for _, want := range []string{"Vibe Pro", "8.5 USD", "25% used", "mistral-large-latest"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q: %s", want, out)
		}
	}
}

func TestMgmtDecodePreservesHeaderNames(t *testing.T) {
	body := []byte(`{"method":"GET","path":"/v0/management/mistral-vibe/render","headers":{"X-Vibe-Plan-Key":["sk-plan"]},"query":{"plan_key":["sk"]}}`)
	var req pluginapi.ManagementRequest
	if err := tolerantInto(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := req.Headers.Get("X-Vibe-Plan-Key"); got != "sk-plan" {
		t.Fatalf("header name mangled: %v", req.Headers)
	}
	if got := req.Query.Get("plan_key"); got != "sk" {
		t.Fatalf("query key mangled: %v", req.Query)
	}
}

func TestTolerantIntoPreservesTaggedFieldsAndNestedStructs(t *testing.T) {
	body := []byte(`{"provider":"mistral-vibe","auth_index":"idx-42","auth_id":"aid-9","storage_json":{"plan_key":"sk-x"}}`)
	var qReq pluginapi.QuotaFetchRequest
	if err := tolerantInto(body, &qReq); err != nil {
		t.Fatalf("quota decode: %v", err)
	}
	if qReq.AuthIndex != "idx-42" || qReq.AuthID != "aid-9" {
		t.Fatalf("tagged fields lost: %+v", qReq)
	}

	usageBody := []byte(`{
		"provider":"mistral-vibe",
		"model":"mistral-large-latest",
		"api_key":"k1",
		"detail":{"input_tokens":120,"output_tokens":30,"cached_tokens":40,"total_tokens":150},
		"response_headers":{"X-Custom-Header":["val"]}
	}`)
	var uRec pluginapi.UsageRecord
	if err := tolerantInto(usageBody, &uRec); err != nil {
		t.Fatalf("usage decode: %v", err)
	}
	if uRec.APIKey != "k1" || uRec.Detail.InputTokens != 120 || uRec.Detail.CachedTokens != 40 || uRec.Detail.TotalTokens != 150 {
		t.Fatalf("nested usage detail lost: %+v", uRec)
	}
	if got := uRec.ResponseHeaders.Get("X-Custom-Header"); got != "val" {
		t.Fatalf("response header mangled: %v", uRec.ResponseHeaders)
	}
}

func TestStorageBytesDecodesNonJSONBase64(t *testing.T) {
	// "hello world" in standard base64.
	if got := storageBytes("aGVsbG8gd29ybGQ="); string(got) != "hello world" {
		t.Fatalf("got %q, want %q", got, "hello world")
	}
}
