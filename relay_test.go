package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRelayDisabledUsesDirectUpstream(t *testing.T) {
	t.Setenv("ZEN_RELAY_URL", "")
	var relayHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=1", strings.NewReader(`{"stream":true}`))
	recorder := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNoContent)
	}
	if relayHeaders.Get("x-relay-target") != "" || relayHeaders.Get("x-relay-path") != "" {
		t.Fatalf("direct request unexpectedly carried relay headers: %v", relayHeaders)
	}
}

func TestRelayForwardsFinalRequestAndSSE(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "relay-test-key")
	body := `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	var gotMethod, gotPath, gotTarget, gotRelayPath, gotBody, gotAuth, gotRequestID string
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.RequestURI()
		gotTarget = r.Header.Get("x-relay-target")
		gotRelayPath = r.Header.Get("x-relay-path")
		gotAuth = r.Header.Get("Authorization")
		gotRequestID = r.Header.Get("x-opencode-request")
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read relay body: %v", err)
			return
		}
		gotBody = string(payload)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Relay-Response", "kept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message\ndata: {\"ok\":true}\n\ndata: [DONE]\n\n")
	}))
	defer relay.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?trace=a%2Fb", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-relay-target", "https://attacker.invalid")
	request.Header.Set("x-relay-path", "/spoofed")
	request.Header.Set("x-opencode-request", "spoofed-id")
	recorder := httptest.NewRecorder()
	proxyHandlerWithRelay("https://opencode.ai/inference", relay.Client(), &idMint{}, parseRelayURL(relay.URL+"/relay")).ServeHTTP(recorder, request)

	const eventStream = "event: message\ndata: {\"ok\":true}\n\ndata: [DONE]\n\n"
	if recorder.Code != http.StatusOK || recorder.Body.String() != eventStream {
		t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" || recorder.Header().Get("Cache-Control") != "no-cache" || recorder.Header().Get("X-Relay-Response") != "kept" {
		t.Fatalf("response headers = %v", recorder.Header())
	}
	if gotMethod != http.MethodPost || gotPath != "/relay" {
		t.Fatalf("relay request = %s %q, want POST /relay", gotMethod, gotPath)
	}
	if gotTarget != relayTarget {
		t.Fatalf("relay target = %q, want %q", gotTarget, relayTarget)
	}
	if gotRelayPath != "/inference/v1/chat/completions?trace=a%2Fb" {
		t.Fatalf("relay path = %q", gotRelayPath)
	}
	if gotAuth != "Bearer relay-test-key" || gotRequestID == "" || gotRequestID == "spoofed-id" {
		t.Fatalf("forwarded headers: Authorization=%q request id=%q", gotAuth, gotRequestID)
	}
	if !strings.Contains(gotBody, `"stream":true`) || !strings.Contains(gotBody, `"name":"read"`) {
		t.Fatalf("relay body did not preserve transformed request: %s", gotBody)
	}
}

func TestRelayPassesSSEThrough(t *testing.T) {
	const eventStream = "event: message\ndata: {\"id\":\"evt_1\"}\n\n"
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Relay-Response", "sse")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, eventStream)
	}))
	defer relay.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions?stream=1", strings.NewReader(`{"stream":true}`))
	request.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	proxyHandlerWithRelay("https://opencode.ai/inference", relay.Client(), &idMint{}, parseRelayURL(relay.URL)).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != eventStream {
		t.Fatalf("SSE response = %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Content-Type") != "text/event-stream" || recorder.Header().Get("Cache-Control") != "no-cache" || recorder.Header().Get("X-Relay-Response") != "sse" {
		t.Fatalf("SSE response headers = %v", recorder.Header())
	}
}

func TestRelayInvalidConfigFailsClosed(t *testing.T) {
	invalidURLs := []string{
		"http://relay.example/relay",
		"https:///relay",
		"https://user:pass@relay.example/relay",
		"https://relay.example/relay?query=1",
		"https://relay.example/relay#fragment",
		"https://relay.example/%zz",
		"https://relay.example/relay\nheader: value",
	}
	for _, relayURL := range invalidURLs {
		t.Run(relayURL, func(t *testing.T) {
			var hits int
			upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				hits++
			}))
			defer upstream.Close()
			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay(upstream.URL, upstream.Client(), &idMint{}, parseRelayURL(relayURL)).ServeHTTP(recorder, request)
			if recorder.Code != http.StatusInternalServerError || hits != 0 {
				t.Fatalf("invalid config response = %d, upstream hits = %d", recorder.Code, hits)
			}
		})
	}
}

func TestRelayDoesNotReplayResponses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "bad request protocol marker", status: http.StatusBadRequest, body: `{"error":"ModelProtocolUnsupported"}`},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":"forbidden"}`},
		{name: "other client error", status: http.StatusUnprocessableEntity, body: `{"error":"invalid"}`},
		{name: "server error", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			hits := 0
			relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer relay.Close()
			request := httptest.NewRequest(http.MethodPost, "/v1/responses?trace=1", strings.NewReader(`{"model":"test-model","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay("https://opencode.ai/inference", relay.Client(), &idMint{}, parseRelayURL(relay.URL)).ServeHTTP(recorder, request)
			if hits != 1 || recorder.Code != test.status {
				t.Fatalf("response = %d, relay hits = %d", recorder.Code, hits)
			}
			if recorder.Body.String() != test.body {
				t.Fatalf("response body = %q, want %q", recorder.Body.String(), test.body)
			}
		})
	}
}

func TestRelayRedirectsAreReturnedWithoutFollowing(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
	}{
		{name: "302", status: http.StatusFound},
		{name: "307", status: http.StatusTemporaryRedirect},
	} {
		t.Run(test.name, func(t *testing.T) {
			var relayHits, redirectHits int
			redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirectHits++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer redirectTarget.Close()

			relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				relayHits++
				w.Header().Set("Location", redirectTarget.URL+"/redirected")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, "redirect response")
			}))
			defer relay.Close()

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay("https://opencode.ai/inference", relay.Client(), &idMint{}, parseRelayURL(relay.URL)).ServeHTTP(recorder, request)

			if recorder.Code != test.status || recorder.Header().Get("Location") != redirectTarget.URL+"/redirected" || recorder.Body.String() != "redirect response" {
				t.Fatalf("response = %d, Location=%q, body=%q", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
			}
			if relayHits != 1 || redirectHits != 0 {
				t.Fatalf("relay hits = %d, redirect hits = %d", relayHits, redirectHits)
			}
		})
	}
}

func TestRelayTransportFailureDoesNotRetryOrFallBack(t *testing.T) {
	var relayHits, directHits int
	direct := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		directHits++
	}))
	defer direct.Close()
	relay := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relayHits++
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("relay response writer does not support hijacking")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack relay connection: %v", err)
			return
		}
		_ = connection.Close()
	}))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	recorder := httptest.NewRecorder()
	proxyHandlerWithRelay(direct.URL, relay.Client(), &idMint{}, parseRelayURL(relay.URL)).ServeHTTP(recorder, request)
	relay.Close()

	if recorder.Code != http.StatusBadGateway || relayHits != 1 || directHits != 0 {
		t.Fatalf("response = %d, relay hits = %d, direct hits = %d", recorder.Code, relayHits, directHits)
	}
}
