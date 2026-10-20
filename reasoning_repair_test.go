package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
)

func reasoningFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func reasoningObject(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var v map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func assertReasoningRepair(t *testing.T, before, after []byte) {
	t.Helper()
	original, repaired := reasoningObject(t, before), reasoningObject(t, after)
	oldMessages := original["messages"].([]any)
	newMessages := repaired["messages"].([]any)
	if len(oldMessages) != len(newMessages) {
		t.Fatal("message count changed")
	}
	assistants, missing, added := 0, 0, 0
	for i, value := range oldMessages {
		old, ok := value.(map[string]any)
		if !ok || old["role"] != "assistant" {
			continue
		}
		assistants++
		next := newMessages[i].(map[string]any)
		if _, ok := next["reasoning_content"]; !ok {
			t.Fatalf("assistant %d missing reasoning_content", i)
		}
		if _, ok := old["reasoning_content"]; !ok {
			missing++
			if next["reasoning_content"] != "" {
				t.Fatalf("assistant %d has invented reasoning", i)
			}
			delete(next, "reasoning_content")
			added++
		}
	}
	if assistants == 0 || missing == 0 || added != missing {
		t.Fatalf("assistants=%d missing=%d added=%d", assistants, missing, added)
	}
	if !reflect.DeepEqual(original, repaired) {
		t.Fatal("repair changed unrelated JSON values")
	}
}

func TestIsReasoningRequiredError(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"both", "reasoning_content in thinking mode", true},
		{"reasoning only", "reasoning_content", false},
		{"mode only", "thinking mode", false},
		{"unrelated", "invalid request", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReasoningRequiredError([]byte(tc.body)); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestRepairReasoningContentMissing98(t *testing.T) {
	body := reasoningFixture(t, "reasoning-missing-98.json")
	original := reasoningObject(t, body)
	if len(original["messages"].([]any)) != 98 {
		t.Fatal("fixture must contain 98 messages")
	}
	repaired, changed := repairReasoningContent(body)
	if !changed {
		t.Fatal("expected repair")
	}
	assertReasoningRepair(t, body, repaired)
	for _, v := range original["messages"].([]any) {
		m, ok := v.(map[string]any)
		if ok && m["role"] == "assistant" {
			if _, exists := m["reasoning_content"]; exists {
				t.Fatal("fixture assistant already has reasoning")
			}
		}
	}
}

func TestRepairReasoningContentAlreadyPresent(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","reasoning_content":"existing"},{"role":"assistant","reasoning_content":""},{"role":"assistant","reasoning_content":null}]}`)
	saved := bytes.Clone(body)
	repaired, changed := repairReasoningContent(body)
	if changed || repaired != nil || !bytes.Equal(body, saved) {
		t.Fatal("already-present reasoning modified")
	}
}

func TestRepairReasoningContentInvalidOrUnchanged(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid", `{`}, {"null", `null`}, {"non-object", `[]`},
		{"missing", `{}`}, {"null messages", `{"messages":null}`},
		{"non-array", `{"messages":{}}`}, {"no assistant", `{"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, changed := repairReasoningContent([]byte(tc.body))
			if changed || b != nil {
				t.Fatal("unexpected repair")
			}
		})
	}
	t.Run("preserve unrelated fields", func(t *testing.T) {
		body := []byte(`{"large":9007199254740993,"tools":[{"type":"function","function":{"name":"lookup"}}],"messages":[null,7,{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}]}`)
		repaired, changed := repairReasoningContent(body)
		if !changed {
			t.Fatal("expected repair")
		}
		assertReasoningRepair(t, body, repaired)
	})
}

func runReasoningHandler(t *testing.T, body []byte, relay bool, failFirst, failAgain bool) (int, []byte, [][]byte, []int64) {
	t.Helper()
	t.Setenv("ZEN_DEBUG_DUMP", "")
	t.Setenv("ZEN_DEBUG_DUMP_RAW", "")
	t.Setenv("ZEN_RELAY_URL", "")
	errorBody := reasoningFixture(t, "reasoning-required-error.json")
	if len(errorBody) != 275 {
		t.Fatalf("error fixture length %d, want 275", len(errorBody))
	}
	var received [][]byte
	var lengths []int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		received = append(received, b)
		lengths = append(lengths, r.ContentLength)
		if relay && (r.Header.Get("x-relay-target") == "" || r.Header.Get("x-relay-path") == "") {
			t.Error("missing relay routing headers")
		}
		w.Header().Set("Content-Type", "application/json")
		if failFirst && (len(received) == 1 || failAgain) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write(errorBody)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-repair","object":"chat.completion","model":"deepseek-v4.1-flash-free","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	upstream := server.URL
	if relay {
		t.Setenv("ZEN_RELAY_URL", server.URL)
		upstream = "https://opencode.ai/zen/v1"
	}
	handler := proxyHandler(upstream, server.Client(), &idMint{})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes(), received, lengths
}

func TestProxyReasoningRepair(t *testing.T) {
	body := reasoningFixture(t, "reasoning-missing-98.json")
	status, _, received, lengths := runReasoningHandler(t, body, false, true, false)
	if status != 200 || len(received) != 2 {
		t.Fatalf("status=%d attempts=%d", status, len(received))
	}
	assertReasoningRepair(t, received[0], received[1])
	if lengths[1] != int64(len(received[1])) {
		t.Fatal("repaired content length mismatch")
	}
}

func TestProxyReasoningRepairFirstSuccess(t *testing.T) {
	body := []byte(`{"messages":[{"content":"hello","role":"user"}],"model":"deepseek-v4.1-flash-free","stream":true,"tool_choice":"none","tools":[{"function":{"name":"lookup"},"type":"function"}]}`)
	status, _, received, _ := runReasoningHandler(t, body, false, false, false)
	if status != 200 || len(received) != 1 {
		t.Fatalf("status=%d attempts=%d", status, len(received))
	}
	if !bytes.Equal(received[0], body) {
		t.Fatalf("first-success body changed: %s", received[0])
	}
}

func TestProxyReasoningRepairRepeatedError(t *testing.T) {
	body := reasoningFixture(t, "reasoning-missing-98.json")
	status, response, received, _ := runReasoningHandler(t, body, false, true, true)
	if status != 400 || len(received) != 2 {
		t.Fatalf("status=%d attempts=%d", status, len(received))
	}
	if !bytes.Equal(response, reasoningFixture(t, "reasoning-required-error.json")) || len(response) != 275 {
		t.Fatalf("error payload changed: %s", response)
	}
}

func TestProxyReasoningRepairNoChange(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"assistant","content":"ok","reasoning_content":""}]}`)
	status, response, received, _ := runReasoningHandler(t, body, false, true, true)
	if status != 400 || len(received) != 1 {
		t.Fatalf("status=%d attempts=%d", status, len(received))
	}
	if !bytes.Equal(response, reasoningFixture(t, "reasoning-required-error.json")) {
		t.Fatal("error payload changed")
	}
}

func TestProxyReasoningRepairRelay(t *testing.T) {
	body := reasoningFixture(t, "reasoning-missing-98.json")
	status, response, received, lengths := runReasoningHandler(t, body, true, true, false)
	if status != 200 || len(received) != 2 {
		t.Fatalf("status=%d attempts=%d response=%s", status, len(received), response)
	}
	assertReasoningRepair(t, received[0], received[1])
	if lengths[1] != int64(len(received[1])) {
		t.Fatal("repaired content length mismatch")
	}
}
