package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const stabilityInterruptedPair = "data: {\"error\":{\"message\":\"oc-zen: upstream stream interrupted\",\"type\":\"upstream_interrupted\"}}\n\ndata: [DONE]\n\n"

const stabilityCompletedResponsesSSE = "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"msg_1\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":123,\"usage\":{}}}\n\n"

func setStabilityTestKnobs(t *testing.T) {
	t.Helper()
	oldAttempts := maxAttempts
	oldBackoffs := retryBackoffs
	oldRetryAfterCap := retryAfterCap
	oldRetryLoopBudget := retryLoopBudget
	oldUpstreamIdleTimeout := upstreamIdleTimeout
	oldClientKeepAliveInterval := clientKeepAliveInterval
	t.Cleanup(func() {
		maxAttempts = oldAttempts
		retryBackoffs = oldBackoffs
		retryAfterCap = oldRetryAfterCap
		retryLoopBudget = oldRetryLoopBudget
		upstreamIdleTimeout = oldUpstreamIdleTimeout
		clientKeepAliveInterval = oldClientKeepAliveInterval
	})
	maxAttempts = 3
	retryBackoffs = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	retryAfterCap = 50 * time.Millisecond
	retryLoopBudget = 3 * time.Second
	upstreamIdleTimeout = 80 * time.Millisecond
	clientKeepAliveInterval = 20 * time.Millisecond
}

func stabilityRequest(body string, accept string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req.WithContext(context.WithValue(req.Context(), contextKey{}, "session-stability-test"))
}

func serveStabilityRequest(upstream *httptest.Server, body string, accept string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, stabilityRequest(body, accept))
	return recorder
}

func assertInterruptedPair(t *testing.T, body string) {
	t.Helper()
	if !strings.HasSuffix(body, stabilityInterruptedPair) {
		t.Fatalf("stream does not end with the interruption error and DONE pair: %q", body)
	}
}

func writeCompletedResponses(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, stabilityCompletedResponsesSSE)
}

func TestProxyHandlerRetriesBeforeMuseContent(t *testing.T) {
	setStabilityTestKnobs(t)
	const requestBody = `{"model":"muse-spark-1.3","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	var hits atomic.Int32
	var sessions, requestIDs, bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request body: %v", err)
			return
		}
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		requestIDs = append(requestIDs, r.Header.Get("x-opencode-request"))
		bodies = append(bodies, string(body))
		if len(sessions) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeCompletedResponses(w)
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, requestBody, "text/event-stream")
	if hits.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want 2", hits.Load())
	}
	if len(sessions) != 2 || sessions[0] != "session-stability-test" || sessions[1] != sessions[0] {
		t.Fatalf("upstream session IDs = %q, want the same session on both attempts", sessions)
	}
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[1] != requestIDs[0] {
		t.Fatalf("upstream request IDs = %q, want one stable non-empty ID", requestIDs)
	}
	if len(bodies) != 2 || bodies[0] == "" || bodies[1] != bodies[0] {
		t.Fatalf("upstream request bodies differ across retries: %q", bodies)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"content":"hello"`) || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("downstream stream is not a complete streamed answer: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestProxyHandlerStopsAfterThreeFailedAttempts(t *testing.T) {
	setStabilityTestKnobs(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
	if hits.Load() != 3 {
		t.Fatalf("upstream attempts = %d, want exactly 3", hits.Load())
	}
	assertInterruptedPair(t, recorder.Body.String())
	if strings.Count(recorder.Body.String(), "data: [DONE]\n\n") != 1 {
		t.Fatalf("downstream stream has other than one DONE frame: %q", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "finish_reason") {
		t.Fatalf("failed upstream fabricated a finish reason: %q", recorder.Body.String())
	}
}

func TestProxyHandlerRetryAfterDeltaAndCap(t *testing.T) {
	t.Run("one second delta is honored", func(t *testing.T) {
		setStabilityTestKnobs(t)
		retryAfterCap = 2 * time.Second
		retryLoopBudget = 3 * time.Second
		var hits atomic.Int32
		var firstAttempt time.Time
		var retryAttempt time.Time
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				firstAttempt = time.Now()
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			retryAttempt = time.Now()
			writeCompletedResponses(w)
		}))
		defer upstream.Close()

		recorder := serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
		if hits.Load() != 2 {
			t.Fatalf("upstream attempts = %d, want 2", hits.Load())
		}
		waited := retryAttempt.Sub(firstAttempt)
		if waited < 900*time.Millisecond || waited > 2500*time.Millisecond {
			t.Fatalf("Retry-After: 1 delay = %s, want approximately one second", waited)
		}
		if !strings.Contains(recorder.Body.String(), `"content":"hello"`) {
			t.Fatalf("retry did not return the streamed answer: %q", recorder.Body.String())
		}
	})

	t.Run("sixty second delta is capped", func(t *testing.T) {
		setStabilityTestKnobs(t)
		const shortCap = 60 * time.Millisecond
		retryAfterCap = shortCap
		var hits atomic.Int32
		var firstAttempt time.Time
		var retryAttempt time.Time
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) == 1 {
				firstAttempt = time.Now()
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			retryAttempt = time.Now()
			writeCompletedResponses(w)
		}))
		defer upstream.Close()

		serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
		if hits.Load() != 2 {
			t.Fatalf("upstream attempts = %d, want 2", hits.Load())
		}
		waited := retryAttempt.Sub(firstAttempt)
		if waited < 40*time.Millisecond || waited > 500*time.Millisecond {
			t.Fatalf("Retry-After: 60 delay = %s, want the shortened cap near %s", waited, shortCap)
		}
	})
}

