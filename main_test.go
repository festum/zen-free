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
	chatInjected := augmentChatTools(chatBody, false)
	responsesBody := map[string]any{}
	responsesInjected := augmentFlatTools(responsesBody, false)
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

func TestInferencePathRecognizesExactSegments(t *testing.T) {
	tests := []struct {
		upstream string
		want     bool
	}{
		{upstream: "https://opencode.ai/inference", want: true},
		{upstream: "https://opencode.ai/inference/openai", want: true},
		{upstream: "https://opencode.ai/inference/anthropic", want: true},
		{upstream: "https://opencode.ai/inference/google", want: true},
		{upstream: "https://opencode.ai/zen/v1", want: false},
		{upstream: "https://opencode.ai/not-inference/openai", want: false},
	}
	for _, tt := range tests {
		if got := inferencePath(tt.upstream); got != tt.want {
			t.Errorf("inferencePath(%q) = %t, want %t", tt.upstream, got, tt.want)
		}
	}
}

func TestNormalizeCapturedCoreToolsPreservesCallerTools(t *testing.T) {
	for _, nested := range []bool{true, false} {
		name := "Responses"
		tools := []any{
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{"type": "string"}},
			map[string]any{"type": "function", "name": "write", "parameters": map[string]any{"type": "string"}},
		}
		if nested {
			name = "Chat"
			for _, tool := range tools {
				flat := tool.(map[string]any)
				function := make(map[string]any, len(flat)-1)
				for key, value := range flat {
					if key != "type" {
						function[key] = value
					}
				}
				flat["function"] = function
				delete(flat, "name")
				delete(flat, "parameters")
			}
		}
		t.Run(name, func(t *testing.T) {
			body := map[string]any{"tools": tools}
			var filter map[string]bool
			if nested {
				filter = augmentChatTools(body, true)
			} else {
				filter = augmentFlatTools(body, true)
			}
			got := array(t, body["tools"])
			wantNames := []string{"lookup", "bash", "write", "glob", "grep", "read"}
			if len(got) != len(wantNames) {
				t.Fatalf("normalized tools count = %d, want %d", len(got), len(wantNames))
			}
			for i, value := range got {
				tool := object(t, value)
				if nested {
					tool = object(t, tool["function"])
				}
				if tool["name"] != wantNames[i] {
					t.Errorf("tool %d name = %#v, want %q", i, tool["name"], wantNames[i])
				}
				if wantNames[i] != "lookup" {
					for _, captured := range capturedOpenCodeTools {
						function := object(t, object(t, captured)["function"])
						if function["name"] != wantNames[i] {
							continue
						}
						want, err := json.Marshal(function)
						if err != nil {
							t.Fatal(err)
						}
						gotSchemaValue := tool
						if !nested {
							gotSchemaValue = make(map[string]any, len(tool)-1)
							for key, value := range tool {
								if key != "type" {
									gotSchemaValue[key] = value
								}
							}
						}
						gotSchema, err := json.Marshal(gotSchemaValue)
						if err != nil {
							t.Fatal(err)
						}
						if string(gotSchema) != string(want) {
							t.Errorf("tool %q does not match captured definition", wantNames[i])
						}
					}
				}
			}
			for _, core := range []string{"bash", "glob", "grep", "read", "write"} {
				if !filter[core] {
					t.Errorf("filter does not hide normalized core tool %q", core)
				}
			}
			if filter["lookup"] {
				t.Error("filter hides arbitrary caller tool lookup")
			}
		})
	}
}
