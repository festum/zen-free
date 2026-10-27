package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode JSON: %v\n%s", err, raw)
	}
	return value
}

func object(t *testing.T, value any) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want object", value)
	}
	return result
}

func array(t *testing.T, value any) []any {
	t.Helper()
	result, ok := value.([]any)
	if !ok {
		t.Fatalf("got %T, want array", value)
	}
	return result
}

func sampleResponsesSSE(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/responses-sse-sample.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func responses(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func ssePayloads(t *testing.T, body string) []map[string]any {
	t.Helper()
	var payloads []map[string]any
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.TrimSpace(line) == "data: [DONE]" {
			continue
		}
		payloads = append(payloads, decodeJSON(t, []byte(strings.TrimPrefix(line, "data: "))))
	}
	return payloads
}

func TestBuildResponsesRequest(t *testing.T) {
	chat := map[string]any{
		"model":  "vendor/muse-spark-1.3 (high)",
		"stream": false,
		"messages": []any{
			map[string]any{"role": "system", "content": "  follow rules  "},
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{
				map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call-1", "content": "found"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "description": "look up a value", "parameters": map[string]any{"type": "object"},
		}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
		"temperature": 0.2,
		"top_p":       0.8,
		"max_tokens":  99,
		"discard_me":  "not part of the Responses request",
	}

	got := buildResponsesRequest(chat)
	if got["model"] != chat["model"] || got["stream"] != true {
		t.Fatalf("model/stream = (%v, %v), want (%v, true)", got["model"], got["stream"], chat["model"])
	}
	if got["instructions"] != "follow rules\n\nbe brief" {
		t.Fatalf("instructions = %q", got["instructions"])
	}
	if _, exists := got["discard_me"]; exists {
		t.Fatal("unrecognized request field was copied")
	}
	if got["temperature"] != 0.2 || got["top_p"] != 0.8 || got["max_output_tokens"] != 99 {
		t.Fatalf("sampling fields = %#v", got)
	}
	if got["tool_choice"] == nil || !reflect.DeepEqual(got["tool_choice"], map[string]any{"type": "function", "name": "lookup"}) {
		t.Fatalf("named tool_choice = %#v, want the selected lookup tool", got["tool_choice"])
	}
	tools := array(t, got["tools"])
	var lookup map[string]any
	for _, entry := range tools {
		tool := object(t, entry)
		if tool["name"] == "lookup" {
			lookup = tool
		}
	}
	if lookup == nil || lookup["type"] != "function" || lookup["description"] != "look up a value" || lookup["parameters"] == nil {
		t.Fatalf("nested function tool not converted to flat tool: %#v", lookup)
	}

	input := array(t, got["input"])
	if len(input) != 4 {
		t.Fatalf("input has %d entries, want message, function call, and function output in source order: %#v", len(input), input)
	}
	user := object(t, input[0])
	if user["type"] != "message" || user["role"] != "user" || object(t, array(t, user["content"])[0])["type"] != "input_text" {
		t.Fatalf("user input = %#v", user)
	}
	assistant := object(t, input[1])
	if assistant["type"] != "message" || object(t, array(t, assistant["content"])[0])["text"] != "checking" {
		t.Fatalf("assistant text was not retained before its call: %#v", assistant)
	}
	call := object(t, input[2])
	if call["type"] != "function_call" || call["call_id"] != "call-1" || call["arguments"] != `{"q":"x"}` {
		t.Fatalf("paired function call = %#v", call)
	}
	output := object(t, input[3])
	if output["type"] != "function_call_output" || output["call_id"] != "call-1" || output["output"] != "found" {
		t.Fatalf("paired tool output = %#v", output)
	}
}

func TestBuildResponsesRequestContentParts(t *testing.T) {
	chat := map[string]any{"model": "muse-spark", "messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}},
			map[string]any{"type": "unsupported", "value": "drop"},
		}},
	}}
	input := array(t, buildResponsesRequest(chat)["input"])
	content := array(t, object(t, input[0])["content"])
	if len(content) != 2 {
		t.Fatalf("content parts = %#v, want text and image only", content)
	}
	if part := object(t, content[0]); part["type"] != "input_text" || part["text"] != "look" {
		t.Fatalf("text part = %#v", part)
	}
	if part := object(t, content[1]); part["type"] != "input_image" || part["image_url"] != "https://example.invalid/image.png" {
		t.Fatalf("image part = %#v", part)
	}
}

func TestBuildResponsesRequestInstructionsToolsAndChoice(t *testing.T) {
	callerTool := map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}
	callerRead := map[string]any{"type": "function", "function": map[string]any{"name": "read", "parameters": map[string]any{"type": "object"}}}
	wantCallerTool := map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}
	wantCallerRead := map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}
	tests := []struct {
		name        string
		tools       []any
		choice      any
		wantChoice  any
		wantPresent bool
		wantTools   []any
	}{
		{name: "missing tools stay empty", wantTools: []any{}},
		{name: "empty tools stay empty", tools: []any{}, wantTools: []any{}},
		{name: "caller tool preserved", tools: []any{callerTool}, wantTools: []any{wantCallerTool}},
		{name: "caller read stays alone", tools: []any{callerRead}, wantTools: []any{wantCallerRead}},
		{name: "auto choice without tools omitted", choice: "auto", wantTools: []any{}},
		{name: "caller function choice preserved", tools: []any{callerTool}, choice: map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, wantChoice: map[string]any{"type": "function", "name": "lookup"}, wantPresent: true, wantTools: []any{wantCallerTool}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := map[string]any{"model": "muse-spark", "messages": []any{}}
			if tt.tools != nil {
				chat["tools"] = tt.tools
			}
			if tt.choice != nil {
				chat["tool_choice"] = tt.choice
			}
			got := buildResponsesRequest(chat)
			if _, exists := got["instructions"]; exists {
				t.Fatalf("empty instructions should be omitted: %#v", got["instructions"])
			}
			choice, exists := got["tool_choice"]
			if exists != tt.wantPresent || !reflect.DeepEqual(choice, tt.wantChoice) {
				t.Fatalf("tool_choice = %#v, present=%t; want %#v, present=%t", choice, exists, tt.wantChoice, tt.wantPresent)
			}
			tools, exists := got["tools"]
			if !exists || !reflect.DeepEqual(tools, tt.wantTools) {
				t.Fatalf("tools = %#v, want %#v", tools, tt.wantTools)
			}
		})
	}
}

