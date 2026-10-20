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
			for _, core := range []string{"glob", "grep", "read"} {
				if !filter[core] {
					t.Errorf("filter does not hide appended tool %q", core)
				}
			}
			for _, caller := range []string{"bash", "write", "lookup"} {
				if filter[caller] {
					t.Errorf("filter hides caller tool %q", caller)
				}
			}
			cases := []struct {
				name      string
				callers   []string
				normalize bool
			}{
				{name: "absent", normalize: true},
				{name: "empty", callers: []string{}, normalize: true},
				{name: "complete required", callers: []string{"bash", "glob", "grep", "read"}, normalize: true},
				{name: "outside inference", callers: []string{"bash", "lookup"}},
			}
			for _, raw := range capturedOpenCodeTools {
				core := toolName(object(t, raw))
				cases = append(cases, struct {
					name      string
					callers   []string
					normalize bool
				}{name: "caller " + core, callers: []string{core}, normalize: true})
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					body := map[string]any{}
					callerTools := make([]any, 0, len(tc.callers))
					for _, caller := range tc.callers {
						function := map[string]any{"name": caller, "parameters": map[string]any{"type": "string"}}
						tool := map[string]any{"type": "function", "function": function}
						if !nested {
							tool = map[string]any{"type": "function", "name": caller, "parameters": function["parameters"]}
						}
						callerTools = append(callerTools, tool)
					}
					if tc.callers != nil {
						body["tools"] = callerTools
					}
					before, _ := json.Marshal(body)
					filter := augmentTools(body, nested, tc.normalize)
					if !tc.normalize {
						after, _ := json.Marshal(body)
						if string(after) != string(before) || len(filter) != 0 {
							t.Fatalf("non-inference tools changed: body=%s filter=%v", after, filter)
						}
						return
					}
					got := array(t, body["tools"])
					wantNames := append([]string{}, tc.callers...)
					if len(tc.callers) == 0 {
						wantNames = []string{"bash", "edit", "glob", "grep", "read", "write"}
					} else {
						for _, required := range []string{"bash", "glob", "grep", "read"} {
							present := false
							for _, caller := range tc.callers {
								present = present || caller == required
							}
							if !present {
								wantNames = append(wantNames, required)
							}
						}
					}
					if len(got) != len(wantNames) || len(filter) != len(wantNames)-len(tc.callers) {
						t.Fatalf("tools=%#v filter=%v, want names=%v and %d hidden", got, filter, wantNames, len(wantNames)-len(tc.callers))
					}
					for i, wantName := range wantNames {
						tool := object(t, got[i])
						want, _ := json.Marshal(capturedTool(wantName, nested))
						actual, _ := json.Marshal(tool)
						if toolName(tool) != wantName || string(actual) != string(want) || filter[wantName] != (i >= len(tc.callers)) {
							t.Errorf("tool[%d]=%s hidden=%t, want %s hidden=%t", i, actual, filter[wantName], want, i >= len(tc.callers))
						}
					}
				})
			}
		})
	}
}
