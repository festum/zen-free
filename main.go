package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var opencodeVersion = "1.18.35"

var fingerprintUA = buildFingerprintUA()

func buildFingerprintUA() string {
	return "opencode/" + opencodeVersion + " ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
}

type contextKey struct{}

type idMint struct {
	mu        sync.Mutex
	lastStamp int64
	seq       int64
}

func (m *idMint) mint(prefix string) string {
	m.mu.Lock()
	nowMS := time.Now().UnixMilli()
	if nowMS != m.lastStamp {
		m.lastStamp = nowMS
		m.seq = 0
	}
	m.seq++
	value := ^(uint64(nowMS)<<12 | uint64(m.seq&0xFFF))
	m.mu.Unlock()

	var raw [6]byte
	for i := range raw {
		raw[i] = byte(value >> (40 - 8*i))
	}
	var random [14]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic(fmt.Errorf("generate fingerprint id: %w", err))
	}
	var id [30]byte
	copy(id[:], prefix)
	hex.Encode(id[len(prefix):len(prefix)+12], raw[:])
	for i, c := range random {
		id[len(prefix)+12+i] = base62[int(c)%len(base62)]
	}
	return string(id[:])
}

var sessionIDPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

func validSessionID(value string) bool {
	return sessionIDPattern.MatchString(value)
}

func augmentChatTools(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if _, exists := body["tools"]; exists && !ok {
		return false
	}
	if !ok {
		tools = nil
	}
	present := make(map[string]bool)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := tool["name"].(string); ok {
			present[strings.ToLower(name)] = true
		}
		if function, ok := tool["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok {
				present[strings.ToLower(name)] = true
			}
		}
	}
	missing := false
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		missing = true
		tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
			"name": name, "description": quartetDescription,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}})
	}
	if !missing {
		return false
	}
	if _, exists := body["tools"]; !exists {
		if _, choiceExists := body["tool_choice"]; !choiceExists {
			body["tool_choice"] = "none"
		}
	}
	body["tools"] = tools
	return true
}

func augmentFlatTools(body map[string]any) bool {
	tools, _ := body["tools"].([]any)
	present := make(map[string]bool)
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok || tool["type"] != "function" {
			continue
		}
		if name, ok := tool["name"].(string); ok {
			present[strings.ToLower(name)] = true
		}
	}
	missing := false
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		missing = true
		tools = append(tools, map[string]any{"type": "function", "name": name, "description": quartetDescription, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	}
	if missing {
		body["tools"] = tools
	}
	return missing
}

func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Set("Content-Length", fmt.Sprint(len(body)))
}

func prepareBody(req *http.Request) ([]byte, map[string]any, bool, error) {
	if !hasBody(req) || !strings.Contains(strings.ToLower(req.Header.Get("Content-Type")), "application/json") {
		return nil, nil, false, nil
	}
	original, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, nil, false, fmt.Errorf("read request body: %w", err)
	}
	_ = req.Body.Close()
	var body map[string]any
	if err := json.Unmarshal(original, &body); err != nil || body == nil {
		setRequestBody(req, original)
		return original, nil, false, nil
	}
	return original, body, true, nil
}

func hasBody(req *http.Request) bool {
	return req.Body != nil && req.Body != http.NoBody
}

var hopHeaders = []string{
	"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Proxy-Connection",
	"TE", "Trailer", "Trailers", "Host", "Content-Length",
}

func stripHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopHeaders {
		header.Del(name)
	}
}

