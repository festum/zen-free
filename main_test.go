package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFingerprintUAContract(t *testing.T) {
	const suffix = " ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

	if expectedVersion := os.Getenv("EXPECTED_OPENCODE_VERSION"); expectedVersion != "" {
		if got, want := opencodeVersion, expectedVersion; got != want {
			t.Fatalf("opencodeVersion = %q, want linker-injected %q", got, want)
		}
	}

	if !strings.HasPrefix(fingerprintUA, "opencode/") {
		t.Fatalf("fingerprintUA = %q, want prefix opencode/", fingerprintUA)
	}

	version := strings.TrimPrefix(fingerprintUA, "opencode/")
	version, _, _ = strings.Cut(version, " ")
	if version == "" {
		t.Fatal("fingerprintUA has an empty opencode version")
	}

	if !strings.HasSuffix(fingerprintUA, suffix) {
		t.Fatalf("fingerprintUA = %q, want suffix %q", fingerprintUA, suffix)
	}
	if got, want := fingerprintUA, "opencode/"+opencodeVersion+suffix; got != want {
		t.Fatalf("fingerprintUA = %q, want %q", got, want)
	}
}

func TestBuildFingerprintUAUsesOpencodeVersion(t *testing.T) {
	originalVersion := opencodeVersion
	t.Cleanup(func() {
		opencodeVersion = originalVersion
	})

	opencodeVersion = "9.9.9"
	const expected = "opencode/9.9.9 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if got := buildFingerprintUA(); got != expected {
		t.Fatalf("buildFingerprintUA() = %q, want %q", got, expected)
	}
}

func TestCapturedOpenCodeToolSchemasMatchChatAndResponsesWires(t *testing.T) {
	if len(capturedOpenCodeTools) != 6 {
		t.Fatalf("captured tool count = %d, want 6", len(capturedOpenCodeTools))
	}
	chatBody := map[string]any{}
	chatInjected := augmentChatTools(chatBody)
	responsesBody := map[string]any{}
	responsesInjected := augmentFlatTools(responsesBody)
	chatTools := chatBody["tools"].([]any)
	responsesTools := responsesBody["tools"].([]any)
	if len(chatTools) != len(capturedOpenCodeTools) || len(responsesTools) != len(capturedOpenCodeTools) {
		t.Fatalf("wire tool counts = chat %d, Responses %d", len(chatTools), len(responsesTools))
	}
	for i, raw := range capturedOpenCodeTools {
		captured := raw.(map[string]any)["function"]
		chatTool := chatTools[i].(map[string]any)["function"]
		responsesTool := responsesTools[i].(map[string]any)
		responsesFunction := make(map[string]any, len(responsesTool)-1)
		for key, value := range responsesTool {
			if key != "type" {
				responsesFunction[key] = value
			}
		}
		want, err := json.Marshal(captured)
		if err != nil {
			t.Fatal(err)
		}
		gotChat, err := json.Marshal(chatTool)
		if err != nil {
			t.Fatal(err)
		}
		gotResponses, err := json.Marshal(responsesFunction)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotChat) != string(want) || string(gotResponses) != string(want) {
			t.Fatalf("tool %d schema differs from captured definition", i)
		}
		name := stringValue(captured.(map[string]any)["name"])
		if !chatInjected[name] || !responsesInjected[name] {
			t.Fatalf("tool %q missing from injection filter", name)
		}
	}
}
