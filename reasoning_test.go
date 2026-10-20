package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const capturedReasoningError = `{"error":{"param":null,"type":"invalid_request_error","code":"invalid_request_error","message":"Upstream request failed: [invalid_request_error] The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API. (request_id: 16af04e4-314b-448a-b59f-533c068dbc0b)"}}`

const capturedReasoningSuccess = "data: {\"id\":\"safe_completion\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"safe_answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func capturedReasoningFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/reasoning-missing-98.json")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	missing, calls, results := 0, 0, 0
	ids := map[string]bool{}
	for _, raw := range messages {
		message := raw.(map[string]any)
		if message["role"] == "assistant" {
			if _, exists := message["reasoning_content"]; !exists {
				missing++
			}
		}
		for _, rawCall := range arrayOrNil(message["tool_calls"]) {
			call := rawCall.(map[string]any)
			id := call["id"].(string)
			if ids[id] {
				t.Fatalf("duplicate captured call %q", id)
			}
			ids[id] = true
			calls++
		}
		if message["role"] == "tool" {
			if !ids[message["tool_call_id"].(string)] {
				t.Fatal("captured result has no preceding call")
			}
			results++
		}
	}
	if len(messages) != 98 || missing != 42 || calls != 36 || results != 36 || len(body["tools"].([]any)) != 30 || len(capturedReasoningError) != 275 {
		t.Fatalf("captured shape messages=%d missing=%d calls=%d results=%d tools=%d error=%d", len(messages), missing, calls, results, len(body["tools"].([]any)), len(capturedReasoningError))
	}
	for _, index := range []int{40, 44, 55, 62, 86, 88} {
		message := messages[index].(map[string]any)
		if message["role"] != "assistant" || len(arrayOrNil(message["tool_calls"])) != 0 {
			t.Fatalf("plain assistant position %d changed", index)
		}
	}
	return body
}

func arrayOrNil(value any) []any {
	array, _ := value.([]any)
	return array
}

func capturedReasoningRequest(t *testing.T, route string, stream bool) []byte {
	t.Helper()
	body := capturedReasoningFixture(t)
	body["stream"] = stream
	if route == "/v1/responses" {
		input := []any{}
		for _, raw := range body["messages"].([]any) {
			message := raw.(map[string]any)
			if message["role"] == "tool" {
				input = append(input, map[string]any{"type": "function_call_output", "call_id": message["tool_call_id"], "output": message["content"]})
				continue
			}
			input = append(input, map[string]any{"type": "message", "role": message["role"], "content": message["content"]})
			for _, rawCall := range arrayOrNil(message["tool_calls"]) {
				call := rawCall.(map[string]any)
				function := call["function"].(map[string]any)
				input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": function["arguments"]})
			}
		}
		tools := []any{}
		for _, raw := range body["tools"].([]any) {
			function := raw.(map[string]any)["function"].(map[string]any)
			tools = append(tools, map[string]any{"type": "function", "name": function["name"], "description": function["description"], "parameters": function["parameters"]})
		}
		body["input"], body["tools"] = input, tools
		delete(body, "messages")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func capturedReasoningHandler(t *testing.T, relay bool, upstream *httptest.Server) http.Handler {
	t.Helper()
	if relay {
		return proxyHandlerWithRelay("https://opencode.ai/inference", upstream.Client(), &idMint{}, parseRelayURL(upstream.URL+"/relay"))
	}
	return proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})
}

func capturedReasoningClientRequest(route string, body []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, route, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-opencode-session", "ses_reasoning_captured")
	request.Header.Set("Authorization", "Bearer safe-test-key")
	return request
}

func assertCapturedReasoningBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return nil
	}
	if r.ContentLength != int64(len(raw)) || r.Header.Get("Content-Length") != fmt.Sprint(len(raw)) {
		t.Errorf("request length = %d/%q, bytes=%d", r.ContentLength, r.Header.Get("Content-Length"), len(raw))
	}
	return raw
}

func TestCapturedReasoningRepair(t *testing.T) {
	for _, route := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, relay := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/relay=%v", route, stream, relay), func(t *testing.T) {
					setStabilityTestKnobs(t)
					t.Setenv("OPENCODE_GO_API_KEY", "")
					body := capturedReasoningRequest(t, route, stream)
					outboundBodies := make(chan []byte, 2)
					var sends atomic.Int32
					upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						n := sends.Add(1)
						if n > 2 {
							t.Errorf("unexpected upstream send %d", n)
							http.Error(w, "third send forbidden", http.StatusBadRequest)
							return
						}
						outboundBodies <- assertCapturedReasoningBody(t, r)
						if n == 1 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, capturedReasoningError)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, capturedReasoningSuccess)
					}))
					defer upstream.Close()
					recorder := httptest.NewRecorder()
					capturedReasoningHandler(t, relay, upstream).ServeHTTP(recorder, capturedReasoningClientRequest(route, body))
					if recorder.Code != http.StatusOK {
						t.Fatalf("client status = %d, want 200; upstream sends = %d", recorder.Code, sends.Load())
					}
					if sends.Load() != 2 || !strings.Contains(recorder.Body.String(), "safe_answer") {
						t.Fatalf("repair completion sends=%d body=%q", sends.Load(), recorder.Body.String())
					}
					if route == "/v1/responses" && !strings.Contains(recorder.Body.String(), `"status":"completed"`) {
						t.Fatalf("Responses completion missing: %s", recorder.Body.String())
					}
					firstOutbound, secondOutbound := <-outboundBodies, <-outboundBodies
					first := reasoningObject(t, firstOutbound)
					missing := 0
					for _, raw := range first["messages"].([]any) {
						message := raw.(map[string]any)
						if message["role"] == "assistant" {
							if _, exists := message["reasoning_content"]; !exists {
								missing++
							}
						}
					}
					if missing != 42 {
						t.Fatalf("first outbound missing reasoning = %d, want 42", missing)
					}
					assertReasoningRepair(t, firstOutbound, secondOutbound)
				})
			}
		}
	}
}
