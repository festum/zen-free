package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func eventTypes(t *testing.T, body string) []string {
	t.Helper()
	var types []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			types = append(types, strings.TrimPrefix(line, "event: "))
		}
	}
	return types
}

func assertSameResponseID(t *testing.T, payloads []map[string]any) {
	t.Helper()
	var id string
	for _, payload := range payloads {
		value, exists := payload["response"]
		if !exists {
			continue
		}
		responseID := object(t, value)["id"].(string)
		if !strings.HasPrefix(responseID, "resp_") {
			t.Fatalf("response id %q lacks resp_ prefix", responseID)
		}
		if id == "" {
			id = responseID
		} else if responseID != id {
			t.Fatalf("response id changed mid-stream: %q vs %q", responseID, id)
		}
	}
	if id == "" {
		t.Fatal("no response payload observed")
	}
}

func TestBuildChatCompletionsRequest(t *testing.T) {
	responses := map[string]any{
		"model":             "longcat-2.5-preview-free",
		"stream":            true,
		"instructions":      "be brief",
		"temperature":       0.3,
		"max_output_tokens": 128,
		"top_p":             0.9,
		"store":             true,
		"tool_choice":       "auto",
		"tools": []any{
			map[string]any{"type": "function", "name": "lookup", "description": "look up", "parameters": map[string]any{"type": "object"}},
			map[string]any{"type": "computer_preview", "name": "click"},
		},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi"},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "checking"}}},
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": `{"q":"x"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "found"},
			map[string]any{"type": "reasoning", "summary": []any{}},
			map[string]any{"type": "message", "role": "user", "content": "hi"},
		},
	}
	chat := buildChatCompletionsRequest(responses)
	if chat["model"] != "longcat-2.5-preview-free" || chat["stream"] != true {
		t.Fatalf("model/stream = (%v, %v)", chat["model"], chat["stream"])
	}
	if chat["temperature"] != 0.3 || chat["max_tokens"] != 128 {
		t.Fatalf("sampling fields = %#v", chat)
	}
	for _, key := range []string{"top_p", "store", "instructions", "max_output_tokens"} {
		if _, exists := chat[key]; exists {
			t.Fatalf("Responses-only key %q was forwarded", key)
		}
	}
	messages := array(t, chat["messages"])
	if len(messages) != 5 {
		t.Fatalf("messages = %#v, want system + user + assistant + tool + user", messages)
	}
	system := object(t, messages[0])
	if system["role"] != "system" || system["content"] != "be brief" {
		t.Fatalf("system message = %#v", system)
	}
	user := object(t, messages[1])
	if user["role"] != "user" || user["content"] != "hi" {
		t.Fatalf("user message = %#v", user)
	}
	assistant := object(t, messages[2])
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	parts := array(t, assistant["content"])
	if part := object(t, parts[0]); part["type"] != "text" || part["text"] != "checking" {
		t.Fatalf("content part = %#v", part)
	}
	calls := array(t, assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", calls)
	}
	call := object(t, calls[0])
	if call["id"] != "call-1" || call["type"] != "function" {
		t.Fatalf("call = %#v", call)
	}
	function := object(t, call["function"])
	if function["name"] != "lookup" || function["arguments"] != `{"q":"x"}` {
		t.Fatalf("function = %#v", function)
	}
	toolReply := object(t, messages[3])
	if toolReply["role"] != "tool" || toolReply["tool_call_id"] != "call-1" || toolReply["content"] != "found" {
		t.Fatalf("tool reply = %#v", toolReply)
	}
	after := object(t, messages[4])
	if after["role"] != "user" || after["content"] != "hi" {
		t.Fatalf("message after reasoning item = %#v", after)
	}
	tools := array(t, chat["tools"])
	nested := object(t, tools[0])
	nestedFunction := object(t, nested["function"])
	if nested["type"] != "function" || nestedFunction["name"] != "lookup" || nestedFunction["description"] != "look up" {
		t.Fatalf("nested tool = %#v", nested)
	}
	if passthrough := object(t, tools[1]); passthrough["name"] != "click" || passthrough["type"] != "computer_preview" {
		t.Fatalf("non-function tool = %#v", passthrough)
	}
	if chat["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v", chat["tool_choice"])
	}
}

func TestBuildChatCompletionsRequestTable(t *testing.T) {
	tests := []struct {
		name         string
		responses    map[string]any
		wantChoice   any
		wantChoiceIn bool
		wantMessages func(t *testing.T, messages []any)
	}{
		{
			name:      "string input",
			responses: map[string]any{"input": "hello"},
			wantMessages: func(t *testing.T, messages []any) {
				if len(messages) != 1 {
					t.Fatalf("messages = %#v", messages)
				}
				user := object(t, messages[0])
				if user["role"] != "user" || user["content"] != "hello" {
					t.Fatalf("user = %#v", user)
				}
			},
		},
		{
			name:      "missing input keeps empty messages array",
			responses: map[string]any{},
			wantMessages: func(t *testing.T, messages []any) {
				if messages == nil || len(messages) != 0 {
					t.Fatalf("messages = %#v, want empty array", messages)
				}
			},
		},
		{
			name: "object tool_choice nested",
			responses: map[string]any{
				"tool_choice": map[string]any{"type": "function", "name": "lookup"},
			},
			wantChoice:   map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}},
			wantChoiceIn: true,
		},
		{
			name:         "absent tool_choice omitted",
			responses:    map[string]any{},
			wantChoiceIn: false,
		},
		{
			name: "consecutive function_calls merge into one assistant message",
			responses: map[string]any{"input": []any{
				map[string]any{"type": "function_call", "call_id": "c1", "name": "a", "arguments": "{}"},
				map[string]any{"type": "function_call", "call_id": "c2", "name": "b", "arguments": "{}"},
			}},
			wantMessages: func(t *testing.T, messages []any) {
				if len(messages) != 1 {
					t.Fatalf("messages = %#v, want single merged assistant", messages)
				}
				assistant := object(t, messages[0])
				if assistant["role"] != "assistant" {
					t.Fatalf("assistant = %#v", assistant)
				}
				calls := array(t, assistant["tool_calls"])
				if len(calls) != 2 {
					t.Fatalf("tool_calls = %#v", calls)
				}
				first, second := object(t, calls[0]), object(t, calls[1])
				if first["id"] != "c1" || second["id"] != "c2" {
					t.Fatalf("call order = %#v", calls)
				}
			},
		},
		{
			name: "image part mapped",
			responses: map[string]any{"input": []any{
				map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": "look"},
					map[string]any{"type": "input_image", "image_url": "https://example.invalid/i.png"},
					map[string]any{"type": "input_audio", "data": "drop"},
				}},
			}},
			wantMessages: func(t *testing.T, messages []any) {
				parts := array(t, object(t, messages[0])["content"])
				if len(parts) != 2 {
					t.Fatalf("parts = %#v, want text + image", parts)
				}
				if part := object(t, parts[0]); part["type"] != "text" || part["text"] != "look" {
					t.Fatalf("text part = %#v", part)
				}
				if part := object(t, parts[1]); part["type"] != "image_url" || part["image_url"] != "https://example.invalid/i.png" {
					t.Fatalf("image part = %#v", part)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chat := buildChatCompletionsRequest(tt.responses)
			choice, exists := chat["tool_choice"]
			if exists != tt.wantChoiceIn || !reflect.DeepEqual(choice, tt.wantChoice) {
				t.Fatalf("tool_choice = %#v present=%t, want %#v present=%t", choice, exists, tt.wantChoice, tt.wantChoiceIn)
			}
			if tt.wantMessages != nil {
				messages, ok := chat["messages"].([]any)
				if !ok {
					t.Fatalf("messages = %#v", chat["messages"])
				}
				tt.wantMessages(t, messages)
			}
		})
	}
}

func TestWriteChatStreamAsResponsesEventOrder(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"content":"Hel"}}]}

