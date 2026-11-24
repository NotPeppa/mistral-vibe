package main

import (
	"strings"
	"testing"
)

func TestMergeCompatEntryAppendsWhenMissing(t *testing.T) {
	config := "port: 8317\nplugins:\n  enabled: true\n"
	merged, changed := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k", []string{"a", "b"}, nil)
	if !changed {
		t.Fatal("expected a change")
	}
	if !strings.Contains(merged, "openai-compatibility:") {
		t.Fatalf("block missing:\n%s", merged)
	}
	if !strings.Contains(merged, `"Mistral Vibe"`) || !strings.Contains(merged, `api-key: "mstrl_k"`) {
		t.Fatalf("entry wrong:\n%s", merged)
	}
	// The original settings survive.
	if !strings.Contains(merged, "port: 8317") || !strings.Contains(merged, "plugins:") {
		t.Fatalf("config lost:\n%s", merged)
	}
}

func TestMergeCompatEntryIsIdempotent(t *testing.T) {
	config := "port: 8317\n"
	once, _ := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k", []string{"a", "b"}, nil)
	twice, changed := mergeCompatEntry(once, "https://api.mistral.ai", "mstrl_k", []string{"a", "b"}, nil)
	if changed {
		t.Fatalf("a second run must not rewrite:\n%s", twice)
	}
	if once != twice {
		t.Fatalf("content changed:\n%s\n---\n%s", once, twice)
	}
}

func TestMergeCompatEntryUpdatesTheKey(t *testing.T) {
	config := "port: 8317\n"
	first, _ := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_old", []string{"a"}, nil)
	second, changed := mergeCompatEntry(first, "https://api.mistral.ai", "mstrl_new", []string{"a"}, nil)
	if !changed {
		t.Fatal("a new key must be written")
	}
	if strings.Contains(second, "mstrl_old") || !strings.Contains(second, "mstrl_new") {
		t.Fatalf("key not replaced:\n%s", second)
	}
}

func TestMergeCompatEntryKeepsFollowingBlocks(t *testing.T) {
	config := `port: 8317
openai-compatibility:
  - name: "Someone Else"
    base-url: "https://other.example/v1"
gateway:
  enabled: true
`
	merged, changed := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k", []string{"a", "b"}, nil)
	if !changed {
		t.Fatal("expected a change")
	}
	if !strings.Contains(merged, "gateway:") {
		t.Fatalf("following block lost:\n%s", merged)
	}
	if !strings.Contains(merged, "Someone Else") || !strings.Contains(merged, "https://other.example/v1") {
		t.Fatalf("existing provider was wiped out:\n%s", merged)
	}
	if !strings.Contains(merged, "Mistral Vibe") {
		t.Fatalf("entry missing:\n%s", merged)
	}
	// Updating Mistral Vibe must keep "Someone Else" intact too.
	updated, changed2 := mergeCompatEntry(merged, "https://api.mistral.ai", "mstrl_k2", []string{"a", "b"}, nil)
	if !changed2 {
		t.Fatal("expected update on key change")
	}
	if !strings.Contains(updated, "Someone Else") || !strings.Contains(updated, "mstrl_k2") || strings.Contains(updated, `"mstrl_k"`) {
		t.Fatalf("update broke existing providers:\n%s", updated)
	}
	// Idempotent after update.
	if _, changed3 := mergeCompatEntry(updated, "https://api.mistral.ai", "mstrl_k2", []string{"a", "b"}, nil); changed3 {
		t.Fatalf("third merge should be idempotent:\n%s", updated)
	}
}

func TestMergeCompatEntryAtLineZeroAndTrailingV1(t *testing.T) {
	config := "openai-compatibility: []\nport: 8317\n"
	merged, changed := mergeCompatEntry(config, "https://api.mistral.ai/v1", "mstrl_k", []string{"a"}, nil)
	if !changed {
		t.Fatal("expected a change")
	}
	if strings.HasPrefix(merged, "\n") {
		t.Fatalf("spurious leading newline:\n%q", merged)
	}
	if strings.Contains(merged, "/v1/v1") {
		t.Fatalf("double /v1 suffix:\n%s", merged)
	}
	if !strings.Contains(merged, `"https://api.mistral.ai/v1"`) {
		t.Fatalf("missing normalized base-url:\n%s", merged)
	}
}

func TestCompatBlockListsTheGivenModels(t *testing.T) {
	block := compatBlock("https://api.mistral.ai", "k", []string{"mistral-large-latest", "codestral-latest", "voxtral-mini-latest"}, nil)
	for _, id := range []string{"mistral-large-latest", "codestral-latest", "voxtral-mini-latest"} {
		if !strings.Contains(block, id) {
			t.Errorf("model %s missing", id)
		}
	}
}

func TestCompatBlockFallsBackWhenNoModels(t *testing.T) {
	// An empty list comes from a failed read; the block must still be usable.
	merged, changed := mergeCompatEntry("port: 1\n", "https://api.mistral.ai", "k", nil, nil)
	if !changed {
		t.Fatal("expected a write")
	}
	for _, id := range fallbackModels {
		if !strings.Contains(merged, id) {
			t.Errorf("fallback model %s missing", id)
		}
	}
}

// The provider entry is what CPA forwards with, so the Vibe CLI presentation has
// to be on it: that is what asks Mistral to book this traffic against the Vibe
// allowance instead of the API one.
func TestPublishedEntryCarriesTheVibeCLIPresentation(t *testing.T) {
	headers := ambientHeaders("vibe_cli", nil)
	if headers["X-Cli-Scoping"] != "cli" || headers["X-Mistral-Client"] != "vibe-cli" {
		t.Fatalf("headers = %+v", headers)
	}
	merged, changed := mergeCompatEntry("port: 1\n", "https://api.mistral.ai", "k", []string{"a"}, headers)
	if !changed {
		t.Fatal("expected a write")
	}
	for _, want := range []string{"    headers:\n", `X-Cli-Scoping: "cli"`, `User-Agent: "mistral-vibe-cli/0.1"`} {
		if !strings.Contains(merged, want) {
			t.Fatalf("missing %q in:\n%s", want, merged)
		}
	}
	// Stable rendering: the same headers must not rewrite the file again.
	if _, again := mergeCompatEntry(merged, "https://api.mistral.ai", "k", []string{"a"}, headers); again {
		t.Fatal("republishing unchanged headers rewrote the config")
	}
}

func TestOperatorHeadersRideOnTheDefaults(t *testing.T) {
	headers := ambientHeaders("vibe_cli", map[string]string{"x-mistral-client": "vibe-cli-pro", "Accept": ""})
	// The operator's value wins, under the spelling the default established, so
	// one header never appears twice in two cases.
	if headers["X-Mistral-Client"] != "vibe-cli-pro" {
		t.Fatalf("override lost: %+v", headers)
	}
	for name := range headers {
		if strings.EqualFold(name, "X-Mistral-Client") && name != "X-Mistral-Client" {
			t.Fatalf("duplicate spelling %q beside the default: %+v", name, headers)
		}
	}
	if _, still := headers["Accept"]; still {
		t.Fatalf("an empty value must clear the default: %+v", headers)
	}
	if len(ambientHeaders("passthrough", nil)) != 0 {
		t.Fatal("passthrough must send nothing the plugin invented")
	}
}