type flushWriter struct {
	w http.ResponseWriter
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func proxyHandler(upstream string, client *http.Client, mint *idMint) http.Handler {
	base := strings.TrimRight(upstream, "/")
	apiKey := os.Getenv("OPENCODE_GO_API_KEY")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		session, _ := r.Context().Value(contextKey{}).(string)
		if session == "" {
			session = r.Header.Get("x-opencode-session")
			if session == "" {
				session = r.Header.Get("x-opencode-session-id")
			}
			if !validSessionID(session) {
				session = mint.mint("ses_")
			}
		}
		requestID := mint.mint("msg_")
		status, attempts := http.StatusBadGateway, 0
		defer func() {
			log.Printf("%s %s -> %d ses=%s req=%s attempt=%d dur=%s", r.Method, r.URL.RequestURI(), status, session, requestID, attempts, time.Since(started))
		}()
		original, requestBody, decoded, err := prepareBody(r)
		if err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			status = http.StatusBadRequest
			return
		}
		museChat, streaming, chatWire, responsesWire, clientStream := false, false, false, false, false
		requestedModel := ""
		if decoded {
			requestedModel, _ = requestBody["model"].(string)
			streaming, _ = requestBody["stream"].(bool)
			clientStream = streaming
		}
		switch {
		case strings.Contains(r.URL.Path, "/v1/responses"):
			responsesWire = true
			if decoded {
				forcedStream := requestBody["stream"] != true
				if forcedStream {
					requestBody["stream"] = true
				}
				toolsChanged := augmentFlatTools(requestBody)
				choiceChanged := sanitizeResponsesToolChoice(requestBody)
				if forcedStream || toolsChanged || choiceChanged {
					updated, marshalErr := json.Marshal(requestBody)
					if marshalErr != nil {
						http.Error(w, "invalid request body", http.StatusBadRequest)
						status = http.StatusBadRequest
						return
					}
					setRequestBody(r, updated)
				} else {
					setRequestBody(r, original)
				}
			}
		case strings.Contains(r.URL.Path, "/v1/chat/completions") && isMuseModel(requestedModel):
			museChat = true
			converted, marshalErr := json.Marshal(buildResponsesRequest(requestBody))
			if marshalErr != nil {
				http.Error(w, "invalid request body", http.StatusBadRequest)
				status = http.StatusBadRequest
				return
			}
			setRequestBody(r, converted)
		case strings.Contains(r.URL.Path, "/v1/chat/completions") && isJevModel(requestedModel):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"model requires the systemone wire, not supported by oc-zen"}}`)
			status = http.StatusBadRequest
			return
		default:
			if decoded {
				toolsChanged := augmentChatTools(requestBody)
				if strings.Contains(r.URL.Path, "/v1/chat/completions") {
					chatWire = true
					requestBody["stream"] = true
				}
				if toolsChanged || chatWire {
					updated, marshalErr := json.Marshal(requestBody)
					if marshalErr != nil {
						http.Error(w, "invalid request body", http.StatusBadRequest)
						status = http.StatusBadRequest
						return
					}
					setRequestBody(r, updated)
				} else {
					setRequestBody(r, original)
				}
			}
		}
		requestURI := r.URL.RequestURI()
		if museChat {
			requestURI = "/v1/responses"
			if r.URL.RawQuery != "" {
				requestURI += "?" + r.URL.RawQuery
			}
		}
		target := base + requestURI
		var bodyBytes []byte
		if hasBody(r) {
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "upstream request failed", http.StatusBadGateway)
				return
			}
			_ = r.Body.Close()
		}
		contentLength := r.ContentLength
		headers := r.Header.Clone()
		stripHopHeaders(headers)
		headers.Set("User-Agent", fingerprintUA)
		headers.Set("x-opencode-client", "desktop")
		headers.Set("x-opencode-project", "global")
		headers.Set("x-opencode-session", session)
		headers.Set("x-opencode-session-id", session)
		if apiKey != "" {
			headers.Set("Authorization", "Bearer "+apiKey)
		}
		headers.Set("x-opencode-request", requestID)
		headers.Set("Accept-Encoding", "identity")
		if r.Header.Get("Accept") == "" {
			headers.Set("Accept", "*/*")
		}
		if !museChat && strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
			streaming = true
		}
		loopCtx, cancel := context.WithTimeout(r.Context(), retryLoopBudget)
		defer cancel()
		replay := &responseReplayState{}
		var finalErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			attempts = attempt
			if err := loopCtx.Err(); err != nil {
				finalErr = err
				break
			}
			attemptCtx, cancelAttempt := context.WithCancel(r.Context())
			stopAttempt := context.AfterFunc(loopCtx, cancelAttempt)
			out, reqErr := http.NewRequestWithContext(attemptCtx, r.Method, target, bytes.NewReader(bodyBytes))
			if reqErr != nil {
				stopAttempt()
				cancelAttempt()
				finalErr = reqErr
				break
			}
			out.Header, out.ContentLength = headers.Clone(), contentLength
			resp, doErr := client.Do(out)
			stopAttempt()
			if doErr != nil {
				cancelAttempt()
				finalErr = doErr
				if r.Context().Err() != nil || attempt == maxAttempts || loopCtx.Err() != nil {
					break
				}
				retryLog(attempt, "transport", session)
				if !waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
					break
				}
				continue
			}
			if retryableStatus(resp.StatusCode) {
				finalErr = fmt.Errorf("upstream status %d", resp.StatusCode)
				delay := retryDelay(attempt, resp.Header, resp.StatusCode)
				_ = resp.Body.Close()
				cancelAttempt()
				if attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, delay) {
					retryLog(attempt, "status", session)
					continue
				}
				break
			}
			stripHopHeaders(resp.Header)
			if museChat && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					writer := newSerializedStreamWriter(w, clientKeepAliveInterval)
					resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
					attemptStatus, retryable := writeResponsesAsChat(writer, resp, requestedModel, true, replay)
					_ = resp.Body.Close()
					cancelAttempt()
					writer.Close()
					status = attemptStatus
					if retryable && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "stream_before_content", session)
						continue
					}
					if retryable {
						status = writeStreamInterrupted(w, true)
					}
					return
				}
				for key, values := range resp.Header {
					for _, value := range values {
						w.Header().Add(key, value)
					}
				}
				status, _ = writeResponsesAsChat(w, resp, requestedModel, false, replay)
				_ = resp.Body.Close()
				cancelAttempt()
				return
			}
			isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
			if chatWire && !clientStream && isSSE && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
				completion, aggErr := aggregateChatStream(resp.Body, requestedModel)
				_ = resp.Body.Close()
				cancelAttempt()
				if aggErr != nil {
					if errors.Is(aggErr, errEmptyChatStream) && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "empty_chat_stream", session)
						continue
					}
					status = writeJSONError(w, aggErr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(completion)
				status = http.StatusOK
				return
			}
			if responsesWire && !clientStream && isSSE && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
				completion, aggErr := aggregateResponsesStream(resp.Body)
				_ = resp.Body.Close()
				cancelAttempt()
				if aggErr != nil {
					if errors.Is(aggErr, errEmptyResponsesStream) && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "empty_responses_stream", session)
						continue
					}
					status = writeJSONError(w, aggErr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(completion)
				status = http.StatusOK
				return
			}

			if streaming && isSSE {
				attemptStatus, retryable := relayPassThroughStream(w, r, resp, session)
				cancelAttempt()
				status = attemptStatus
				if retryable && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
					retryLog(attempt, "stream_before_content", session)
					continue
				}
				if retryable {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, interruptedStreamPair)
					status = http.StatusBadGateway
				}
				return
			}

			for key, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			status = resp.StatusCode
			w.WriteHeader(status)
			_, _ = io.Copy(flushWriter{w}, resp.Body)
			_ = resp.Body.Close()
			cancelAttempt()
			return
		}
		if streaming {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, interruptedStreamPair)
		} else {
			writeJSONError(w, "upstream request failed")
		}
		_ = finalErr
	})
}

func retryableStatus(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504, 599:
		return true
	}
	return status >= 520 && status <= 527
}

func retryLog(attempt int, reason, session string) {
	log.Printf("retry attempt=%d reason=%s ses=%s", attempt, reason, session)
}

func retryDelay(attempt int, header http.Header, status int) time.Duration {
	delay := time.Duration(0)
	if attempt > 0 {
		if attempt-1 < len(retryBackoffs) {
			delay = retryBackoffs[attempt-1]
		} else if len(retryBackoffs) > 0 {
			step := attempt - len(retryBackoffs)
			if step > 10 {
				step = 10
			}
			multiplier := time.Duration(1 << uint(step))
			delay = retryBackoffs[len(retryBackoffs)-1] * multiplier
			if delay > 15*time.Second {
				delay = 15 * time.Second
			}
		}
	}
	if status == http.StatusTooManyRequests && header != nil {
		if value := header.Get("Retry-After"); value != "" {
			if seconds, err := time.ParseDuration(value + "s"); err == nil {
				delay = seconds
			} else if when, err := http.ParseTime(value); err == nil {
				delay = time.Until(when)
			}
			if delay < 0 {
				delay = 0
			}
			if delay > retryAfterCap {
				delay = retryAfterCap
			}
		}
	}
	return delay
}

func waitRetry(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err() == nil
	case <-ctx.Done():
		return false
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envSeconds(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		log.Printf("invalid %s=%q, keeping %s", name, value, fallback)
		return fallback
	}
	return time.Duration(seconds) * time.Second
}
func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	v, err := strconv.Atoi(value)
	if err != nil || v <= 0 {
		log.Printf("invalid %s=%q, keeping %d", name, value, fallback)
		return fallback
	}
	return v
}

func initStabilityKnobs() {
	retryLoopBudget = envSeconds("ZEN_RETRY_BUDGET", retryLoopBudget)
	responseHeaderTimeout = envSeconds("ZEN_RESPONSE_HEADER_TIMEOUT", responseHeaderTimeout)
	if responseHeaderTimeout < retryLoopBudget {
		responseHeaderTimeout = retryLoopBudget
	}
	upstreamIdleTimeout = envSeconds("ZEN_IDLE_TIMEOUT", upstreamIdleTimeout)
	clientKeepAliveInterval = envSeconds("ZEN_KEEPALIVE_INTERVAL", clientKeepAliveInterval)
	maxAttempts = envInt("ZEN_MAX_ATTEMPTS", maxAttempts)
	retryAfterCap = envSeconds("ZEN_RETRY_AFTER_CAP", retryAfterCap)
	maxSSELineBytes = envInt("ZEN_MAX_SSE_LINE_BYTES", maxSSELineBytes)
	sseErrorScanWindow = envInt("ZEN_SSE_ERROR_SCAN_WINDOW", sseErrorScanWindow)
	log.Printf("retry budget=%s response header timeout=%s idle timeout=%s keepalive=%s max attempts=%d retry after cap=%s max sse line=%d error scan window=%d",
		retryLoopBudget, responseHeaderTimeout, upstreamIdleTimeout, clientKeepAliveInterval, maxAttempts, retryAfterCap, maxSSELineBytes, sseErrorScanWindow)
}

func main() {
	initStabilityKnobs()

	mint := &idMint{}
	client := newUpstreamClient()
	server := &http.Server{
		Addr:    "0.0.0.0:" + envOr("ZEN_PORT", "8801"),
		Handler: proxyHandler(envOr("ZEN_UPSTREAM", "https://opencode.ai/zen"), client, mint),
		ConnContext: func(ctx context.Context, _ net.Conn) context.Context {
			session := mint.mint("ses_")
			log.Printf("new-conn ses=%s", session)
			return context.WithValue(ctx, contextKey{}, session)
		},
	}
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