data: {"choices":[{"delta":{"content":"lo"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: {"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6,"prompt_tokens_details":{"cached_tokens":1}}}

data: [DONE]

`
	recorder := httptest.NewRecorder()
	status, retryable := writeChatStreamAsResponses(recorder, strings.NewReader(upstream), "longcat-2.5-preview-free", toolCallFilter{})
	if status != http.StatusOK || retryable {
		t.Fatalf("status/retryable = (%d, %t)", status, retryable)
	}
	body := recorder.Body.String()
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	gotOrder := eventTypes(t, body)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	payloads := ssePayloads(t, body)
	if len(payloads) != len(wantOrder) {
		t.Fatalf("payload count = %d, want %d", len(payloads), len(wantOrder))
	}
	for i, payload := range payloads {
		if payload["sequence_number"] != float64(i) {
			t.Fatalf("payload %d sequence = %v, want %d", i, payload["sequence_number"], i)
		}
	}
	assertSameResponseID(t, payloads)
	completed := object(t, payloads[len(payloads)-1]["response"])
	if completed["status"] != "completed" || completed["model"] != "longcat-2.5-preview-free" {
		t.Fatalf("completed response = %#v", completed)
	}
	usage := object(t, completed["usage"])
	if usage["input_tokens"] != float64(4) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(6) {
		t.Fatalf("usage = %#v", usage)
	}
	cached := object(t, usage["input_tokens_details"])
	if cached["cached_tokens"] != float64(1) {
		t.Fatalf("input_tokens_details = %#v", cached)
	}
	output := array(t, completed["output"])
	message := object(t, output[0])
	if message["type"] != "message" || message["status"] != "completed" {
		t.Fatalf("message item = %#v", message)
	}
	part := object(t, array(t, message["content"])[0])
	if part["type"] != "output_text" || part["text"] != "Hello" {
		t.Fatalf("text part = %#v", part)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("stream does not end with DONE: %q", body[len(body)-40:])
	}
}

func TestWriteChatStreamAsResponsesFunctionCalls(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call-b","function":{"name":"other","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	recorder := httptest.NewRecorder()
	status, retryable := writeChatStreamAsResponses(recorder, strings.NewReader(upstream), "longcat-2.5-preview-free", toolCallFilter{})
	if status != http.StatusOK || retryable {
		t.Fatalf("status/retryable = (%d, %t)", status, retryable)
	}
	body := recorder.Body.String()
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	gotOrder := eventTypes(t, body)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	payloads := ssePayloads(t, body)
	assertSameResponseID(t, payloads)
	for i, payload := range payloads {
		if payload["sequence_number"] != float64(i) {
			t.Fatalf("payload %d sequence = %v", i, payload["sequence_number"])
		}
	}
	completed := object(t, payloads[len(payloads)-1]["response"])
	output := array(t, completed["output"])
	if len(output) != 3 {
		t.Fatalf("output = %#v, want message + 2 function calls", output)
	}
	first := object(t, output[1])
	if first["type"] != "function_call" || first["call_id"] != "call-a" || first["name"] != "lookup" || first["arguments"] != `{"q":1}` {
		t.Fatalf("first call = %#v", first)
	}
	second := object(t, output[2])
	if second["type"] != "function_call" || second["call_id"] != "call-b" || second["arguments"] != "{}" {
		t.Fatalf("second call = %#v", second)
	}
}

func TestChatCompletionToResponseShape(t *testing.T) {
	chat := map[string]any{
		"id":      "chatcmpl-1",
		"created": float64(1700000000),
		"model":   "longcat-2.5-preview-free",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": "done",
				"tool_calls": []any{map[string]any{
					"id":       "call-1",
					"type":     "function",
					"function": map[string]any{"name": "lookup", "arguments": `{"q":"x"}`},
				}},
			},
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3, "total_tokens": 10},
	}
	response := chatCompletionToResponse(chat)
	id := response["id"].(string)
	if !strings.HasPrefix(id, "resp_") || len(id) != len("resp_")+24 {
		t.Fatalf("id = %q", id)
	}
	if response["object"] != "response" || response["status"] != "completed" || response["model"] != "longcat-2.5-preview-free" {
		t.Fatalf("response = %#v", response)
	}
	if response["created_at"] != int64(1700000000) {
		t.Fatalf("created_at = %#v", response["created_at"])
	}
	output := array(t, response["output"])
	if len(output) != 2 {
		t.Fatalf("output = %#v", output)
	}
	message := object(t, output[0])
	if message["type"] != "message" || message["role"] != "assistant" || message["status"] != "completed" {
		t.Fatalf("message = %#v", message)
	}
	part := object(t, array(t, message["content"])[0])
	if part["type"] != "output_text" || part["text"] != "done" {
		t.Fatalf("part = %#v", part)
	}
	call := object(t, output[1])
	if call["type"] != "function_call" || call["call_id"] != "call-1" || call["name"] != "lookup" || call["arguments"] != `{"q":"x"}` {
		t.Fatalf("call = %#v", call)
	}
	usage := object(t, response["usage"])
	if usage["input_tokens"] != 7 || usage["output_tokens"] != 3 || usage["total_tokens"] != 10 {
		t.Fatalf("usage = %#v", usage)
	}
}

func chatSSEFixture() string {
	return "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"BENCH\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n" +
		"data: [DONE]\n\n"
}

func responsesTestRequest(path, body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestProxyHandlerResponsesChatOnlyStreaming(t *testing.T) {
	setStabilityTestKnobs(t)
	var mu sync.Mutex
	var paths []string
	var upstreamStream any
	var upstreamChatBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		decoded := decodeJSON(t, body)
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		upstreamChatBody = decoded
		upstreamStream = decoded["stream"]
		mu.Unlock()
		if r.URL.Path != "/inference/openai/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSEFixture())
	}))
	defer upstream.Close()

	request := responsesTestRequest("/v1/responses?trace=stream%2F1", `{"model":"longcat-2.5-preview-free","stream":true,"input":"Reply with exactly: BENCH"}`)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("client content type = %q", got)
	}
	mu.Lock()
	if len(paths) != 1 || paths[0] != "/inference/openai/v1/chat/completions?trace=stream%2F1" {
		t.Fatalf("upstream paths = %v, want mapped Chat route with query preserved", paths)
	}
	if upstreamStream != true {
		t.Fatalf("upstream stream = %#v, want true", upstreamStream)
	}
	mu.Unlock()
	assertHasNestedToolNames(t, upstreamChatBody["tools"], "bash", "edit", "glob", "grep", "read", "write")
	body := w.Body.String()
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	gotOrder := eventTypes(t, body)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	payloads := ssePayloads(t, body)
	assertSameResponseID(t, payloads)
	completed := object(t, payloads[len(payloads)-1]["response"])
	output := array(t, completed["output"])
	message := object(t, output[0])
	part := object(t, array(t, message["content"])[0])
	if part["text"] != "BENCH" {
		t.Fatalf("final text = %#v", part)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("client stream lacks DONE terminator")
	}
}

func TestProxyHandlerResponsesChatOnlyNonStreaming(t *testing.T) {
	setStabilityTestKnobs(t)
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		decoded := decodeJSON(t, body)
		if decoded["stream"] != true {
			t.Errorf("upstream stream = %#v, want forced true", decoded["stream"])
		}
		if r.URL.Path != "/inference/openai/v1/chat/completions" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, chatSSEFixture())
	}))
	defer upstream.Close()

	request := responsesTestRequest("/v1/responses?trace=nonstream%2F1", `{"model":"longcat-2.5-preview-free","stream":false,"input":"Reply with exactly: BENCH"}`)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("client content type = %q", got)
	}
	mu.Lock()
	if len(paths) != 1 || paths[0] != "/inference/openai/v1/chat/completions?trace=nonstream%2F1" {
		t.Fatalf("upstream paths = %v, want mapped Chat route with query preserved", paths)
	}
	mu.Unlock()
	response := decodeJSON(t, w.Body.Bytes())
	if response["object"] != "response" || response["status"] != "completed" {
		t.Fatalf("response = %#v", response)
	}
	if !strings.HasPrefix(response["id"].(string), "resp_") {
		t.Fatalf("id = %#v", response["id"])
	}
	usage := object(t, response["usage"])
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(1) || usage["total_tokens"] != float64(6) {
		t.Fatalf("usage = %#v", usage)
	}
	output := array(t, response["output"])
	message := object(t, output[0])
	part := object(t, array(t, message["content"])[0])
	if part["text"] != "BENCH" {
		t.Fatalf("output = %#v", output)
	}
}

func TestProxyHandlerResponsesProtocolFallback(t *testing.T) {
	setStabilityTestKnobs(t)
	var paths []string
	var mu sync.Mutex
	var chatBody, nativeBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		decoded := decodeJSON(t, body)
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/inference/openai/v1/chat/completions":
			chatBody = decoded
		case "/inference/openai/v1/responses":
			nativeBody = decoded
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/inference/openai/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported"}}`)
		case "/inference/openai/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_native00000000000000000000","object":"response","status":"in_progress","output":[]}}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_native00000000000000000000","object":"response","status":"completed","output":[{"type":"message","id":"msg_native","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	request := responsesTestRequest("/v1/responses?trace=fallback%2F1", `{"model":"longcat-2.5-preview-free","stream":true,"input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	mu.Lock()
	seen := strings.Join(paths, ",")
	mu.Unlock()
	wantPaths := "/inference/openai/v1/chat/completions?trace=fallback%2F1,/inference/openai/v1/responses?trace=fallback%2F1"
	if seen != wantPaths {
		t.Fatalf("upstream paths = %q, want mapped chat-first then native Responses with query preserved", seen)
	}
	wantChatTools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	wantNativeTools := []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
	if !reflect.DeepEqual(findToolByName(t, chatBody["tools"], "lookup", true), wantChatTools[0]) || !reflect.DeepEqual(findToolByName(t, nativeBody["tools"], "lookup", false), wantNativeTools[0]) {
		t.Fatalf("caller tools changed across fallback: chat=%#v native=%#v", chatBody["tools"], nativeBody["tools"])
	}
	assertHasNestedToolNames(t, chatBody["tools"], "lookup")
	assertHasFlatToolNames(t, nativeBody["tools"], "lookup")
	if chatBody["tool_choice"] != "required" {
		t.Fatalf("chat bridge tool_choice = %#v, want caller choice required", chatBody["tool_choice"])
	}
	if nativeBody["tool_choice"] != "required" {
		t.Fatalf("native fallback tool_choice = %#v, want caller choice required", nativeBody["tool_choice"])
	}
	body := w.Body.String()
	if !strings.Contains(body, "response.completed") {
		t.Fatalf("client did not receive native Responses SSE: %q", body)
	}
	payloads := ssePayloads(t, body)
	last := payloads[len(payloads)-1]
	response := object(t, last["response"])
	output := array(t, response["output"])
	message := object(t, output[0])
	part := object(t, array(t, message["content"])[0])
	if part["text"] != "native" {
		t.Fatalf("native output = %#v", output)
	}
}

func TestProxyHandlerPreservesNormalizedSchemasWithoutInjectedTools(t *testing.T) {
	for _, mode := range []string{"native Muse", "protocol fallback", "default non-Chat inference"} {
		t.Run(mode, func(t *testing.T) {
			setStabilityTestKnobs(t)
			t.Setenv("ZEN_RELAY_URL", "")
			t.Setenv("OPENCODE_GO_API_KEY", "")
			var mu sync.Mutex
			var paths []string
			var bodies []map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				decoded := decodeJSON(t, raw)
				mu.Lock()
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, decoded)
				mu.Unlock()
				if mode == "default non-Chat inference" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"ok":true}`)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_schema\",\"object\":\"response\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"item-caller-bash\",\"call_id\":\"caller-bash\",\"name\":\"bash\",\"arguments\":\"\"}}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"item-caller-bash\",\"delta\":\"{\\\"command\\\":\\\"printf caller-visible\\\"}\"}\n\n")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_schema\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"item-caller-bash\",\"call_id\":\"caller-bash\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"printf caller-visible\\\"}\"}]}}\n\n")
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()
			model, path := "muse-spark-1.3", "/v1/responses"
			if mode == "protocol fallback" {
				model = "longcat-2.5-preview-free"
			}
			body := map[string]any{"model": model, "stream": true, "input": "run", "tool_choice": "required"}
			var tools []any
			for _, name := range []string{"bash", "glob", "grep", "read"} {
				function := map[string]any{"name": name, "parameters": map[string]any{"type": "string"}}
				tool := map[string]any{"type": "function", "name": name, "parameters": function["parameters"]}
				if mode == "default non-Chat inference" {
					tool = map[string]any{"type": "function", "function": function}
				}
				tools = append(tools, tool)
			}
			body["tools"] = tools
			if mode == "default non-Chat inference" {
				path = "/v1/completions"
			}
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			request := responsesTestRequest(path, string(encoded))
			w := httptest.NewRecorder()
			proxyHandler(upstream.URL+"/inference/openai", upstream.Client(), &idMint{}).ServeHTTP(w, request)
			mu.Lock()
			defer mu.Unlock()
			wantPaths := []string{"/inference/openai/v1/responses"}
			if mode == "protocol fallback" {
				wantPaths = []string{"/inference/openai/v1/chat/completions", "/inference/openai/v1/responses"}
			} else if mode == "default non-Chat inference" {
				wantPaths = []string{"/inference/openai/v1/completions"}
			}
			if !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("upstream paths=%v, want %v", paths, wantPaths)
			}
			for i, upstreamBody := range bodies {
				t.Logf("local fixture normalized request path=%s body=%#v", paths[i], upstreamBody)
				if upstreamBody["tool_choice"] != "required" || upstreamBody["stream"] != true {
					t.Errorf("upstream request lost explicit choice or streaming: %#v", upstreamBody)
				}
				got := array(t, upstreamBody["tools"])
				if len(got) != 6 {
					t.Fatalf("v2 tool list count = %d, want 6 (5 bridged manifest + bash): %#v", len(got), got)
				}
				nested := strings.HasSuffix(paths[i], "/v1/chat/completions") || mode == "default non-Chat inference"
				wantOrder := []string{"edit", "glob", "grep", "read", "write", "bash"}
				for j, raw := range got {
					tool := object(t, raw)
					probe := tool
					if nested {
						probe = object(t, tool["function"])
					}
					if name := stringValue(probe["name"]); name != wantOrder[j] {
						t.Fatalf("%s tool[%d] = %s, want %s", paths[i], j, name, wantOrder[j])
					}
					var canonicalFlat any
					if j < len(bridgedManifestTools) {
						canonicalFlat = wireTool(bridgedManifestTools[j], false)
					} else {
						canonicalFlat = capturedTool("bash", false)
					}
					wantTool := object(t, canonicalFlat)
					for _, field := range []string{"description", "parameters"} {
						gotJSON, err := json.Marshal(probe[field])
						if err != nil {
							t.Fatal(err)
						}
						wantJSON, err := json.Marshal(wantTool[field])
						if err != nil {
							t.Fatal(err)
						}
						if string(gotJSON) != string(wantJSON) {
							t.Errorf("%s tool[%d] %s=%s, want canonical %s %s", paths[i], j, field, gotJSON, wantOrder[j], field)
						}
					}
				}
			}
			t.Logf("local fixture schema client status=%d body=%s", w.Code, w.Body.String())
			if w.Code != http.StatusOK {
				t.Fatalf("client response=%d %s", w.Code, w.Body.String())
			}
			if mode == "default non-Chat inference" {
				return
			}
			if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("client stream lacks terminal marker: %s", w.Body.String())
			}
			payloads := ssePayloads(t, w.Body.String())
			last := payloads[len(payloads)-1]
			if last["type"] != "response.completed" {
				t.Fatalf("terminal event=%#v", last)
			}
			response := object(t, last["response"])
			output := array(t, response["output"])
			if response["status"] != "completed" || len(output) != 1 {
				t.Fatalf("completed caller output=%#v", response)
			}
			call := object(t, output[0])
			if call["type"] != "function_call" || call["name"] != "bash" || call["call_id"] != "caller-bash" || call["arguments"] != `{"command":"printf caller-visible"}` {
				t.Errorf("normalized caller bash was changed or hidden: %#v", call)
			}
		})
	}
}

