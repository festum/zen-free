package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	if _, exists := got["tool_choice"]; exists {
		t.Fatalf("unsupported named tool_choice was retained: %#v", got["tool_choice"])
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
	tests := []struct {
		name          string
		tools         []any
		choice        any
		wantChoice    any
		wantPresent   bool
		wantToolCount int
	}{
		{name: "missing quartet and omitted choice", wantToolCount: 4},
		{
			name: "all four declared and choice omitted",
			tools: []any{
				map[string]any{"type": "function", "name": "bash"}, map[string]any{"type": "function", "name": "glob"},
				map[string]any{"type": "function", "name": "grep"}, map[string]any{"type": "function", "name": "read"},
			},
			wantToolCount: 4,
		},
		{name: "auto choice", choice: "auto", wantChoice: "auto", wantPresent: true, wantToolCount: 4},
		{name: "none choice omitted", choice: "none", wantToolCount: 4},
		{name: "required choice omitted", choice: "required", wantToolCount: 4},
		{name: "case-variant auto omitted", choice: "AUTO", wantToolCount: 4},
		{name: "empty choice omitted", choice: "", wantToolCount: 4},
		{name: "named choice omitted", choice: map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}, wantToolCount: 4},
		{name: "non-string choice omitted", choice: true, wantToolCount: 4},
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
			tools := array(t, got["tools"])
			if len(tools) != tt.wantToolCount {
				t.Fatalf("got %d tools, want %d", len(tools), tt.wantToolCount)
			}
			seen := make(map[string]int)
			for _, value := range tools {
				tool := object(t, value)
				if tool["type"] != "function" || tool["name"] == nil || tool["function"] != nil {
					t.Fatalf("tool is not in flat function format: %#v", tool)
				}
				seen[tool["name"].(string)]++
			}
			for _, name := range []string{"bash", "glob", "grep", "read"} {
				if seen[name] != 1 {
					t.Fatalf("tool %q appears %d times", name, seen[name])
				}
			}
			choice, exists := got["tool_choice"]
			if exists != tt.wantPresent || choice != tt.wantChoice {
				t.Fatalf("tool_choice = %#v, present=%t; want %#v, present=%t", choice, exists, tt.wantChoice, tt.wantPresent)
			}
		})
	}
}

func TestProxyHandlerResponsesToolChoiceSanitization(t *testing.T) {
	quartet := `[{"type":"function","name":"bash"},{"type":"function","name":"glob"},{"type":"function","name":"grep"},{"type":"function","name":"read"}]`
	tests := []struct {
		name         string
		path         string
		body         string
		wantChoice   any
		wantPresent  bool
		wantInjected bool
	}{
		{
			name:         "omitted choice with decoys",
			path:         "/v1/chat/completions",
			body:         `{"model":"muse-spark-1.3","messages":[]}`,
			wantInjected: true,
		},
		{
			name:        "explicit auto",
			path:        "/v1/chat/completions",
			body:        `{"model":"muse-spark-1.3","messages":[],"tool_choice":"auto"}`,
			wantChoice:  "auto",
			wantPresent: true,
		},
		{
			name: "named choice omitted",
			path: "/v1/chat/completions",
			body: `{"model":"muse-spark-1.3","messages":[],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
		},
		{
			name:        "direct responses auto retained with complete quartet",
			path:        "/v1/responses",
			body:        `{"model":"other","tools":` + quartet + `,"tool_choice":"auto"}`,
			wantChoice:  "auto",
			wantPresent: true,
		},
		{
			name: "direct responses none removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":"none"}`,
		},
		{
			name: "direct responses required removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":"required"}`,
		},
		{
			name: "direct responses named choice removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
		},
		{
			name: "direct responses non-string choice removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":true}`,
		},
		{
			name: "direct responses null choice removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":null}`,
		},
		{
			name: "direct responses case-variant auto removed with complete quartet",
			path: "/v1/responses",
			body: `{"model":"other","tools":` + quartet + `,"tool_choice":"AUTO"}`,
		},
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
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			request := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			request.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
			choice, exists := upstreamBody["tool_choice"]
			if exists != tt.wantPresent || choice != tt.wantChoice {
				t.Fatalf("upstream tool_choice = %#v, present=%t; want %#v, present=%t", choice, exists, tt.wantChoice, tt.wantPresent)
			}
			if tt.wantInjected {
				assertFlatQuartet(t, upstreamBody["tools"])
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
	status, retryable := writeResponsesAsChat(w, responses(sampleResponsesSSE(t)), "muse-spark-1.3", true, &responseReplayState{})
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
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", true, &responseReplayState{})
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
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", true, &responseReplayState{})
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
	status, retryable := writeResponsesAsChat(w, responses(sampleResponsesSSE(t)), "client-model", false, &responseReplayState{})
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
	status, retryable := writeResponsesAsChat(w, responses(stream), "muse-spark", false, &responseReplayState{})
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
		assertFlatQuartet(t, converted["tools"])
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
	if upstreamBody["model"] != "gpt-4o" || upstreamBody["stream"] != false {
		t.Fatalf("pass-through body changed unexpectedly: %#v", upstreamBody)
	}
	tools := array(t, upstreamBody["tools"])
	if len(tools) != 4 || object(t, tools[0])["function"] == nil {
		t.Fatalf("ordinary chat pass-through should retain nested quartet augmentation: %#v", tools)
	}
	if upstreamBody["tool_choice"] != "none" {
		t.Fatalf("chat wire tool_choice = %#v, want none", upstreamBody["tool_choice"])
	}
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
	assertFlatQuartet(t, upstreamRequest["tools"])
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

func assertFlatQuartet(t *testing.T, value any) {
	t.Helper()
	tools := array(t, value)
	seen := make(map[string]int)
	for _, item := range tools {
		tool := object(t, item)
		if tool["type"] != "function" || tool["function"] != nil {
			t.Fatalf("Responses tool is not flat: %#v", tool)
		}
		if name, ok := tool["name"].(string); ok {
			seen[name]++
		}
	}
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if seen[name] != 1 {
			t.Fatalf("flat tool %q appears %d times", name, seen[name])
		}
	}
}
