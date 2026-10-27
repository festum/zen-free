package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const v2ChatSSE = "data: {\"id\":\"chatcmpl_v2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_v2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

type v2CapturedRequest struct {
	path string
	body []byte
}

func newV2Upstream(t *testing.T, responseSSE string) (*httptest.Server, chan v2CapturedRequest) {
	t.Helper()
	captured := make(chan v2CapturedRequest, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
		}
		captured <- v2CapturedRequest{r.URL.Path, body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responseSSE)
	}))
	t.Cleanup(upstream.Close)
	return upstream, captured
}

func postV2Request(t *testing.T, handler http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeV2Body(t *testing.T, captured v2CapturedRequest) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(captured.body, &body); err != nil {
		t.Fatalf("decode upstream body %q: %v", captured.body, err)
	}
	return body
}

func TestV2IdentityAndTracingHeaders(t *testing.T) {
	var mu sync.Mutex
	var captured []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		captured = append(captured, r.Header.Clone())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list"}`)
	}))
	defer upstream.Close()

	handler := proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})
	for i := range 2 {
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d body %q", i, recorder.Code, recorder.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("upstream requests = %d, want 2", len(captured))
	}
	first, second := captured[0], captured[1]
	if got, want := first.Get("User-Agent"), "opencode/latest/2.1.4/cli"; got != want {
		t.Errorf("User-Agent = %q, want %q", got, want)
	}
	if got := first.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client = %q, want cli", got)
	}
	if got, want := first.Get("x-opencode-project"), opencodeProjectID; got != want {
		t.Errorf("x-opencode-project = %q, want %q", got, want)
	}
	session := first.Get("x-opencode-session")
	if session == "" || session != first.Get("x-opencode-session-id") || session != first.Get("x-session-affinity") || session != first.Get("x-session-id") {
		t.Fatalf("session headers = (%q, %q, %q, %q), want equal non-empty", session, first.Get("x-opencode-session-id"), first.Get("x-session-affinity"), first.Get("x-session-id"))
	}
	b3Pattern := regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}-1-[0-9a-f]{16}$`)
	traceparentPattern := regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	b3 := first.Get("b3")
	traceparent := first.Get("traceparent")
	if !b3Pattern.MatchString(b3) {
		t.Errorf("b3 = %q, want <32hex>-<16hex>-1-<16hex>", b3)
	}
	if !traceparentPattern.MatchString(traceparent) {
		t.Errorf("traceparent = %q, want 00-<32hex>-<16hex>-01", traceparent)
	}
	firstFields, secondFields := strings.Split(b3, "-"), strings.Split(second.Get("b3"), "-")
	if firstFields[0] != secondFields[0] {
		t.Errorf("trace root changed across requests: %q vs %q", firstFields[0], secondFields[0])
	}
	if firstFields[0] != strings.Split(traceparent, "-")[1] {
		t.Errorf("b3 root %q does not match traceparent root %q", firstFields[0], strings.Split(traceparent, "-")[1])
	}
	if firstFields[1] == secondFields[1] {
		t.Errorf("trace span reused across requests: %q", firstFields[1])
	}
}

func TestV2ChatWireStreamDefaultsAndBudget(t *testing.T) {
	upstream, captured := newV2Upstream(t, v2ChatSSE)
	handler := proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})

	recorder := postV2Request(t, handler, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "OK") {
		t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	got := <-captured
	if got.path != "/inference/openai/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /inference/openai/v1/chat/completions", got.path)
	}
	body := decodeV2Body(t, got)
	if options := object(t, body["stream_options"]); options["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true", options)
	}
	if body["max_completion_tokens"] != float64(v2DefaultTokenBudget) {
		t.Fatalf("max_completion_tokens = %#v, want %d", body["max_completion_tokens"], v2DefaultTokenBudget)
	}
	if body["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %#v, want none", body["tool_choice"])
	}
	first := object(t, array(t, body["messages"])[0])
	if first["role"] != "system" || !strings.HasPrefix(stringValue(first["content"]), preambleMarker) {
		t.Fatalf("first message = %#v, want preamble system message", first)
	}
	toolNames := map[string]bool{}
	for _, raw := range array(t, body["tools"]) {
		toolNames[toolName(object(t, raw))] = true
	}
	for _, want := range []string{"edit", "glob", "grep", "read", "write", "bash"} {
		if !toolNames[want] {
			t.Errorf("tools missing %q: %v", want, toolNames)
		}
	}
	for _, absent := range []string{"shell", "question", "skill", "subagent", "webfetch", "websearch", "execute"} {
		if toolNames[absent] {
			t.Errorf("tools contains unmapped manifest tool %q", absent)
		}
	}

	recorder = postV2Request(t, handler, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"max_tokens":4096}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("explicit max_tokens client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	body = decodeV2Body(t, <-captured)
	if body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %#v, want 4096", body["max_tokens"])
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("max_completion_tokens = %#v, want absent with explicit max_tokens", body["max_completion_tokens"])
	}

	recorder = postV2Request(t, handler, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":55}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("explicit max_completion_tokens client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	body = decodeV2Body(t, <-captured)
	if body["max_completion_tokens"] != float64(55) {
		t.Fatalf("max_completion_tokens = %#v, want 55", body["max_completion_tokens"])
	}
}

func TestV2ResponsesToChatBudgetMapping(t *testing.T) {
	upstream, captured := newV2Upstream(t, v2ChatSSE)
	handler := proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})

	recorder := postV2Request(t, handler, "/v1/responses", `{"model":"deepseek-v4.1-flash-free","instructions":"house rules","input":"hi","max_output_tokens":64}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "OK") {
		t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	got := <-captured
	if got.path != "/inference/openai/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /inference/openai/v1/chat/completions", got.path)
	}
	body := decodeV2Body(t, got)
	if body["max_tokens"] != float64(64) || body["max_completion_tokens"] != float64(64) {
		t.Fatalf("budget mapping = max_tokens %#v max_completion_tokens %#v, want 64/64", body["max_tokens"], body["max_completion_tokens"])
	}
	if options := object(t, body["stream_options"]); options["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true", options)
	}
	system := stringValue(object(t, array(t, body["messages"])[0])["content"])
	if !strings.HasPrefix(system, preambleMarker) || !strings.Contains(system, "house rules") {
		t.Fatalf("system content = %.80q, want preamble prefix and original instructions", system)
	}
}