func TestProxyHandlerResponsesMuseStaysNative(t *testing.T) {
	setStabilityTestKnobs(t)
	var mu sync.Mutex
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"muse"}}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	request := responsesTestRequest("/v1/responses?trace=muse%2Fnative", `{"model":"muse-spark-1.3-contributor-free","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/inference/openai/v1/responses?trace=muse%2Fnative" {
		t.Fatalf("upstream paths = %v, want single mapped native OpenAI Responses route with query preserved", paths)
	}
}

func TestProxyHandlerResponsesToolRoundTrip(t *testing.T) {
	setStabilityTestKnobs(t)
	inbound := `{"model":"longcat-2.5-preview-free","stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"input":[` +
		`{"type":"message","role":"user","content":"weather in Paris?"},` +
		`{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"city\":\"Paris\"}"},` +
		`{"type":"function_call_output","call_id":"call-1","output":"sunny"}]}`
	var mu sync.Mutex
	var upstreamBody map[string]any
	var upstreamURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamURI = r.URL.RequestURI()
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = decodeJSON(t, raw)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-2","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	request := responsesTestRequest("/v1/responses", inbound)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	if upstreamURI != "/inference/openai/v1/chat/completions" {
		t.Fatalf("upstream request URI = %q, want mapped Chat route", upstreamURI)
	}
	mu.Lock()
	if upstreamBody == nil {
		t.Fatal("upstream saw no body")
	}
	messages := array(t, upstreamBody["messages"])
	mu.Unlock()
	if len(messages) != 4 {
		t.Fatalf("upstream messages = %#v, want system(preamble) + user + assistant(tool_calls) + tool", messages)
	}
	system := object(t, messages[0])
	if system["role"] != "system" || !strings.HasPrefix(stringValue(system["content"]), preambleMarker) {
		t.Fatalf("system = %#v", system)
	}
	user := object(t, messages[1])
	if user["role"] != "user" {
		t.Fatalf("user = %#v", user)
	}
	assistant := object(t, messages[2])
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant = %#v", assistant)
	}
	calls := array(t, assistant["tool_calls"])
	call := object(t, calls[0])
	function := object(t, call["function"])
	if call["id"] != "call-1" || function["name"] != "lookup" || function["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("assistant call = %#v", call)
	}
	toolReply := object(t, messages[3])
	if toolReply["role"] != "tool" || toolReply["tool_call_id"] != "call-1" || toolReply["content"] != "sunny" {
		t.Fatalf("tool reply = %#v", toolReply)
	}
	body := w.Body.String()
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	gotOrder := eventTypes(t, body)
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("event order = %v, want %v", gotOrder, wantOrder)
	}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("event[%d] = %q, want %q (full: %v)", i, gotOrder[i], wantOrder[i], gotOrder)
		}
	}
	payloads := ssePayloads(t, body)
	completed := object(t, payloads[len(payloads)-1]["response"])
	output := array(t, completed["output"])
	if len(output) != 1 {
		t.Fatalf("output = %#v", output)
	}
	converted := object(t, output[0])
	if converted["type"] != "function_call" || converted["call_id"] != "call-2" || converted["name"] != "lookup" || converted["arguments"] != `{"q":1}` {
		t.Fatalf("converted call = %#v", converted)
	}
}