func TestProxyHandlerToolAugmentationAndChoicePreservation(t *testing.T) {
	callerTools := `[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`
	callerChatTools := `[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]`
	wantNames := []string{"bash", "edit", "glob", "grep", "read", "write"}
	tests := []struct {
		name          string
		path          string
		body          string
		wantChoice    any
		wantPresent   bool
		wantNames     []string
		wantNested    bool
		wantToolsJSON string
	}{
		{name: "chat tool-less injects captured tools and disables choice", path: "/v1/chat/completions", body: `{"model":"gpt-4o","messages":[]}`, wantChoice: "none", wantPresent: true, wantNames: wantNames, wantNested: true},
		{name: "chat empty tools injects and preserves explicit choice", path: "/v1/chat/completions", body: `{"model":"gpt-4o","messages":[],"tools":[],"tool_choice":"required"}`, wantChoice: "required", wantPresent: true, wantNames: wantNames, wantNested: true},
		{name: "chat caller tools and choice untouched", path: "/v1/chat/completions", body: `{"model":"gpt-4o","messages":[],"tools":` + callerChatTools + `,"tool_choice":"none"}`, wantChoice: "none", wantPresent: true, wantNames: []string{"lookup"}, wantNested: true, wantToolsJSON: callerChatTools},
		{name: "Responses tool-less injects flat tools and leaves choice unpinned", path: "/v1/responses", body: `{"model":"muse-spark-1.3","input":"hello"}`, wantNames: wantNames},
		{name: "Responses empty tools injects and preserves choice", path: "/v1/responses", body: `{"model":"muse-spark-1.3","input":"hello","tools":[],"tool_choice":"required"}`, wantChoice: "required", wantPresent: true, wantNames: wantNames},
		{name: "Responses caller tools and choice untouched", path: "/v1/responses", body: `{"model":"muse-spark-1.3","input":"hello","tools":` + callerTools + `,"tool_choice":"none"}`, wantChoice: "none", wantPresent: true, wantNames: []string{"lookup"}, wantToolsJSON: callerTools},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamBody map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				upstreamBody = decodeJSON(t, body)
				if strings.Contains(r.URL.Path, "/v1/responses") {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sampleResponsesSSE(t))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			request := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
			w := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
			if w.Code != http.StatusOK {
				t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
			}
			choice, exists := upstreamBody["tool_choice"]
			if exists != tt.wantPresent || choice != tt.wantChoice {
				t.Fatalf("upstream tool_choice = %#v, present=%t; want %#v, present=%t", choice, exists, tt.wantChoice, tt.wantPresent)
			}
			tools := array(t, upstreamBody["tools"])
			if len(tools) != len(tt.wantNames) {
				t.Fatalf("upstream tools = %#v, want %v tools", tools, tt.wantNames)
			}
			for i, value := range tools {
				tool := object(t, value)
				if tool["type"] != "function" {
					t.Fatalf("upstream tool %d has type %#v", i, tool["type"])
				}
				if tt.wantNested {
					tool = object(t, tool["function"])
				}
				if tool["name"] != tt.wantNames[i] {
					t.Fatalf("upstream tool %d name = %#v, want %q", i, tool["name"], tt.wantNames[i])
				}
			}
			if tt.wantToolsJSON != "" {
				wantTools := array(t, decodeJSON(t, []byte(`{"tools":`+tt.wantToolsJSON+`}`))["tools"])
				if !reflect.DeepEqual(tools, wantTools) {
					t.Fatalf("upstream tools = %#v, want caller tools unchanged %#v", tools, wantTools)
				}
			}
		})
	}
}

func TestProxyHandlerInferenceNormalizationPreservesWireFormats(t *testing.T) {
	for _, tt := range []struct {
		name         string
		upstreamPath string
		path         string
		wantURI      string
		body         string
		wantNested   bool
		wantNames    []string
		wantChoice   any
	}{
		{name: "inference OpenAI Chat", upstreamPath: "/inference", path: "/v1/chat/completions?trace=chat%2F1", wantURI: "/inference/openai/v1/chat/completions?trace=chat%2F1", body: `{"model":"gpt-4o","messages":[],"tool_choice":"required","tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"string"}}},{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, wantNested: true, wantNames: []string{"edit", "glob", "grep", "read", "write", "bash", "lookup"}, wantChoice: "required"},
		{name: "inference Muse Chat", upstreamPath: "/inference", path: "/v1/chat/completions?trace=muse%2Fchat", wantURI: "/inference/openai/v1/responses?trace=muse%2Fchat", body: `{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hi"}],"tool_choice":"required","tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"string"}}}]}`, wantNames: []string{"edit", "glob", "grep", "read", "write", "bash"}, wantChoice: "required"},
		{name: "inference Muse Responses", upstreamPath: "/inference", path: "/v1/responses?trace=muse%2Fresponses", wantURI: "/inference/openai/v1/responses?trace=muse%2Fresponses", body: `{"model":"muse-spark-1.3","input":"hi","tools":[{"type":"function","name":"bash","parameters":{"type":"string"}},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`, wantNames: []string{"edit", "glob", "grep", "read", "write", "bash", "lookup"}, wantChoice: "required"},
		{name: "exact inference OpenAI family", upstreamPath: "/inference/openai", path: "/v1/chat/completions?trace=exact%2Ffamily", wantURI: "/inference/openai/v1/chat/completions?trace=exact%2Ffamily", body: `{"model":"gpt-4o","messages":[],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, wantNested: true, wantNames: []string{"edit", "glob", "grep", "read", "write", "lookup", "bash"}},
		{name: "inference Anthropic Responses", upstreamPath: "/inference/anthropic", path: "/v1/responses", wantURI: "/inference/anthropic/v1/responses", body: `{"model":"muse-spark-1.3","input":"hi","tools":[{"type":"function","name":"bash","parameters":{"type":"string"}},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`, wantNames: []string{"edit", "glob", "grep", "read", "write", "bash", "lookup"}, wantChoice: "required"},
		{name: "inference Google Chat", upstreamPath: "/inference/google", path: "/v1/chat/completions", wantURI: "/inference/google/v1/chat/completions", body: `{"model":"gpt-4o","messages":[],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, wantNested: true, wantNames: []string{"edit", "glob", "grep", "read", "write", "lookup", "bash"}},
		{name: "unrelated legacy path keeps client tools", upstreamPath: "/zen/v1", path: "/chat/completions", wantURI: "/zen/v1/chat/completions", body: `{"model":"gpt-4o","messages":[],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, wantNested: true, wantNames: []string{"lookup"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamURI string
			var upstreamBody map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamURI = r.URL.RequestURI()
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				upstreamBody = decodeJSON(t, payload)
				if strings.Contains(r.URL.Path, "responses") {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, sampleResponsesSSE(t))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			request := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "text/event-stream")
			w := httptest.NewRecorder()
			proxyHandler(upstream.URL+tt.upstreamPath, upstream.Client(), &idMint{}).ServeHTTP(w, request)
			if w.Code != http.StatusOK {
				t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
			}
			if upstreamURI != tt.wantURI {
				t.Fatalf("upstream request URI = %q, want %q", upstreamURI, tt.wantURI)
			}
			tools := array(t, upstreamBody["tools"])
			if len(tools) != len(tt.wantNames) {
				t.Fatalf("upstream tools count = %d, want %d (%#v)", len(tools), len(tt.wantNames), tools)
			}
			for i, value := range tools {
				tool := object(t, value)
				if tt.wantNested {
					tool = object(t, tool["function"])
				}
				if tool["name"] != tt.wantNames[i] {
					t.Errorf("tool %d name = %#v, want %q", i, tool["name"], tt.wantNames[i])
				}
			}
			if upstreamBody["tool_choice"] != tt.wantChoice {
				t.Errorf("tool_choice = %#v, want %#v", upstreamBody["tool_choice"], tt.wantChoice)
			}
		})
	}
}

