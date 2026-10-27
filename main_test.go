package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestFingerprintUAContract(t *testing.T) {
	if expectedVersion := os.Getenv("EXPECTED_OPENCODE_VERSION"); expectedVersion != "" {
		if got, want := opencodeVersion, expectedVersion; got != want {
			t.Fatalf("opencodeVersion = %q, want linker-injected %q", got, want)
		}
	} else if opencodeVersion != "latest/2.1.4" {
		t.Fatalf("opencodeVersion = %q, want default %q", opencodeVersion, "latest/2.1.4")
	}

	if got, want := fingerprintUA, "opencode/"+opencodeVersion+"/cli"; got != want {
		t.Fatalf("fingerprintUA = %q, want %q", got, want)
	}
	if got, want := buildFingerprintUA(), fingerprintUA; got != want {
		t.Fatalf("buildFingerprintUA() = %q, want %q", got, want)
	}
}

func TestBuildFingerprintUAUsesOpencodeVersion(t *testing.T) {
	originalVersion := opencodeVersion
	t.Cleanup(func() {
		opencodeVersion = originalVersion
	})

	opencodeVersion = "9.9.9"
	const expected = "opencode/9.9.9/cli"
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
			wantNames := []string{"edit", "glob", "grep", "read", "write", "lookup", "bash"}
			wantHidden := map[string]bool{"edit": true, "glob": true, "grep": true, "read": true}
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
				assertWiredToolSchema(t, value, wantNames[i], nested)
				if filter[wantNames[i]] != wantHidden[wantNames[i]] {
					t.Errorf("tool %q hidden = %t, want %t", wantNames[i], filter[wantNames[i]], wantHidden[wantNames[i]])
				}
			}

			cases := []struct {
				name       string
				callers    []string
				normalize  bool
				wantNames  []string
				wantHidden []string
			}{
				{
					name:       "absent",
					normalize:  true,
					wantNames:  []string{"edit", "glob", "grep", "read", "write", "bash"},
					wantHidden: []string{"bash", "edit", "glob", "grep", "read", "write"},
				},
				{
					name:       "empty",
					callers:    []string{},
					normalize:  true,
					wantNames:  []string{"edit", "glob", "grep", "read", "write", "bash"},
					wantHidden: []string{"bash", "edit", "glob", "grep", "read", "write"},
				},
				{
					name:       "complete required",
					callers:    []string{"bash", "glob", "grep", "read"},
					normalize:  true,
					wantNames:  []string{"edit", "glob", "grep", "read", "write", "bash"},
					wantHidden: []string{"edit", "write"},
				},
				{
					name:      "outside inference",
					callers:   []string{"bash", "lookup"},
					wantNames: []string{"bash", "lookup"},
				},
			}
			for _, core := range []string{"bash", "edit", "glob", "grep", "read", "write"} {
				hidden := []string{"bash", "edit", "glob", "grep", "read", "write"}
				for i, candidate := range hidden {
					if candidate == core {
						hidden = append(hidden[:i], hidden[i+1:]...)
						break
					}
				}
				cases = append(cases, struct {
					name       string
					callers    []string
					normalize  bool
					wantNames  []string
					wantHidden []string
				}{
					name:       "caller " + core,
					callers:    []string{core},
					normalize:  true,
					wantNames:  []string{"edit", "glob", "grep", "read", "write", "bash"},
					wantHidden: hidden,
				})
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
					wantHidden := make(map[string]bool, len(tc.wantHidden))
					for _, hidden := range tc.wantHidden {
						wantHidden[hidden] = true
					}
					if len(got) != len(tc.wantNames) {
						t.Fatalf("tools=%#v filter=%v, want names=%v", got, filter, tc.wantNames)
					}
					for i, wantName := range tc.wantNames {
						value := got[i]
						tool := object(t, value)
						if toolName(tool) != wantName {
							t.Errorf("tool[%d] = %q, want %q", i, toolName(tool), wantName)
						}
						if filter[wantName] != wantHidden[wantName] {
							t.Errorf("tool %q hidden = %t, want %t", wantName, filter[wantName], wantHidden[wantName])
						}
						if wantName != "bash" && !containsManifestName(wantName) {
							want, _ := json.Marshal(callerTool(callerTools, wantName))
							actual, _ := json.Marshal(value)
							if string(actual) != string(want) {
								t.Errorf("caller tool %q = %s, want %s", wantName, actual, want)
							}
							continue
						}
						assertWiredToolSchema(t, value, wantName, nested)
					}
				})
			}
		})
	}
}