func TestV2MuseChatToResponsesBudget(t *testing.T) {
	upstream, captured := newV2Upstream(t, sampleResponsesSSE(t))
	handler := proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})

	recorder := postV2Request(t, handler, "/v1/chat/completions", `{"model":"muse-spark-1.3","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":77}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	got := <-captured
	if got.path != "/inference/openai/v1/responses" {
		t.Fatalf("upstream path = %q, want /inference/openai/v1/responses", got.path)
	}
	body := decodeV2Body(t, got)
	if body["max_output_tokens"] != float64(77) {
		t.Fatalf("max_output_tokens = %#v, want 77", body["max_output_tokens"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatalf("stream_options = %#v, want absent on Responses wire", body["stream_options"])
	}
	if body["stream"] != true {
		t.Fatalf("stream = %#v, want true", body["stream"])
	}
	if instructions := stringValue(body["instructions"]); !strings.HasPrefix(instructions, preambleMarker) {
		t.Fatalf("instructions = %.80q, want preamble prefix", instructions)
	}
}

func TestV2UnofferedToolCallsHiddenFromClient(t *testing.T) {
	toolSSE := "data: {\"id\":\"chatcmpl_v2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"id\":\"call_bash\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_v2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"id\":\"call_lookup\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_v2\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	upstream, _ := newV2Upstream(t, toolSSE)
	handler := proxyHandlerWithRelay(upstream.URL+"/inference", upstream.Client(), &idMint{}, relayConfig{})

	recorder := postV2Request(t, handler, "/v1/chat/completions", `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	clientBody := recorder.Body.String()
	if !strings.Contains(clientBody, "lookup") {
		t.Fatalf("client stream missing caller tool call: %s", clientBody)
	}
	if strings.Contains(clientBody, "bash") {
		t.Fatalf("client stream leaked hidden tool call: %s", clientBody)
	}
}