func TestBuildResponsesRequestRepairsToolPairing(t *testing.T) {
	chat := map[string]any{"model": "muse-spark", "messages": []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "paired", "type": "function", "function": map[string]any{"name": "one"}},
			map[string]any{"id": "dangling-call", "type": "function", "function": map[string]any{"name": "two"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "paired", "content": "answer"},
		map[string]any{"role": "tool", "tool_call_id": "dangling-output", "content": "orphan"},
		map[string]any{"role": "assistant", "content": "kept text", "tool_calls": []any{
			map[string]any{"id": "no-reply", "type": "function", "function": map[string]any{"name": "three"}},
		}},
	}}
	input := array(t, buildResponsesRequest(chat)["input"])
	if len(input) != 4 {
		t.Fatalf("repaired input has %d entries, want 4: %#v", len(input), input)
	}
	if object(t, input[1])["type"] != "function_call" || object(t, input[1])["call_id"] != "paired" {
		t.Fatalf("paired call missing or reordered: %#v", input[1])
	}
	if object(t, input[2])["type"] != "function_call_output" || object(t, input[2])["call_id"] != "paired" {
		t.Fatalf("paired tool reply missing or reordered: %#v", input[2])
	}
	if object(t, input[3])["type"] != "message" || object(t, array(t, object(t, input[3])["content"])[0])["text"] != "kept text" {
		t.Fatalf("assistant text should survive without its dangling call: %#v", input[3])
	}
	for _, value := range input {
		entry := object(t, value)
		if entry["call_id"] == "dangling-call" || entry["call_id"] == "dangling-output" || entry["call_id"] == "no-reply" {
			t.Fatalf("dangling tool entry survived pairing repair: %#v", entry)
		}
	}
}

func TestNormalizeAndClassifyModels(t *testing.T) {
	museModels := []string{
		"muse-spark-1.2-contributor-free",
		"muse-spark-1.3",
		"muse-spark-1.3-contributor-free",
		"muse-spark-1.2",
	}
	for _, model := range museModels {
		for _, candidate := range []string{model, strings.ToUpper(model), "vendor/" + model + " (high)"} {
			if !isMuseModel(candidate) {
				t.Errorf("isMuseModel(%q) = false, want true", candidate)
			}
		}
	}
	jevModels := []string{"jev-1.13-free"}
	for _, model := range jevModels {
		for _, candidate := range []string{model, strings.ToUpper(model), "vendor/" + model + " (high)"} {
			if !isJevModel(candidate) {
				t.Errorf("isJevModel(%q) = false, want true", candidate)
			}
			if isMuseModel(candidate) {
				t.Errorf("isMuseModel(%q) = true, want false", candidate)
			}
		}
	}
	if isMuseModel("big-pickle") || isJevModel("big-pickle") {
		t.Fatal("big-pickle must remain on the generic wire")
	}
	if isMuseModel("gpt-4o") || isJevModel("gpt-4o") {
		t.Fatal("unrelated model was classified as Muse or Jev")
	}
}

func TestWriteResponsesAsChatFixture(t *testing.T) {
	w := httptest.NewRecorder()
	status, retryable := writeResponsesAsChat(w, responses(sampleResponsesSSE(t)), "muse-spark-1.3", true, &responseReplayState{}, toolCallFilter{})
	if retryable || status != http.StatusOK || w.Code != http.StatusOK {
		t.Fatalf("status = %d, recorder = %d, want 200", status, w.Code)
	}
	body := w.Body.String()
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream did not terminate with [DONE]: %q", body)
	}
	payloads := ssePayloads(t, body)
	if len(payloads) < 3 {
		t.Fatalf("got only %d SSE payloads", len(payloads))
	}
	firstChoice := object(t, array(t, payloads[0]["choices"])[0])
	if object(t, firstChoice["delta"])["role"] != "assistant" {
		t.Fatalf("first chunk is not the assistant role prelude: %#v", payloads[0])
	}
	var sawText, sawStop, sawUsage bool
	for _, payload := range payloads {
		choice := object(t, array(t, payload["choices"])[0])
		delta := object(t, choice["delta"])
		if delta["content"] == "OK" {
			sawText = true
		}
		if choice["finish_reason"] == "stop" {
			sawStop = true
			usage := object(t, payload["usage"])
			sawUsage = usage["prompt_tokens"] == float64(652) && usage["completion_tokens"] == float64(27) && usage["total_tokens"] == float64(679)
		}
	}
	if !sawText || !sawStop || !sawUsage {
		t.Fatalf("fixture stream missing text/stop/usage: %s", body)
	}
}