func deepSeekReasoningItem(text string) map[string]any {
	return map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": text}}}
}

func assertDeepSeekReasoningItem(t *testing.T, item map[string]any, text, status string) {
	t.Helper()
	id, _ := item["id"].(string)
	if item["type"] != "reasoning" || !strings.HasPrefix(id, "rs_") || item["status"] != status {
		t.Fatalf("reasoning item = %#v", item)
	}
	if summary := array(t, item["summary"]); len(summary) != 0 {
		t.Fatalf("reasoning summary = %#v, want empty", summary)
	}
	parts := array(t, item["content"])
	if len(parts) != 1 || !reflect.DeepEqual(object(t, parts[0]), map[string]any{"type": "reasoning_text", "text": text}) {
		t.Fatalf("reasoning content = %#v, want %q", parts, text)
	}
}

func TestDeepSeekReasoningInboundAssociations(t *testing.T) {
	message := func(role, text string) map[string]any {
		return map[string]any{"type": "message", "role": role, "content": text}
	}
	chatMessage := func(role, text, reasoning string) map[string]any {
		m := map[string]any{"role": role, "content": text}
		if reasoning != "" {
			m["reasoning_content"] = reasoning
		}
		return m
	}
	call := func(id string) map[string]any {
		return map[string]any{"type": "function_call", "call_id": id, "name": "lookup", "arguments": `{"q":"雪"}`}
	}
	assistantCalls := func(reasoning string, ids ...string) map[string]any {
		m := chatMessage("assistant", "", reasoning)
		calls := []any{}
		for _, id := range ids {
			calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"雪"}`}})
		}
		m["tool_calls"] = calls
		return m
	}
	toolOutput := map[string]any{"type": "function_call_output", "call_id": "c1", "output": "found"}
	toolMessage := map[string]any{"role": "tool", "tool_call_id": "c1", "content": "found"}
	r := deepSeekReasoningItem(" 雪\n")
	tests := []struct {
		name  string
		input []any
		want  []any
	}{
		{"pending assistant", []any{r, message("assistant", "A")}, []any{chatMessage("assistant", "A", " 雪\n")}},
		{"preceding assistant", []any{message("assistant", "A"), r}, []any{chatMessage("assistant", "A", " 雪\n")}},
		{"ambiguous adjacency keeps separate assistants", []any{message("assistant", "A"), r, message("assistant", "B")}, []any{chatMessage("assistant", "A", " 雪\n"), chatMessage("assistant", "B", "")}},
		{"multiple pending items", []any{r, deepSeekReasoningItem("二"), message("assistant", "A")}, []any{chatMessage("assistant", "A", " 雪\n二")}},
		{"multiple preceding items", []any{message("assistant", "A"), r, deepSeekReasoningItem("二")}, []any{chatMessage("assistant", "A", " 雪\n二")}},
		{"pending end", []any{r}, []any{chatMessage("assistant", "", " 雪\n")}},
		{"pending user boundary", []any{r, message("user", "U")}, []any{chatMessage("assistant", "", " 雪\n"), chatMessage("user", "U", "")}},
		{"pending system boundary", []any{r, message("system", "S")}, []any{chatMessage("assistant", "", " 雪\n"), chatMessage("system", "S", "")}},
		{"pending tool boundary", []any{r, toolOutput}, []any{chatMessage("assistant", "", " 雪\n"), toolMessage}},
		{"no search across user", []any{message("assistant", "A"), message("user", "U"), r}, []any{chatMessage("assistant", "A", ""), chatMessage("user", "U", ""), chatMessage("assistant", "", " 雪\n")}},
		{"no search across system", []any{message("assistant", "A"), message("system", "S"), r}, []any{chatMessage("assistant", "A", ""), chatMessage("system", "S", ""), chatMessage("assistant", "", " 雪\n")}},
		{"no search across tool", []any{call("c1"), toolOutput, r, message("assistant", "B")}, []any{assistantCalls("", "c1"), toolMessage, chatMessage("assistant", "B", " 雪\n")}},
		{"pending grouped calls", []any{r, call("c1"), call("c2")}, []any{assistantCalls(" 雪\n", "c1", "c2")}},
		{"reasoning after grouped calls", []any{call("c1"), call("c2"), r}, []any{assistantCalls(" 雪\n", "c1", "c2")}},
		{"reasoning between calls", []any{call("c1"), r, call("c2"), toolOutput}, []any{assistantCalls(" 雪\n", "c1", "c2"), toolMessage}},
		{"tool results separate call groups", []any{r, call("c1"), toolOutput, deepSeekReasoningItem("次"), call("c2"), map[string]any{"type": "function_call_output", "call_id": "c2", "output": "second"}}, []any{assistantCalls(" 雪\n", "c1"), toolMessage, assistantCalls("次", "c2"), map[string]any{"role": "tool", "tool_call_id": "c2", "content": "second"}}},
		{"empty skipped messages inert", []any{r, map[string]any{"type": "message", "role": "user", "content": []any{}}, map[string]any{"type": "message", "role": "system", "content": []any{map[string]any{"type": "unsupported"}}}, message("assistant", "A")}, []any{chatMessage("assistant", "A", " 雪\n")}},
		{"unknown items inert", []any{r, nil, "bad", map[string]any{"type": "unknown"}, message("assistant", "A")}, []any{chatMessage("assistant", "A", " 雪\n")}},
		{"existing assistant takes pending call", []any{r, message("assistant", "A"), call("c1")}, []any{map[string]any{"role": "assistant", "content": "A", "reasoning_content": " 雪\n", "tool_calls": assistantCalls("", "c1")["tool_calls"]}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildChatCompletionsRequest(map[string]any{"input": tt.input})["messages"]
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("messages = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDeepSeekReasoningInboundExtraction(t *testing.T) {
	tests := []struct {
		name string
		item map[string]any
		want string
	}{
		{"canonical ordered whitespace", map[string]any{"content": []any{map[string]any{"type": "reasoning_text", "text": " 雪\n"}, map[string]any{"type": "reasoning_text", "text": "二 "}}}, " 雪\n二 "},
		{"summary fallback", map[string]any{"summary": []any{map[string]any{"type": "summary_text", "text": "想\n"}, map[string]any{"type": "summary_text", "text": " 法"}}}, "想\n 法"},
		{"canonical summary deduplicated", map[string]any{"content": []any{map[string]any{"type": "reasoning_text", "text": "raw"}}, "summary": []any{map[string]any{"type": "summary_text", "text": "summary"}}}, "raw"},
		{"empty canonical falls back", map[string]any{"content": []any{map[string]any{"type": "reasoning_text", "text": ""}}, "summary": []any{map[string]any{"type": "summary_text", "text": "fallback"}}}, "fallback"},
		{"malformed canonical falls back", map[string]any{"content": "bad", "summary": []any{map[string]any{"type": "summary_text", "text": "fallback"}}}, "fallback"},
		{"ignore malformed and unsupported parts", map[string]any{"content": []any{nil, "bad", map[string]any{"type": "summary_text", "text": "drop"}, map[string]any{"type": "reasoning_text", "text": 42}, map[string]any{"type": "reasoning_text", "text": "keep"}}, "summary": []any{map[string]any{"type": "summary_text", "text": "drop"}}}, "keep"},
		{"summary ignores invalid parts", map[string]any{"summary": []any{nil, "bad", map[string]any{"type": "reasoning_text", "text": "drop"}, map[string]any{"type": "summary_text", "text": false}, map[string]any{"type": "summary_text", "text": "keep"}}}, "keep"},
		{"empty summary existing case", map[string]any{"summary": []any{}}, ""},
		{"empty canonical", map[string]any{"content": []any{}}, ""},
		{"missing fields", map[string]any{}, ""},
		{"malformed fields", map[string]any{"content": 42, "summary": "bad", "encrypted_content": "opaque"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.item["type"] = "reasoning"
			messages := array(t, buildChatCompletionsRequest(map[string]any{"input": []any{tt.item, map[string]any{"type": "message", "role": "assistant", "content": "answer"}}})["messages"])
			if len(messages) != 1 {
				t.Fatalf("messages = %#v", messages)
			}
			assistant := object(t, messages[0])
			if tt.want == "" {
				if _, exists := assistant["reasoning_content"]; exists {
					t.Fatalf("empty reasoning emitted: %#v", assistant)
				}
			} else if assistant["reasoning_content"] != tt.want {
				t.Fatalf("reasoning = %#v, want %q", assistant, tt.want)
			}
		})
	}
}

func TestDeepSeekReasoningNonstream(t *testing.T) {
	tests := []struct {
		name    string
		content string
		calls   []any
		finish  string
	}{
		{"text", "答案 雪\n二", nil, "stop"},
		{"reasoning only", "", nil, "stop"},
		{"caller tools", "查詢\n", []any{map[string]any{"id": "caller-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"雪"}`}}, map[string]any{"id": "caller-2", "type": "function", "function": map[string]any{"name": "lookup_next", "arguments": `{"q":"二"}`}}}, "tool_calls"},
		{"length", "partial", nil, "length"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := map[string]any{"role": "assistant", "content": tt.content, "reasoning_content": "思考 雪\n", "reasoning": "驗證 "}
			if tt.calls != nil {
				message["tool_calls"] = tt.calls
			}
			response := chatCompletionToResponse(map[string]any{"model": "deepseek-v4.1-flash-free", "choices": []any{map[string]any{"finish_reason": tt.finish, "message": message}}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 9, "total_tokens": 16, "completion_tokens_details": map[string]any{"reasoning_tokens": 6}}})
			output := array(t, response["output"])
			wantCount := 1 + len(tt.calls)
			if tt.content != "" {
				wantCount++
			}
			if len(output) != wantCount {
				t.Fatalf("output = %#v", output)
			}
			assertDeepSeekReasoningItem(t, object(t, output[0]), "思考 雪\n驗證 ", "completed")
			callIndex := 1
			if tt.content != "" {
				message := object(t, output[1])
				part := object(t, array(t, message["content"])[0])
				if message["type"] != "message" || part["type"] != "output_text" || part["text"] != tt.content {
					t.Fatalf("message = %#v", message)
				}
				callIndex++
			}
			for i, rawCall := range tt.calls {
				want := object(t, rawCall)
				function := object(t, want["function"])
				call := object(t, output[callIndex+i])
				if call["type"] != "function_call" || call["call_id"] != want["id"] || call["name"] != function["name"] || call["arguments"] != function["arguments"] {
					t.Fatalf("call[%d] = %#v, want %#v", i, call, want)
				}
			}
			usage := object(t, response["usage"])
			if object(t, usage["output_tokens_details"])["reasoning_tokens"] != 6 || usage["input_tokens"] != 7 || usage["output_tokens"] != 9 {
				t.Fatalf("usage = %#v", usage)
			}
			if tt.finish == "length" {
				if response["status"] != "incomplete" || object(t, response["incomplete_details"])["reason"] != "max_output_tokens" {
					t.Fatalf("length response = %#v", response)
				}
			} else if response["status"] != "completed" {
				t.Fatalf("status = %#v", response)
			}
		})
	}
}

func TestDeepSeekReasoningStreamLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		types  []string
		text   string
	}{
		{"fragmented aliases role prelude", []string{`{"role":"assistant"}`, `{"reasoning_content":"思考 雪\n"}`, `{"reasoning_content":"二","reasoning":" 三"}`, `{"reasoning":"\n驗證 "}`, `{"content":"答案\n雪"}`}, []string{"reasoning", "message"}, "思考 雪\n二 三\n驗證 "},
		{"reasoning only", []string{`{"reasoning_content":"只有\n","reasoning":"思考"}`}, []string{"reasoning"}, "只有\n思考"},
		{"alias only", []string{`{"reasoning":"別名\n"}`}, []string{"reasoning"}, "別名\n"},
		{"role only prelude with reasoning", []string{`{"role":"assistant"}`, `{"reasoning_content":"思考"}`}, []string{"reasoning", "message"}, "思考"},
		{"same chunk content tools reasoning", []string{`{"content":"答案\n雪","tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{\"q\":\"雪\"}"}}],"reasoning_content":"原始\n","reasoning":"別名"}`}, []string{"reasoning", "message", "function_call"}, "原始\n別名"},
		{"reasoning before tools keeps indexes", []string{`{"reasoning_content":"先"}`, `{"tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{\"q\":\"雪\"}"}}]}`, `{"content":"答案\n雪","reasoning":"後"}`}, []string{"reasoning", "message", "function_call"}, "先後"},
		{"content before late reasoning", []string{`{"content":"答案\n"}`, `{"reasoning_content":"遲來 雪\n"}`, `{"content":"雪"}`, `{"reasoning":"驗證 "}`}, []string{"reasoning", "message"}, "遲來 雪\n驗證 "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstream strings.Builder
			var wantTextDeltas []string
			var wantReasoningDeltas []string
			for _, chunk := range tt.chunks {
				upstream.WriteString("data: {\"choices\":[{\"delta\":" + chunk + "}]}\n\n")
				delta := decodeJSON(t, []byte(chunk))
				if text, _ := delta["content"].(string); text != "" {
					wantTextDeltas = append(wantTextDeltas, text)
				}
				for _, field := range []string{"reasoning_content", "reasoning"} {
					if text, _ := delta[field].(string); text != "" {
						wantReasoningDeltas = append(wantReasoningDeltas, text)
					}
				}
			}
			upstream.WriteString("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":5,\"total_tokens\":7,\"completion_tokens_details\":{\"reasoning_tokens\":4}}}\n\ndata: [DONE]\n\n")
			w := httptest.NewRecorder()
			status, retryable := writeChatStreamAsResponses(w, strings.NewReader(upstream.String()), "deepseek-v4.1-flash-free", toolCallFilter{})
			if status != http.StatusOK || retryable {
				t.Fatalf("status/retryable = %d/%t", status, retryable)
			}
			payloads := ssePayloads(t, w.Body.String())
			if strings.Count(w.Body.String(), "data: [DONE]\n\n") != 1 || !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") {
				t.Fatalf("stream terminal marker = %q", w.Body.String())
			}
			assertSameResponseID(t, payloads)
			completed := object(t, payloads[len(payloads)-1]["response"])
			output := array(t, completed["output"])
			if completed["status"] != "completed" || len(output) != len(tt.types) {
				t.Fatalf("completed = %#v", completed)
			}
			var reasoningID string
			var reasoningIndex int
			indexes := make(map[string]int)
			for i, typ := range tt.types {
				item := object(t, output[i])
				if item["type"] != typ {
					t.Fatalf("output[%d] = %#v, want %s", i, item, typ)
				}
				id, _ := item["id"].(string)
				if id == "" {
					t.Fatalf("output[%d] has no ID: %#v", i, item)
				}
				if _, exists := indexes[id]; exists {
					t.Fatalf("duplicate item ID %q: %#v", id, output)
				}
				indexes[id] = i
				if typ == "reasoning" {
					assertDeepSeekReasoningItem(t, item, tt.text, "completed")
					reasoningID = item["id"].(string)
					reasoningIndex = i
				}
				if typ == "function_call" && (item["call_id"] != "c1" || item["name"] != "lookup" || item["arguments"] != `{"q":"雪"}`) {
					t.Fatalf("call = %#v", item)
				}
				if typ == "message" {
					wantText := "答案\n雪"
					if tt.name == "role only prelude with reasoning" {
						wantText = ""
					}
					if object(t, array(t, item["content"])[0])["text"] != wantText {
						t.Fatalf("message = %#v", item)
					}
				}
			}
			var lifecycle []string
			var deltas strings.Builder
			var addedTypes, textDeltas, reasoningDeltas []string
			firstTextEvent := -1
			lastReasoningEvent := -1
			for i, payload := range payloads {
				if payload["sequence_number"] != float64(i) {
					t.Fatalf("sequence[%d] = %#v", i, payload)
				}
				typ, _ := payload["type"].(string)
				item, _ := payload["item"].(map[string]any)
				id, _ := payload["item_id"].(string)
				if item != nil {
					id, _ = item["id"].(string)
				}
				if id != "" {
					index, exists := indexes[id]
					if !exists || payload["output_index"] != float64(index) {
						t.Fatalf("unstable item ID/index: %#v output=%#v", payload, output)
					}
					if typ == "response.output_item.added" {
						if index != len(addedTypes) {
							t.Fatalf("added order differs from output: %#v", payload)
						}
						addedTypes = append(addedTypes, item["type"].(string))
					}
					if typ == "response.output_item.done" && !reflect.DeepEqual(item, output[index]) {
						t.Fatalf("done item differs from output[%d]: %#v", index, item)
					}
				}
				part, _ := payload["part"].(map[string]any)
				if firstTextEvent < 0 && (strings.HasPrefix(typ, "response.output_text.") || part["type"] == "output_text") {
					firstTextEvent = i
				}
				if typ == "response.output_text.delta" {
					textDeltas = append(textDeltas, payload["delta"].(string))
				}
				if payload["item_id"] != reasoningID && item["id"] != reasoningID {
					continue
				}
				lastReasoningEvent = i
				lifecycle = append(lifecycle, typ)
				if payload["output_index"] != float64(reasoningIndex) {
					t.Fatalf("reasoning index changed: %#v", payload)
				}
				switch typ {
				case "response.output_item.added":
					assertDeepSeekReasoningItem(t, item, "", "in_progress")
				case "response.content_part.added", "response.content_part.done":
					want := ""
					if typ == "response.content_part.done" {
						want = tt.text
					}
					if payload["content_index"] != float64(0) || !reflect.DeepEqual(object(t, payload["part"]), map[string]any{"type": "reasoning_text", "text": want}) {
						t.Fatalf("reasoning part = %#v", payload)
					}
				case "response.reasoning_text.delta":
					if payload["content_index"] != float64(0) {
						t.Fatalf("delta content index = %#v", payload)
					}
					deltas.WriteString(payload["delta"].(string))
					reasoningDeltas = append(reasoningDeltas, payload["delta"].(string))
				case "response.reasoning_text.done":
					if payload["text"] != tt.text || payload["content_index"] != float64(0) {
						t.Fatalf("reasoning done = %#v", payload)
					}
				case "response.output_item.done":
					if !reflect.DeepEqual(item, output[reasoningIndex]) {
						t.Fatalf("done item differs from final output: %#v", item)
					}
				}
			}
			if !reflect.DeepEqual(addedTypes, tt.types) || !reflect.DeepEqual(textDeltas, wantTextDeltas) || !reflect.DeepEqual(reasoningDeltas, wantReasoningDeltas) {
				t.Fatalf("added types=%v text deltas=%q reasoning deltas=%q, want types=%v text=%q reasoning=%q", addedTypes, textDeltas, reasoningDeltas, tt.types, wantTextDeltas, wantReasoningDeltas)
			}
			if reasoningIndex != 0 || (firstTextEvent >= 0 && lastReasoningEvent >= firstTextEvent) {
				t.Fatalf("reasoning index=%d last reasoning event=%d first text event=%d", reasoningIndex, lastReasoningEvent, firstTextEvent)
			}
			if len(lifecycle) < 6 || lifecycle[0] != "response.output_item.added" || lifecycle[1] != "response.content_part.added" || !reflect.DeepEqual(lifecycle[len(lifecycle)-3:], []string{"response.reasoning_text.done", "response.content_part.done", "response.output_item.done"}) || deltas.String() != tt.text {
				t.Fatalf("reasoning lifecycle = %v deltas=%q", lifecycle, deltas.String())
			}
			for _, typ := range lifecycle[2 : len(lifecycle)-3] {
				if typ != "response.reasoning_text.delta" {
					t.Fatalf("unexpected reasoning lifecycle event %q", typ)
				}
			}
			if object(t, object(t, completed["usage"])["output_tokens_details"])["reasoning_tokens"] != float64(4) {
				t.Fatalf("usage = %#v", completed["usage"])
			}
		})
	}
}

