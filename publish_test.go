package main

import (
	"strings"
	"testing"
)

func TestMergeCompatEntryAppendsWhenMissing(t *testing.T) {
	config := "port: 8317\nplugins:\n  enabled: true\n"
	merged, changed := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k")
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
	once, _ := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k")
	twice, changed := mergeCompatEntry(once, "https://api.mistral.ai", "mstrl_k")
	if changed {
		t.Fatalf("a second run must not rewrite:\n%s", twice)
	}
	if once != twice {
		t.Fatalf("content changed:\n%s\n---\n%s", once, twice)
	}
}

func TestMergeCompatEntryUpdatesTheKey(t *testing.T) {
	config := "port: 8317\n"
	first, _ := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_old")
	second, changed := mergeCompatEntry(first, "https://api.mistral.ai", "mstrl_new")
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
	merged, changed := mergeCompatEntry(config, "https://api.mistral.ai", "mstrl_k")
	if !changed {
		t.Fatal("expected a change")
	}
	if !strings.Contains(merged, "gateway:") {
		t.Fatalf("following block lost:\n%s", merged)
	}
	if !strings.Contains(merged, "Mistral Vibe") {
		t.Fatalf("entry missing:\n%s", merged)
	}
}

func TestCompatBlockListsThePlanModels(t *testing.T) {
	block := compatBlock("https://api.mistral.ai", "k")
	for _, id := range compatModels {
		if !strings.Contains(block, id) {
			t.Errorf("model %s missing", id)
		}
	}
}