func TestWriteResponsesAsChatFunctionCallsAndStableIndices(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"resp_calls","created_at":123}}`, "",
		`data: {"type":"response.output_item.added","item":{"id":"item-a","type":"function_call","call_id":"call-a","name":"alpha","arguments":""}}`, "",
		`data: {"type":"response.output_item.added","item":{"id":"item-b","type":"function_call","call_id":"call-b","name":"beta","arguments":""}}`, "",
		`data: {"type":"response.function_call_arguments.delta","item_id":"item-a","delta":"{\"a\":"}`, "",
		`data: {"type":"response.function_call_arguments.delta","item_id":"item-b","delta":"{}"}`, "",
		`data: {"type":"response.completed","response":{"id":"resp_calls","created_at":123,"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`, "",
	}, "\n")
	w := httptest.NewRecorder()
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", true, &responseReplayState{}, toolCallFilter{})
	if retryable || status != http.StatusOK || w.Code != http.StatusOK {
		t.Fatalf("status = %d, recorder = %d", status, w.Code)
	}
	payloads := ssePayloads(t, w.Body.String())
	starts := make(map[string]float64)
	arguments := make(map[float64]string)
	var sawFinish bool
	for _, payload := range payloads {
		choice := object(t, array(t, payload["choices"])[0])
		delta := object(t, choice["delta"])
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, callValue := range calls {
				call := object(t, callValue)
				index := call["index"].(float64)
				if id, ok := call["id"].(string); ok {
					starts[id] = index
				}
				if function, ok := call["function"].(map[string]any); ok {
					if value, ok := function["arguments"].(string); ok {
						arguments[index] += value
					}
				}
			}
		}
		if choice["finish_reason"] == "tool_calls" {
			sawFinish = true
			if starts["call-a"] != 0 || starts["call-b"] != 1 {
				t.Fatalf("tool indices = %#v, want call-a=0 call-b=1", starts)
			}
		}
	}
	if !sawFinish || arguments[0] != `{"a":` || arguments[1] != "{}" {
		t.Fatalf("tool chunks missing finish or argument deltas; starts=%#v arguments=%#v body=%s", starts, arguments, w.Body.String())
	}
}

func TestWriteResponsesAsChatFailure(t *testing.T) {
	stream := "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"upstream exploded\"}}}\n\n"
	w := httptest.NewRecorder()
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", true, &responseReplayState{}, toolCallFilter{})
	if retryable || status != http.StatusOK || w.Code != http.StatusOK {
		t.Fatalf("streaming failure status = %d, recorder = %d", status, w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"message":"upstream exploded"`) || !strings.Contains(body, `"type":"upstream_error"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("failure stream = %q", body)
	}
}

func TestWriteResponsesAsChatNonStreaming(t *testing.T) {
	w := httptest.NewRecorder()
	status, retryable := writeResponsesAsChat(w, responses(sampleResponsesSSE(t)), "client-model", false, &responseReplayState{}, toolCallFilter{})
	if retryable || status != http.StatusOK || w.Code != http.StatusOK {
		t.Fatalf("status = %d, recorder = %d", status, w.Code)
	}
	completion := decodeJSON(t, w.Body.Bytes())
	if completion["object"] != "chat.completion" || completion["model"] != "client-model" {
		t.Fatalf("completion metadata = %#v", completion)
	}
	choice := object(t, array(t, completion["choices"])[0])
	message := object(t, choice["message"])
	if message["role"] != "assistant" || message["content"] != "OK" || choice["finish_reason"] != "stop" {
		t.Fatalf("assembled choice = %#v", choice)
	}
	usage := object(t, completion["usage"])
	if usage["prompt_tokens"] != float64(652) || usage["completion_tokens"] != float64(27) || usage["total_tokens"] != float64(679) {
		t.Fatalf("assembled usage = %#v", usage)
	}
}

func TestWriteResponsesAsChatNonStreamingToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"item-1","type":"function_call","call_id":"call-1","name":"lookup","arguments":""}}`, "",
		`data: {"type":"response.function_call_arguments.delta","item_id":"item-1","delta":"{\"q\":1}"}`, "",
		`data: {"type":"response.completed","response":{"id":"resp_tool","created_at":123,"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`, "",
	}, "\n")
	w := httptest.NewRecorder()
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", false, &responseReplayState{}, toolCallFilter{})
	if retryable || status != http.StatusOK || w.Code != http.StatusOK {
		t.Fatalf("status = %d, recorder = %d", status, w.Code)
	}
	completion := decodeJSON(t, w.Body.Bytes())
	choice := object(t, array(t, completion["choices"])[0])
	calls := array(t, object(t, choice["message"])["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	call := object(t, calls[0])
	function := object(t, call["function"])
	if call["id"] != "call-1" || call["type"] != "function" || function["name"] != "lookup" || function["arguments"] != `{"q":1}` {
		t.Fatalf("assembled tool call = %#v", call)
	}
	if _, exists := call["index"]; exists {
		t.Fatalf("non-stream tool call contains streaming index: %#v", call)
	}
}

func TestProxyHandlerMuseRoutingAndJevRejection(t *testing.T) {
	t.Run("Muse rewrites path and sends flat request", func(t *testing.T) {
		fixture := sampleResponsesSSE(t)
		var upstreamPath, upstreamBody string
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamPath = r.URL.RequestURI()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			upstreamBody = string(body)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, fixture)
		}))
		defer upstream.Close()

		requestBody := `{"model":"vendor/MUSE-SPARK-1.3-CONTRIBUTOR-FREE (high)","stream":true,"messages":[{"role":"system","content":"instructions"},{"role":"user","content":"hello"}]}`
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=1", strings.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "text/event-stream")
		w := httptest.NewRecorder()
		proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)

		if upstreamPath != "/v1/responses?trace=1" {
			t.Fatalf("upstream request URI = %q", upstreamPath)
		}
		converted := decodeJSON(t, []byte(upstreamBody))
		if converted["stream"] != true || converted["instructions"] != "instructions" {
			t.Fatalf("upstream body missing Responses fields: %#v", converted)
		}
		assertHasToolNames(t, converted["tools"], "bash", "edit", "glob", "grep", "read", "write")
		if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatalf("client response was not chat SSE ending in [DONE]: %q", w.Body.String())
		}
	})

	t.Run("Jev returns exact error without upstream", func(t *testing.T) {
		upstreamHits := 0
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHits++
			_, _ = io.WriteString(w, `unexpected`)
		}))
		defer upstream.Close()

		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"vendor/JEV-1.13-FREE (high)","messages":[]}`))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
		if w.Code != http.StatusBadRequest || w.Body.String() != `{"error":{"message":"model requires the systemone wire, not supported by oc-zen"}}` {
			t.Fatalf("Jev response = %d %q", w.Code, w.Body.String())
		}
		if upstreamHits != 0 {
			t.Fatalf("upstream received %d requests", upstreamHits)
		}
	})
}

func TestProxyHandlerUnrelatedModelPassesThrough(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.RequestURI()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		upstreamBody = decodeJSON(t, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?x=2", strings.NewReader(`{"model":"gpt-4o","stream":false,"messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if upstreamPath != "/v1/chat/completions?x=2" {
		t.Fatalf("unrelated model path = %q", upstreamPath)
	}
	if upstreamBody["model"] != "gpt-4o" || upstreamBody["stream"] != true {
		t.Fatalf("chat request should force stream true upstream: %#v", upstreamBody)
	}
	assertHasNestedToolNames(t, upstreamBody["tools"], "bash", "edit", "glob", "grep", "read", "write")
	if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Fatalf("pass-through response = %d %q", w.Code, w.Body.String())
	}
}