func TestDeepSeekReasoningEmptyPreservesTextTools(t *testing.T) {
	base := `{"role":"assistant","content":"雪\n","tool_calls":[{"index":0,"id":"c1","function":{"name":"lookup","arguments":"{}"}}]}`
	for _, empty := range []string{"", `,"reasoning_content":"","reasoning":""`, `,"reasoning_content":null,"reasoning":42`} {
		t.Run("fields="+empty, func(t *testing.T) {
			chunk := strings.TrimSuffix(base, "}") + empty + "}"
			w := httptest.NewRecorder()
			status, retryable := writeChatStreamAsResponses(w, strings.NewReader("data: {\"choices\":[{\"delta\":"+chunk+",\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), "deepseek-v4.1-flash-free", toolCallFilter{})
			if status != http.StatusOK || retryable {
				t.Fatalf("status/retryable = %d/%t", status, retryable)
			}
			wantOrder := []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_item.added", "response.function_call_arguments.delta", "response.output_text.done", "response.output_item.done", "response.function_call_arguments.done", "response.output_item.done", "response.completed"}
			if got := eventTypes(t, w.Body.String()); !reflect.DeepEqual(got, wantOrder) {
				t.Fatalf("event order = %v, want %v", got, wantOrder)
			}
			payloads := ssePayloads(t, w.Body.String())
			output := array(t, object(t, payloads[len(payloads)-1]["response"])["output"])
			if len(output) != 2 || object(t, array(t, object(t, output[0])["content"])[0])["text"] != "雪\n" || object(t, output[1])["arguments"] != "{}" {
				t.Fatalf("output = %#v", output)
			}
			message := map[string]any{"content": "雪\n", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}
			if empty != "" {
				message["reasoning_content"] = ""
				message["reasoning"] = ""
			}
			converted := array(t, chatCompletionToResponse(map[string]any{"choices": []any{map[string]any{"message": message}}})["output"])
			if len(converted) != 2 || object(t, converted[1])["call_id"] != "c1" {
				t.Fatalf("nonstream output = %#v", converted)
			}
		})
	}
}

func TestDeepSeekReasoningStreamEdges(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"思考\\n\"}}]}\n\n"
	queued := "data: {\"choices\":[{\"delta\":{\"content\":\"未完成 雪\\n\",\"tool_calls\":[{\"index\":0,\"id\":\"unfinished\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n"
	tests := []struct {
		name       string
		tail       string
		completed  bool
		incomplete bool
		status     int
	}{
		{"done without finish", "data: [DONE]\n\n", true, false, http.StatusOK},
		{"length", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", true, true, http.StatusOK},
		{"finish without done", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", true, false, http.StatusOK},
		{"interrupted eof", queued, false, false, http.StatusBadGateway},
		{"upstream error", queued + "data: {\"error\":{\"message\":\"upstream failed\"}}\n\n", false, false, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			status, retryable := writeChatStreamAsResponses(w, strings.NewReader(first+tt.tail), "deepseek-v4.1-flash-free", toolCallFilter{})
			if status != tt.status || retryable {
				t.Fatalf("status/retryable = %d/%t body=%s", status, retryable, w.Body.String())
			}
			payloads := ssePayloads(t, w.Body.String())
			var completed map[string]any
			var sawError, sawReasoning bool
			for _, payload := range payloads {
				if payload["type"] == "response.completed" {
					completed = object(t, payload["response"])
				}
				if payload["type"] == "response.reasoning_text.delta" && payload["delta"] == "思考\n" {
					sawReasoning = true
				}
				if _, exists := payload["error"]; exists {
					sawError = true
				}
			}
			if !sawReasoning || (completed != nil) != tt.completed || sawError == tt.completed {
				t.Fatalf("terminal semantics: completed=%#v error=%t reasoning=%t body=%s", completed, sawError, sawReasoning, w.Body.String())
			}
			if tt.completed {
				wantStatus := "completed"
				if tt.incomplete {
					wantStatus = "incomplete"
					if object(t, completed["incomplete_details"])["reason"] != "max_output_tokens" {
						t.Fatalf("incomplete details = %#v", completed)
					}
				}
				if completed["status"] != wantStatus {
					t.Fatalf("status = %#v", completed)
				}
				assertDeepSeekReasoningItem(t, object(t, array(t, completed["output"])[0]), "思考\n", "completed")
			} else if strings.Contains(w.Body.String(), ".done\"") || strings.Contains(w.Body.String(), "response.output_text.") || strings.Contains(w.Body.String(), "response.function_call_arguments.") || strings.Contains(w.Body.String(), `"call_id":"unfinished"`) {
				t.Fatalf("unfinished queued items emitted or marked done: %s", w.Body.String())
			}
		})
	}
}

func TestDeepSeekReasoningToolFiltering(t *testing.T) {
	upstream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"hidden\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"caller\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}],\"reasoning_content\":\"思考\"}}]}\n\ndata: [DONE]\n\n"
	w := httptest.NewRecorder()
	status, retryable := writeChatStreamAsResponses(w, strings.NewReader(upstream), "deepseek-v4.1-flash-free", toolCallFilter{injected: map[string]bool{"read": true}})
	if status != http.StatusOK || retryable || strings.Contains(w.Body.String(), `"call_id":"hidden"`) {
		t.Fatalf("filtered stream = status %d retryable %t body %s", status, retryable, w.Body.String())
	}
	payloads := ssePayloads(t, w.Body.String())
	output := array(t, object(t, payloads[len(payloads)-1]["response"])["output"])
	if len(output) != 2 || object(t, output[1])["type"] != "function_call" || object(t, output[1])["call_id"] != "caller" || object(t, output[1])["name"] != "lookup" || object(t, output[1])["arguments"] != "{}" {
		t.Fatalf("output = %#v", output)
	}
	assertDeepSeekReasoningItem(t, object(t, output[0]), "思考", "completed")
}

func TestDeepSeekReasoningHTTPToolReplay(t *testing.T) {
	for _, mode := range []struct {
		name    string
		stream  bool
		content string
		tool    bool
	}{
		{"nonstream tool only", false, "", true},
		{"stream tool only", true, "", true},
		{"nonstream message and tool", false, "Checking lookup.", true},
		{"stream message and tool", true, "Checking lookup.", true},
		{"nonstream message without tool", false, "5461", false},
		{"stream message without tool", true, "5461", false},
	} {
		t.Run(mode.name, func(t *testing.T) {
			setStabilityTestKnobs(t)
			t.Setenv("ZEN_RELAY_URL", "")
			t.Setenv("OPENCODE_GO_API_KEY", "")
			const reasoning = "查詢 雪\n驗證 "
			const missingReasoningError = "The `reasoning_content` in the thinking mode must be passed back to the API."
			initialText := "Use lookup with q=127*43."
			followupText := "Return the lookup result as the answer."
			answer := "答案 5461\n雪"
			if !mode.tool {
				initialText = "Compute 127 * 43. Reply with only the number."
				followupText = "Add 1 to that number. Reply with only the number."
				answer = "5462"
			}
			var mu sync.Mutex
			var firstTurns, rejected, accepted, repairedAccepted int
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/inference/openai/v1/chat/completions" {
					t.Errorf("upstream path = %q", r.URL.Path)
					http.Error(w, "wrong path", http.StatusBadRequest)
					return
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				if body["stream"] != true {
					t.Errorf("upstream stream = %#v, want true", body["stream"])
				}
				messages, _ := body["messages"].([]any)
				var assistant, result map[string]any
				for _, raw := range messages {
					message, _ := raw.(map[string]any)
					if message["role"] == "assistant" {
						assistant = message
					}
					if message["role"] == "tool" {
						result = message
					}
				}
				if len(messages) == 2 {
					user, _ := messages[1].(map[string]any)
					if user["role"] != "user" || user["content"] != initialText {
						t.Errorf("first-turn history = %#v", messages)
					}
					mu.Lock()
					firstTurns++
					mu.Unlock()
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"查詢 雪\\n\"}}]}\n\n"+
						"data: {\"choices\":[{\"delta\":{\"reasoning\":\"驗證 \"}}]}\n\n")
					if mode.content != "" {
						chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"role": "assistant", "content": mode.content}}}})
						_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\n")
					}
					if mode.tool {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"actual-call-雪\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"127*43\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
					} else {
						_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
					}
					_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":6,\"total_tokens\":10,\"completion_tokens_details\":{\"reasoning_tokens\":5}}}\n\ndata: [DONE]\n\n")
					return
				}
				wantCount := 4
				if mode.tool {
					wantCount++
				}
				if len(messages) != wantCount {
					t.Errorf("continuation history = %#v, want %d separate messages", messages, wantCount)
					http.Error(w, "invalid replay history", http.StatusBadRequest)
					return
				}
				calls, _ := assistant["tool_calls"].([]any)
				user, _ := messages[1].(map[string]any)
				followup, _ := messages[len(messages)-1].(map[string]any)
				valid := reflect.DeepEqual(messages[2], assistant) && user["role"] == "user" && user["content"] == initialText && assistant["role"] == "assistant" && followup["role"] == "user" && followup["content"] == followupText
				if mode.content == "" {
					valid = valid && assistant["content"] == ""
				} else {
					valid = valid && reflect.DeepEqual(assistant["content"], []any{map[string]any{"type": "text", "text": mode.content}})
				}
				if mode.tool {
					valid = valid && len(calls) == 1 && reflect.DeepEqual(messages[3], result) && result["role"] == "tool" && result["tool_call_id"] == "actual-call-雪" && result["content"] == "5461"
				} else {
					valid = valid && len(calls) == 0 && result == nil
				}
				if mode.tool && len(calls) == 1 {
					call, _ := calls[0].(map[string]any)
					function, _ := call["function"].(map[string]any)
					valid = valid && call["id"] == "actual-call-雪" && call["type"] == "function" && function["name"] == "lookup" && function["arguments"] == `{"q":"127*43"}`
				}
				if !valid {
					t.Errorf("continuation changed history: %#v", messages)
					http.Error(w, "invalid tool replay", http.StatusBadRequest)
					return
				}
				value, hasReasoning := assistant["reasoning_content"]
				if !hasReasoning {
					mu.Lock()
					rejected++
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": missingReasoningError}})
					return
				}
				if value != reasoning && value != "" {
					t.Errorf("unexpected replay reasoning = %#v", value)
					http.Error(w, "invalid replay reasoning", http.StatusBadRequest)
					return
				}
				mu.Lock()
				accepted++
				if value == "" {
					repairedAccepted++
				}
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": answer}, "finish_reason": "stop"}}})
				_, _ = io.WriteString(w, "data: "+string(chunk)+"\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			gateway := httptest.NewServer(proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}))
			defer gateway.Close()
			post := func(payload map[string]any) (int, []byte) {
				t.Helper()
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				seed := responsesTestRequest("/v1/responses", string(raw))
				request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", seed.Body)
				if err != nil {
					t.Fatal(err)
				}
				request.Header = seed.Header
				response, err := gateway.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				return response.StatusCode, body
			}
			user := map[string]any{"type": "message", "role": "user", "content": initialText}
			request := map[string]any{"model": "deepseek-v4.1-flash-free", "stream": mode.stream, "input": []any{user}}
			if mode.tool {
				request["tools"] = []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
			} else {
				request["tool_choice"] = "none"
			}
			status, firstBody := post(request)
			if status != http.StatusOK {
				t.Fatalf("first turn status = %d body=%s", status, firstBody)
			}
			var first map[string]any
			if mode.stream {
				payloads := ssePayloads(t, string(firstBody))
				assertSameResponseID(t, payloads)
				if len(payloads) == 0 || payloads[len(payloads)-1]["type"] != "response.completed" {
					t.Fatalf("first stream lacks completed response: %s", firstBody)
				}
				first = object(t, payloads[len(payloads)-1]["response"])
			} else {
				first = decodeJSON(t, firstBody)
			}
			output := array(t, first["output"])
			wantTypes := []string{"reasoning"}
			if mode.content != "" {
				wantTypes = append(wantTypes, "message")
			}
			if mode.tool {
				wantTypes = append(wantTypes, "function_call")
			}
			if len(output) != len(wantTypes) {
				t.Fatalf("first-turn output = %#v, want types %v", output, wantTypes)
			}
			input := append([]any{user}, output...)
			withoutReasoning := []any{user}
			var callID any
			var reasoningCount int
			for i, raw := range output {
				item := object(t, raw)
				if item["type"] != wantTypes[i] {
					t.Fatalf("first-turn output[%d] = %#v, want %s", i, item, wantTypes[i])
				}
				if item["type"] == "message" && object(t, array(t, item["content"])[0])["text"] != mode.content {
					t.Fatalf("first-turn message = %#v", item)
				}
				if item["type"] == "reasoning" {
					reasoningCount++
					assertDeepSeekReasoningItem(t, item, reasoning, "completed")
				} else {
					withoutReasoning = append(withoutReasoning, raw)
				}
				if item["type"] == "function_call" {
					callID = item["call_id"]
				}
			}
			if mode.tool {
				if callID != "actual-call-雪" {
					t.Fatalf("first-turn call = %#v", output)
				}
				result := map[string]any{"type": "function_call_output", "call_id": callID, "output": "5461"}
				input = append(input, result)
				withoutReasoning = append(withoutReasoning, result)
			}
			followup := map[string]any{"type": "message", "role": "user", "content": followupText}
			request["stream"] = false
			request["tool_choice"] = "none"
			request["input"] = append(withoutReasoning, followup)
			status, repairedBody := post(request)
			if status != http.StatusOK {
				t.Fatalf("missing reasoning status = %d body=%s, want HTTP 200", status, repairedBody)
			}
			repaired := decodeJSON(t, repairedBody)
			repairedOutput := array(t, repaired["output"])
			if repaired["status"] != "completed" || len(repairedOutput) != 1 || object(t, repairedOutput[0])["type"] != "message" || object(t, array(t, object(t, repairedOutput[0])["content"])[0])["text"] != answer {
				t.Fatalf("repaired replay final = %#v", repaired)
			}
			mu.Lock()
			firstCount, rejectedCount, acceptedCount, repairedCount := firstTurns, rejected, accepted, repairedAccepted
			mu.Unlock()
			if firstCount != 1 || rejectedCount != 1 || acceptedCount != 1 || repairedCount != 1 {
				t.Fatalf("repaired upstream turns: first=%d rejected=%d accepted=%d repaired=%d", firstCount, rejectedCount, acceptedCount, repairedCount)
			}
			request["input"] = append(input, followup)
			status, finalBody := post(request)
			if status != http.StatusOK {
				t.Fatalf("actual output replay status = %d body=%s, want HTTP 200", status, finalBody)
			}
			final := decodeJSON(t, finalBody)
			finalOutput := array(t, final["output"])
			if final["status"] != "completed" || len(finalOutput) != 1 || object(t, array(t, object(t, finalOutput[0])["content"])[0])["text"] != answer || reasoningCount != 1 {
				t.Fatalf("replay final = %#v reasoning items=%d", final, reasoningCount)
			}
			mu.Lock()
			defer mu.Unlock()
			if firstTurns != 1 || rejected != 1 || accepted != 2 || repairedAccepted != 1 {
				t.Fatalf("upstream turns: first=%d rejected=%d accepted=%d repaired=%d", firstTurns, rejected, accepted, repairedAccepted)
			}
		})
	}
}
