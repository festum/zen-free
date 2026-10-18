package main

import (
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
		if r.URL.Path != "/v1/chat/completions" {
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
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("client content type = %q", got)
	}
	mu.Lock()
	if len(paths) != 1 || paths[0] != "/v1/chat/completions?trace=stream%2F1" {
		t.Fatalf("upstream paths = %v, want the Chat route with query preserved", paths)
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
		if r.URL.Path != "/v1/chat/completions" {
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
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("client content type = %q", got)
	}
	mu.Lock()
	if len(paths) != 1 || paths[0] != "/v1/chat/completions?trace=nonstream%2F1" {
		t.Fatalf("upstream paths = %v, want the Chat route with query preserved", paths)
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
		case "/v1/chat/completions":
			chatBody = decoded
		case "/v1/responses":
			nativeBody = decoded
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"ModelProtocolUnsupported"}}`)
		case "/v1/responses":
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
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	mu.Lock()
	seen := strings.Join(paths, ",")
	mu.Unlock()
	wantPaths := "/v1/chat/completions?trace=fallback%2F1,/v1/responses?trace=fallback%2F1"
	if seen != wantPaths {
		t.Fatalf("upstream paths = %q, want chat-first then native Responses with query preserved", seen)
	}
	wantChatTools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	wantNativeTools := []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}
	if !reflect.DeepEqual(chatBody["tools"].([]any)[:1], wantChatTools) || !reflect.DeepEqual(nativeBody["tools"].([]any)[:1], wantNativeTools) {
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

	request := responsesTestRequest("/v1/responses", `{"model":"muse-spark-1.3-contributor-free","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/v1/responses" {
		t.Fatalf("upstream paths = %v, want single native OpenAI Responses route", paths)
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", w.Code, w.Body.String())
	}
	mu.Lock()
	if upstreamBody == nil {
		t.Fatal("upstream saw no body")
	}
	messages := array(t, upstreamBody["messages"])
	mu.Unlock()
	if len(messages) != 3 {
		t.Fatalf("upstream messages = %#v, want user + assistant(tool_calls) + tool", messages)
	}
	user := object(t, messages[0])
	if user["role"] != "user" {
		t.Fatalf("user = %#v", user)
	}
	assistant := object(t, messages[1])
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant = %#v", assistant)
	}
	calls := array(t, assistant["tool_calls"])
	call := object(t, calls[0])
	function := object(t, call["function"])
	if call["id"] != "call-1" || function["name"] != "lookup" || function["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("assistant call = %#v", call)
	}
	toolReply := object(t, messages[2])
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