func TestProxyHandlerMuseEndToEnd(t *testing.T) {
	fixture := sampleResponsesSSE(t)
	var upstreamPath string
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		upstreamRequest = decodeJSON(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fixture)
	}))
	defer upstream.Close()

	requestBody := map[string]any{
		"model": "muse-spark-1.3", "stream": true,
		"messages": []any{map[string]any{"role": "system", "content": "be concise"}, map[string]any{"role": "user", "content": "say OK"}},
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)

	if upstreamPath != "/v1/responses" {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	if upstreamRequest["stream"] != true || upstreamRequest["instructions"] != "be concise" {
		t.Fatalf("upstream Responses request = %#v", upstreamRequest)
	}
	assertHasFlatToolNames(t, upstreamRequest["tools"], "bash", "edit", "glob", "grep", "read", "write")
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("client response headers = %d %v", w.Code, w.Header())
	}
	payloads := ssePayloads(t, w.Body.String())
	var sawRole, sawOK bool
	for _, payload := range payloads {
		choice := object(t, array(t, payload["choices"])[0])
		delta := object(t, choice["delta"])
		if delta["role"] == "assistant" {
			sawRole = true
		}
		if delta["content"] == "OK" {
			sawOK = true
		}
	}
	if !sawRole || !sawOK || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("client chat stream missing role/text/terminal marker: %s", w.Body.String())
	}
}

func TestProxyHandlerMuseNonStreaming(t *testing.T) {
	fixture := sampleResponsesSSE(t)
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		upstreamRequest = decodeJSON(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fixture)
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"muse-spark-1.3","stream":false,"messages":[{"role":"user","content":"say OK"}]}`))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)

	if upstreamRequest["stream"] != true {
		t.Fatalf("upstream stream = %#v, want true", upstreamRequest["stream"])
	}
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("client response = %d %v", w.Code, w.Header())
	}
	completion := decodeJSON(t, w.Body.Bytes())
	if completion["model"] != "muse-spark-1.3" {
		t.Fatalf("client model = %#v", completion["model"])
	}
	choice := object(t, array(t, completion["choices"])[0])
	message := object(t, choice["message"])
	if message["content"] != "OK" || choice["finish_reason"] != "stop" {
		t.Fatalf("assembled client completion = %#v", choice)
	}
}

func TestProxyHandlerInjectedToolCallsAreFiltered(t *testing.T) {
	for _, responsesWire := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			name := "chat"
			path := "/v1/chat/completions"
			model := "gpt-4o"
			upstreamEvent := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: [DONE]\n\n"
			if responsesWire {
				name = "responses"
				path = "/v1/responses"
				model = "muse-spark-1.3"
				upstreamEvent = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_filter\",\"created_at\":123,\"output\":[]}}\n\n" +
					"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"item-1\",\"call_id\":\"call-1\",\"name\":\"bash\",\"arguments\":\"{}\"}}\n\n" +
					"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item-1\",\"delta\":\"{}\"}\n\n" +
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_filter\",\"created_at\":123,\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"item-1\",\"call_id\":\"call-1\",\"name\":\"bash\",\"arguments\":\"{}\"},{\"type\":\"message\",\"id\":\"msg-1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n" +
					"data: [DONE]\n\n"
			}
			t.Run(name+"/stream="+strconv.FormatBool(streaming), func(t *testing.T) {
				setStabilityTestKnobs(t)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, upstreamEvent)
				}))
				defer upstream.Close()
				body := `{"model":"` + model + `","stream":` + strconv.FormatBool(streaming) + `,"messages":[{"role":"user","content":"run it"}]}`
				if responsesWire {
					body = `{"model":"` + model + `","stream":` + strconv.FormatBool(streaming) + `,"input":"run it"}`
				}
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				if streaming {
					request.Header.Set("Accept", "text/event-stream")
				}
				w := httptest.NewRecorder()
				proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
				if w.Code != http.StatusOK {
					t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
				}
				if responsesWire {
					var payloads []map[string]any
					if streaming {
						payloads = ssePayloads(t, w.Body.String())
						if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
							t.Fatalf("Responses stream lacks terminal marker: %q", w.Body.String())
						}
					} else {
						payloads = []map[string]any{decodeJSON(t, w.Body.Bytes())}
					}
					if strings.Contains(w.Body.String(), "function_call") {
						t.Fatalf("injected Responses tool call leaked: %q", w.Body.String())
					}
					last := payloads[len(payloads)-1]
					response, ok := last["response"].(map[string]any)
					if !ok {
						response = last
					}
					output := array(t, response["output"])
					if len(output) != 1 || object(t, output[0])["type"] != "message" {
						t.Fatalf("Responses output after filtering = %#v", output)
					}
					return
				}
				var completion map[string]any
				if streaming {
					for _, payload := range ssePayloads(t, w.Body.String()) {
						if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
							completion = payload
						}
					}
					if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
						t.Fatalf("chat stream lacks terminal marker: %q", w.Body.String())
					}
				} else {
					completion = decodeJSON(t, w.Body.Bytes())
				}
				choice := object(t, array(t, completion["choices"])[0])
				if choice["finish_reason"] != "stop" {
					t.Fatalf("finish_reason = %#v, want stop; completion=%#v", choice["finish_reason"], completion)
				}
				messageKey := "message"
				if streaming {
					messageKey = "delta"
				}
				message := object(t, choice[messageKey])
				if _, exists := message["tool_calls"]; exists {
					t.Fatalf("injected chat tool call leaked: %#v", message)
				}
			})
		}
	}

	t.Run("caller tool calls remain", func(t *testing.T) {
		setStabilityTestKnobs(t)
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer upstream.Close()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":false,"messages":[],"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}]}`))
		request.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
		completion := decodeJSON(t, w.Body.Bytes())
		choice := object(t, array(t, completion["choices"])[0])
		calls := array(t, object(t, choice["message"])["tool_calls"])
		if len(calls) != 1 || choice["finish_reason"] != "tool_calls" {
			t.Fatalf("caller tool call was modified: %#v", choice)
		}
		function := object(t, object(t, calls[0])["function"])
		if function["name"] != "bash" {
			t.Fatalf("caller tool call = %#v, want caller-supplied bash", function)
		}
	})

	t.Run("normalized caller calls remain", func(t *testing.T) {
		for _, muse := range []bool{false, true} {
			for _, streaming := range []bool{false, true} {
				model := "gpt-4o"
				if muse {
					model = "muse-spark-1.3"
				}
				t.Run(model+"/stream="+strconv.FormatBool(streaming), func(t *testing.T) {
					assertNormalizedCallerHTTP(t, false, muse, streaming)
				})
			}
		}
	})
}

