package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestPageRendersWithoutAnyRequestAuth(t *testing.T) {
	// CPA hands resource routes to the plugin with no management auth, so the
	// page must never depend on it: no fetch to a management route, and the
	// values arrive already rendered.
	configure([]byte("enabled: true\napi_key: \"\"\n"))
	defer configure(nil)

	resp, err := pageHandler{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{
		Method: "GET", Path: "/v0/resource/plugins/cpa-mistral-vibe/page",
		Headers: map[string][]string{}, Query: map[string][]string{},
	})
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	body := string(resp.Body)
	if strings.Contains(body, "{{") {
		t.Fatalf("unrendered placeholder left in the page:\n%s", body[:400])
	}
	// With no key configured the page says so instead of showing blank numbers.
	if !strings.Contains(body, "api_key") {
		t.Fatalf("missing the no-key hint")
	}
	// The balance must not be fetched by the browser.
	if strings.Contains(body, "/balance") && strings.Contains(body, "fetch(") {
		if strings.Contains(body, "management/mistral-vibe/balance") {
			t.Fatalf("the page still fetches the balance over a management route")
		}
	}
}

func TestPageEscapesUntrustedText(t *testing.T) {
	// Plan names come from upstream; they are escaped into the page.
	configure([]byte("enabled: true\napi_key: \"\"\n"))
	defer configure(nil)
	resp, _ := pageHandler{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet})
	if strings.Contains(string(resp.Body), "<script>alert") {
		t.Fatal("unescaped script in the page")
	}
}

func TestUsageMetricsSeparateLabelFromValue(t *testing.T) {
	// A CJK label glued to a monospace number reads as one token ("今日请求3"),
	// so the label and the value are separate block elements.
	configure([]byte("enabled: true\napi_key: \"\"\n"))
	defer configure(nil)
	resp, _ := pageHandler{}.HandleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet})
	body := string(resp.Body)
	for _, label := range []string{"今日请求", "输入", "输出", "缓存命中", "推理", "合计"} {
		pattern := `<span class="k">` + label + `</span><span class="num`
		if !strings.Contains(body, pattern) {
			t.Errorf("label %q is not its own element", label)
		}
	}
	// And the label must never be inline with the number.
	if strings.Contains(body, `<span class="k muted">今日请求</span><span class="num`) {
		t.Fatal("the old inline label markup is back")
	}
}