func TestProxyHandlerDoesNotRetryAfterMuseContent(t *testing.T) {
	setStabilityTestKnobs(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		payload := "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"id\":\"msg_1\"}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)+100))
		_, _ = io.WriteString(w, payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
	if hits.Load() != 1 {
		t.Fatalf("upstream attempts = %d, want one after content started", hits.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"partial"`) {
		t.Fatalf("downstream did not receive content before interruption: %q", recorder.Body.String())
	}
	assertInterruptedPair(t, recorder.Body.String())
	if strings.Contains(recorder.Body.String(), "finish_reason") {
		t.Fatalf("interrupted stream fabricated a finish reason: %q", recorder.Body.String())
	}
}

func TestProxyHandlerPassThroughRetriesBeforeResponseHeaders(t *testing.T) {
	setStabilityTestKnobs(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("upstream response writer cannot close the connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack upstream connection: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: recovered\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"gpt-4o","stream":true,"messages":[]}`, "text/event-stream")
	if hits.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want retry after header-level disconnect", hits.Load())
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "data: recovered\n\ndata: [DONE]\n\n" {
		t.Fatalf("pass-through retry response = %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestProxyHandlerPassThroughDoesNotRetryAfterBytes(t *testing.T) {
	setStabilityTestKnobs(t)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		payload := "data: partial\n\n"
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)+100))
		_, _ = io.WriteString(w, payload)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"gpt-4o","stream":true,"messages":[]}`, "text/event-stream")
	if hits.Load() != 1 {
		t.Fatalf("upstream attempts = %d, want one after downstream bytes", hits.Load())
	}
	if !strings.HasPrefix(recorder.Body.String(), "data: partial\n\n") {
		t.Fatalf("downstream missed upstream bytes: %q", recorder.Body.String())
	}
	assertInterruptedPair(t, recorder.Body.String())
}

func TestProxyHandlerIdleWatchdogRetriesStalledMuseStream(t *testing.T) {
	setStabilityTestKnobs(t)
	upstreamIdleTimeout = 35 * time.Millisecond
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-time.After(120 * time.Millisecond)
			return
		}
		writeCompletedResponses(w)
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
	if hits.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want pre-content retry after idle watchdog", hits.Load())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"hello"`) || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("watchdog retry did not produce a complete stream: %q", recorder.Body.String())
	}
}

func TestProxyHandlerKeepsMuseStreamAliveUntilContent(t *testing.T) {
	setStabilityTestKnobs(t)
	upstreamIdleTimeout = 2 * time.Second
	clientKeepAliveInterval = 30 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(140 * time.Millisecond)
		writeCompletedResponses(w)
	}))
	defer upstream.Close()

	recorder := serveStabilityRequest(upstream, `{"model":"muse-spark-1.3","stream":true,"messages":[]}`, "text/event-stream")
	if !strings.Contains(recorder.Body.String(), ": keep-alive\n\n") {
		t.Fatalf("downstream did not receive an SSE keep-alive: %q", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"content":"hello"`) || !strings.HasSuffix(recorder.Body.String(), "data: [DONE]\n\n") {
		t.Fatalf("stream did not continue to a complete answer after keep-alive: %q", recorder.Body.String())
	}
}