func TestProxyHandlerPreservesNormalizedResponsesCoreCalls(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(streaming), func(t *testing.T) {
			assertNormalizedCallerHTTP(t, true, true, streaming)
		})
	}
}

func TestProxyHandlerPreservesNormalizedChatBackedResponsesCoreCalls(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run("stream="+strconv.FormatBool(streaming), func(t *testing.T) {
			assertNormalizedCallerHTTP(t, true, false, streaming)
		})
	}
}

func normalizedCallerSSE(t *testing.T, responsesWire bool) string {
	t.Helper()
	var fixture strings.Builder
	emit := func(payload map[string]any) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		fixture.WriteString("data: " + string(encoded) + "\n\n")
	}
	if responsesWire {
		emit(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_visibility", "object": "response", "created_at": 123, "status": "in_progress", "output": []any{}}})
	}
	var output []any
	for i, call := range []struct{ id, name, arguments string }{
		{"caller-bash", "bash", `{"command":"printf 'caller-visible\\n'"}`},
		{"proxy-read", "read", `{"filePath":"/tmp/proxy-only"}`},
		{"caller-lookup", "lookup", `{"q":"127*43"}`},
	} {
		itemID := "item-" + call.id
		item := map[string]any{"type": "function_call", "id": itemID, "call_id": call.id, "name": call.name, "arguments": "", "status": "in_progress"}
		if responsesWire {
			emit(map[string]any{"type": "response.output_item.added", "output_index": i, "item": cloneToolValue(item)})
		} else {
			emit(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "id": call.id, "type": "function", "function": map[string]any{"name": call.name, "arguments": ""}}}}}}})
		}
		for _, fragment := range []string{call.arguments[:len(call.arguments)/2], call.arguments[len(call.arguments)/2:]} {
			if responsesWire {
				emit(map[string]any{"type": "response.function_call_arguments.delta", "output_index": i, "item_id": itemID, "delta": fragment})
			} else {
				emit(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": i, "function": map[string]any{"arguments": fragment}}}}}}})
			}
		}
		item["arguments"], item["status"] = call.arguments, "completed"
		output = append(output, item)
		if responsesWire {
			emit(map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": itemID, "arguments": call.arguments})
			emit(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	}
	if responsesWire {
		emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_visibility", "object": "response", "created_at": 123, "status": "completed", "output": output}})
	} else {
		emit(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	}
	fixture.WriteString("data: [DONE]\n\n")
	return fixture.String()
}

func assertNormalizedCallerHTTP(t *testing.T, responsesWire, muse, streaming bool) {
	t.Helper()
	setStabilityTestKnobs(t)
	t.Setenv("ZEN_RELAY_URL", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	fixture := normalizedCallerSSE(t, muse)
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		upstreamBody = decodeJSON(t, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, fixture)
	}))
	defer upstream.Close()
	path, model := "/v1/chat/completions", "gpt-4o"
	if muse {
		model = "muse-spark-1.3"
	}
	body := map[string]any{"model": model, "stream": streaming, "messages": []any{}, "tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "string"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}},
	}}
	if responsesWire {
		path = "/v1/responses"
		body["input"] = "run"
		delete(body, "messages")
		body["tools"] = []any{map[string]any{"type": "function", "name": "bash", "parameters": map[string]any{"type": "string"}}, map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if streaming {
		request.Header.Set("Accept", "text/event-stream")
	}
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference/openai", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	wantPath := "/inference/openai/v1/chat/completions"
	if muse {
		wantPath = "/inference/openai/v1/responses"
	}
	if upstreamPath != wantPath || upstreamBody["stream"] != true {
		t.Fatalf("upstream path=%q body=%#v", upstreamPath, upstreamBody)
	}
	if muse {
		assertHasFlatToolNames(t, upstreamBody["tools"], "bash", "lookup", "read")
	} else {
		assertHasNestedToolNames(t, upstreamBody["tools"], "bash", "lookup", "read")
	}
	t.Logf("local fixture upstream path=%s SSE=%s", upstreamPath, fixture)
	t.Logf("local fixture client stream=%t status=%d body=%s", streaming, w.Code, w.Body.String())
	if w.Code != http.StatusOK {
		t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
	}
	assertNormalizedCallerOutput(t, w.Body.String(), responsesWire, streaming)
}

func assertNormalizedCallerOutput(t *testing.T, body string, responsesWire, streaming bool) {
	t.Helper()
	for _, hidden := range []string{"proxy-read", "proxy-only", "filePath", `"name":"read"`} {
		if strings.Contains(body, hidden) {
			t.Errorf("proxy-only call leaked %q: %s", hidden, body)
		}
	}
	var payloads []map[string]any
	if streaming {
		payloads = ssePayloads(t, body)
		if !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("client stream lacks terminal marker: %s", body)
		}
	} else {
		payloads = []map[string]any{decodeJSON(t, []byte(body))}
	}
	wantIDs := []string{"caller-bash", "caller-lookup"}
	wantNames := []string{"bash", "lookup"}
	wantArguments := []string{`{"command":"printf 'caller-visible\\n'"}`, `{"q":"127*43"}`}
	var calls []any
	if responsesWire {
		last := payloads[len(payloads)-1]
		response := last
		if streaming {
			if last["type"] != "response.completed" {
				t.Fatalf("terminal event=%#v, want response.completed", last)
			}
			response = object(t, last["response"])
			var addedIDs []string
			var doneIDs []string
			itemCalls := map[string]string{}
			arguments := map[string]string{}
			for _, payload := range payloads {
				switch payload["type"] {
				case "response.output_item.added":
					item := object(t, payload["item"])
					if item["type"] == "function_call" {
						id := stringValue(item["call_id"])
						addedIDs = append(addedIDs, id)
						itemCalls[stringValue(item["id"])] = id
					}
				case "response.function_call_arguments.delta":
					id, ok := itemCalls[stringValue(payload["item_id"])]
					if !ok {
						t.Fatalf("arguments event without visible added item: %#v", payload)
					}
					arguments[id] += stringValue(payload["delta"])
				case "response.function_call_arguments.done":
					id := itemCalls[stringValue(payload["item_id"])]
					if id == "" || arguments[id] != stringValue(payload["arguments"]) {
						t.Errorf("arguments done differs from deltas: %#v, assembled=%q", payload, arguments[id])
					}
				case "response.output_item.done":
					item := object(t, payload["item"])
					if item["type"] == "function_call" {
						id := stringValue(item["call_id"])
						doneIDs = append(doneIDs, id)
						if itemCalls[stringValue(item["id"])] != id || arguments[id] != stringValue(item["arguments"]) {
							t.Errorf("done item differs from visible added item/deltas: %#v", item)
						}
					}
				}
			}
			if !reflect.DeepEqual(addedIDs, wantIDs) {
				t.Errorf("visible added call order=%v, want %v", addedIDs, wantIDs)
			}
			if !reflect.DeepEqual(doneIDs, wantIDs) {
				t.Errorf("visible done call order=%v, want %v", doneIDs, wantIDs)
			}
			for i, id := range wantIDs {
				if arguments[id] != wantArguments[i] {
					t.Errorf("streamed %s arguments=%q, want %q", id, arguments[id], wantArguments[i])
				}
			}
		}
		if response["status"] != "completed" {
			t.Errorf("response status=%#v, want completed", response["status"])
		}
		calls = array(t, response["output"])
	} else {
		if streaming {
			indices := map[any]int{}
			for _, payload := range payloads {
				choice := object(t, array(t, payload["choices"])[0])
				delta := object(t, choice["delta"])
				fragments, _ := delta["tool_calls"].([]any)
				for _, raw := range fragments {
					fragment := object(t, raw)
					index := fragment["index"]
					position, exists := indices[index]
					if !exists {
						position = len(calls)
						indices[index] = position
						calls = append(calls, map[string]any{"id": "", "function": map[string]any{"name": "", "arguments": ""}})
					}
					call := object(t, calls[position])
					function := object(t, call["function"])
					part := object(t, fragment["function"])
					if id := stringValue(fragment["id"]); id != "" {
						if call["id"] != "" && call["id"] != id {
							t.Fatalf("call index %v changed ID from %v to %s", index, call["id"], id)
						}
						call["id"] = id
					}
					function["name"] = stringValue(function["name"]) + stringValue(part["name"])
					function["arguments"] = stringValue(function["arguments"]) + stringValue(part["arguments"])
				}
			}
		} else {
			choice := object(t, array(t, payloads[0]["choices"])[0])
			calls = array(t, object(t, choice["message"])["tool_calls"])
		}
		lastChoice := object(t, array(t, payloads[len(payloads)-1]["choices"])[0])
		if lastChoice["finish_reason"] != "tool_calls" {
			t.Errorf("finish_reason=%#v, want tool_calls", lastChoice["finish_reason"])
		}
	}
	if len(calls) != len(wantIDs) {
		t.Fatalf("visible calls=%#v, want bash then lookup", calls)
	}
	for i, raw := range calls {
		call := object(t, raw)
		id, function := call["call_id"], call
		if !responsesWire {
			id, function = call["id"], object(t, call["function"])
		}
		if id != wantIDs[i] || function["name"] != wantNames[i] || function["arguments"] != wantArguments[i] {
			t.Errorf("call[%d]=%#v, want id=%s name=%s arguments=%s", i, call, wantIDs[i], wantNames[i], wantArguments[i])
		}
	}
}
func TestFilterToolCallsInJSON(t *testing.T) {
	for _, injectedName := range []string{"bash", "edit", "glob", "grep", "read", "write"} {
		t.Run(injectedName, func(t *testing.T) {
			filter := toolCallFilter{injected: map[string]bool{injectedName: true}}
			chat := []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"function":{"name":"` + injectedName + `"}},{"function":{"name":"lookup"}}]}}]}`)
			filtered, changed := filterToolCallsInJSON(chat, filter)
			if !changed {
				t.Fatal("chat response was not filtered")
			}
			chatResponse := decodeJSON(t, filtered)
			chatChoice := object(t, array(t, chatResponse["choices"])[0])
			chatCalls := array(t, object(t, chatChoice["message"])["tool_calls"])
			if len(chatCalls) != 1 || stringValue(object(t, object(t, chatCalls[0])["function"])["name"]) != "lookup" || chatChoice["finish_reason"] != "tool_calls" {
				t.Fatalf("filtered chat response = %#v", chatChoice)
			}

			responses := []byte(`{"output":[{"type":"function_call","name":"` + injectedName + `"},{"type":"function_call","name":"lookup"},{"type":"message","content":[]}]}`)
			filtered, changed = filterToolCallsInJSON(responses, filter)
			if !changed {
				t.Fatal("Responses payload was not filtered")
			}
			responsesPayload := decodeJSON(t, filtered)
			output := array(t, responsesPayload["output"])
			if len(output) != 2 || object(t, output[0])["name"] != "lookup" || object(t, output[1])["type"] != "message" {
				t.Fatalf("filtered Responses output = %#v", output)
			}
		})
	}
}

func TestFilterToolCallsInJSONKeepable(t *testing.T) {
	keep := toolCallFilter{offered: map[string]bool{"lookup": true}}
	if !keep.enabled() {
		t.Fatal("offered filter should be enabled")
	}
	chat := []byte(`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"function":{"name":"lookup"}},{"function":{"name":"mystery"}}]}}]}`)
	filtered, changed := filterToolCallsInJSON(chat, keep)
	if !changed {
		t.Fatal("chat response was not filtered")
	}
	choice := object(t, array(t, decodeJSON(t, filtered)["choices"])[0])
	calls := array(t, object(t, choice["message"])["tool_calls"])
	if len(calls) != 1 || stringValue(object(t, object(t, calls[0])["function"])["name"]) != "lookup" {
		t.Fatalf("keepable chat output = %#v", choice)
	}

	responses := []byte(`{"output":[{"type":"function_call","name":"lookup"},{"type":"function_call","name":"mystery"},{"type":"message","content":[]}]}`)
	filtered, changed = filterToolCallsInJSON(responses, keep)
	if !changed {
		t.Fatal("Responses payload was not filtered")
	}
	output := array(t, decodeJSON(t, filtered)["output"])
	if len(output) != 2 || object(t, output[0])["name"] != "lookup" || object(t, output[1])["type"] != "message" {
		t.Fatalf("keepable Responses output = %#v", output)
	}

	legacy := toolCallFilter{injected: map[string]bool{"read": true}}
	if legacy.hides("mystery") || !legacy.hides("read") {
		t.Fatalf("legacy hides(mystery)=%v hides(read)=%v, want false/true", legacy.hides("mystery"), legacy.hides("read"))
	}

	var empty toolCallFilter
	if empty.enabled() {
		t.Fatal("empty filter should be disabled")
	}
	if out, changed := filterToolCallsInJSON(chat, empty); changed || string(out) != string(chat) {
		t.Fatalf("empty filter changed payload: changed=%v", changed)
	}
}

func TestProxyHandlerDropsNonOfferedReplyTools(t *testing.T) {
	setStabilityTestKnobs(t)
	t.Setenv("ZEN_RELAY_URL", "")
	t.Setenv("OPENCODE_GO_API_KEY", "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_lookup","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_mystery","type":"function","function":{"name":"mystery","arguments":"{}"}}]}}]}`)
	}))
	defer upstream.Close()
	body := map[string]any{
		"model":    "gpt-4o",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{"type": "string"}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}},
		},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference/openai", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("proxy response = %d %q", w.Code, w.Body.String())
	}
	choice := object(t, array(t, decodeJSON(t, w.Body.Bytes())["choices"])[0])
	calls := array(t, object(t, choice["message"])["tool_calls"])
	if len(calls) != 1 || stringValue(object(t, object(t, calls[0])["function"])["name"]) != "lookup" {
		t.Fatalf("reply tool_calls = %#v, want only offered lookup", calls)
	}
}

func assertOnlyFlatTool(t *testing.T, value any, wantName string) {
	t.Helper()
	tools := array(t, value)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one", tools)
	}
	tool := object(t, tools[0])
	if tool["name"] != wantName {
		t.Fatalf("tool name = %#v, want %q", tool["name"], wantName)
	}
}

func assertOnlyNestedTool(t *testing.T, value any, wantName string) {
	t.Helper()
	tools := array(t, value)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v, want exactly one", tools)
	}
	tool := object(t, object(t, tools[0])["function"])
	if tool["name"] != wantName {
		t.Fatalf("tool name = %#v, want %q", tool["name"], wantName)
	}
}

func assertHasFlatToolNames(t *testing.T, value any, want ...string) {
	t.Helper()
	assertToolNames(t, value, false, want)
}

func assertHasNestedToolNames(t *testing.T, value any, want ...string) {
	t.Helper()
	assertToolNames(t, value, true, want)
}

func assertHasToolNames(t *testing.T, value any, want ...string) {
	t.Helper()
	assertToolNames(t, value, false, want)
}

func assertToolNames(t *testing.T, value any, nested bool, want []string) {
	t.Helper()
	tools := array(t, value)
	got := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool := object(t, raw)
		if nested {
			tool = object(t, tool["function"])
		}
		got[stringValue(tool["name"])] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("tools = %#v, missing %q", tools, name)
		}
	}
}

func findToolByName(t *testing.T, value any, name string, nested bool) map[string]any {
	t.Helper()
	for _, raw := range array(t, value) {
		tool := object(t, raw)
		probe := tool
		if nested {
			probe = object(t, tool["function"])
		}
		if stringValue(probe["name"]) == name {
			return tool
		}
	}
	return nil
}
