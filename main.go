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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var opencodeVersion = "1.18.35"
var opencodeProjectID = mintProjectID()

var fingerprintUA = buildFingerprintUA()

func mintProjectID() string {
	var value [20]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic(fmt.Errorf("generate project id: %w", err))
	}
	return hex.EncodeToString(value[:])
}
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

func toolName(tool map[string]any) string {
	if name, ok := tool["name"].(string); ok {
		return name
	}
	function, _ := tool["function"].(map[string]any)
	name, _ := function["name"].(string)
	return name
}

func cloneToolValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(value))
		for key, child := range value {
			cloned[key] = cloneToolValue(child)
		}
		return cloned
	case []any:
		cloned := make([]any, len(value))
		for i, child := range value {
			cloned[i] = cloneToolValue(child)
		}
		return cloned
	default:
		return value
	}
}

func inferencePath(upstream string) bool {
	parsed, err := url.Parse(upstream)
	if err != nil {
		return false
	}
	for _, segment := range strings.Split(strings.Trim(parsed.Path, "/"), "/") {
		if segment == "inference" {
			return true
		}
	}
	return false
}

func openAIInferenceRequestURI(upstream, requestURI string) string {
	parsed, err := url.Parse(upstream)
	if err != nil || strings.TrimRight(parsed.Path, "/") != "/inference" {
		return requestURI
	}

	path, query, hasQuery := strings.Cut(requestURI, "?")
	if path != "/v1/chat/completions" && path != "/v1/responses" {
		return requestURI
	}

	path = "/openai" + path
	if hasQuery {
		return path + "?" + query
	}
	return path
}

func capturedTool(name string, nested bool) map[string]any {
	for _, raw := range capturedOpenCodeTools {
		tool := raw.(map[string]any)
		function := tool["function"].(map[string]any)
		if function["name"] != name {
			continue
		}
		if nested {
			return cloneToolValue(tool).(map[string]any)
		}
		flat := map[string]any{"type": "function"}
		for key, value := range cloneToolValue(function).(map[string]any) {
			flat[key] = value
		}
		return flat
	}
	return nil
}

func augmentTools(body map[string]any, nested, normalize bool) map[string]bool {
	tools, _ := body["tools"].([]any)
	if !normalize || len(tools) == 0 {
		if len(tools) > 0 {
			return nil
		}
		injected := make(map[string]bool, len(capturedOpenCodeTools))
		if nested {
			tools = make([]any, len(capturedOpenCodeTools))
			for i, raw := range capturedOpenCodeTools {
				tools[i] = cloneToolValue(raw)
				injected[toolName(tools[i].(map[string]any))] = true
			}
		} else {
			tools = make([]any, len(capturedOpenCodeTools))
			for i, raw := range capturedOpenCodeTools {
				function := raw.(map[string]any)["function"].(map[string]any)
				tools[i] = capturedTool(stringValue(function["name"]), false)
				injected[stringValue(function["name"])] = true
			}
		}
		body["tools"] = tools
		return injected
	}

	coreNames := make(map[string]bool, len(capturedOpenCodeTools))
	for _, raw := range capturedOpenCodeTools {
		coreNames[toolName(raw.(map[string]any))] = true
	}
	filter := make(map[string]bool, len(coreNames))
	seenRequired := make(map[string]bool, 4)
	for i, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := toolName(tool)
		if !coreNames[name] {
			continue
		}
		tools[i] = capturedTool(name, nested)
		seenRequired[name] = true
	}
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if seenRequired[name] {
			continue
		}
		tools = append(tools, capturedTool(name, nested))
		filter[name] = true
	}
	body["tools"] = tools
	return filter
}

func augmentChatTools(body map[string]any, normalize bool) map[string]bool {
	return augmentTools(body, true, normalize)
}

func augmentFlatTools(body map[string]any, normalize bool) map[string]bool {
	return augmentTools(body, false, normalize)
}

type toolCallFilter struct {
	injected map[string]bool
}

func (f toolCallFilter) hides(name string) bool {
	return f.injected[name]
}

func (f toolCallFilter) enabled() bool {
	return len(f.injected) > 0
}

func requestToolFilter(injected map[string]bool) toolCallFilter {
	return toolCallFilter{injected: injected}
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

func isReasoningRequiredError(b []byte) bool {
	return bytes.Contains(b, []byte("reasoning_content")) && bytes.Contains(b, []byte("thinking mode"))
}

func repairReasoningContent(body []byte) ([]byte, bool) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, false
	}
	var messages []json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil {
		return nil, false
	}
	changed := false
	for i, raw := range messages {
		var message map[string]json.RawMessage
		if json.Unmarshal(raw, &message) != nil || message == nil {
			continue
		}
		var role string
		if json.Unmarshal(message["role"], &role) != nil || role != "assistant" {
			continue
		}
		if _, exists := message["reasoning_content"]; exists {
			continue
		}
		message["reasoning_content"] = json.RawMessage(`""`)
		updated, err := json.Marshal(message)
		if err != nil {
			return nil, false
		}
		messages[i] = updated
		changed = true
	}
	if !changed {
		return nil, false
	}
	updated, err := json.Marshal(messages)
	if err != nil {
		return nil, false
	}
	request["messages"] = updated
	repaired, err := json.Marshal(request)
	if err != nil {
		return nil, false
	}
	return repaired, true
}

func proxyHandler(upstream string, client *http.Client, mint *idMint) http.Handler {
	return proxyHandlerWithRelay(upstream, client, mint, parseRelayURL(os.Getenv("ZEN_RELAY_URL")))
}

func proxyHandlerWithRelay(upstream string, client *http.Client, mint *idMint, relay relayConfig) http.Handler {
	base := strings.TrimRight(upstream, "/")
	egressClient := client
	if relay.enabled() {
		relayClient := *client
		relayClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		egressClient = &relayClient
	}
	apiKey := os.Getenv("OPENCODE_GO_API_KEY")
	inference := inferencePath(upstream)
	debugDumps := debugDumpAllowance{enabled: os.Getenv("ZEN_DEBUG_DUMP") != ""}
	debugRaw := os.Getenv("ZEN_DEBUG_DUMP_RAW") == "1"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if relay.err != nil {
			http.Error(w, relay.err.Error(), http.StatusInternalServerError)
			return
		}
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
		museChat, streaming, chatWire, responsesWire, clientStream, responsesToChat, nativeFallback := false, false, false, false, false, false, false
		var injectedTools map[string]bool
		var toolFilter toolCallFilter
		requestedModel := ""
		var nativeResponses []byte
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
				injectedTools = augmentFlatTools(requestBody, inference)
				toolFilter = requestToolFilter(injectedTools)
				nativeResponsesBody := original
				if forcedStream || inference || len(injectedTools) > 0 {
					updated, marshalErr := json.Marshal(requestBody)
					if marshalErr != nil {
						http.Error(w, "invalid request body", http.StatusBadRequest)
						status = http.StatusBadRequest
						return
					}
					nativeResponsesBody = updated
				}
				setRequestBody(r, nativeResponsesBody)
				if !isMuseModel(requestedModel) {
					chatBody := buildChatCompletionsRequest(requestBody)
					chatBody["stream"] = true
					converted, marshalErr := json.Marshal(chatBody)
					if marshalErr != nil {
						http.Error(w, "invalid request body", http.StatusBadRequest)
						status = http.StatusBadRequest
						return
					}
					setRequestBody(r, converted)
					responsesToChat = true
					nativeResponses = nativeResponsesBody
				}
			}
		case strings.Contains(r.URL.Path, "/v1/chat/completions") && isMuseModel(requestedModel):
			museChat = true
			callerTools, _ := requestBody["tools"].([]any)
			injectedTools = augmentChatTools(requestBody, inference)
			toolFilter = requestToolFilter(injectedTools)
			if len(callerTools) == 0 {
				if _, choiceProvided := requestBody["tool_choice"]; !choiceProvided {
					requestBody["tool_choice"] = "none"
				}
			}
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
				callerTools, _ := requestBody["tools"].([]any)
				injectedTools = augmentChatTools(requestBody, inference)
				if strings.Contains(r.URL.Path, "/v1/chat/completions") && len(callerTools) == 0 {
					if _, choiceProvided := requestBody["tool_choice"]; !choiceProvided {
						requestBody["tool_choice"] = "none"
					}
				}
				toolFilter = requestToolFilter(injectedTools)
				if strings.Contains(r.URL.Path, "/v1/chat/completions") {
					chatWire = true
					requestBody["stream"] = true
				}
				if len(injectedTools) > 0 || chatWire || inference {
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
		} else if responsesToChat {
			requestURI = "/v1/chat/completions"
			if r.URL.RawQuery != "" {
				requestURI += "?" + r.URL.RawQuery
			}
		}

		target := base + openAIInferenceRequestURI(upstream, requestURI)
		var bodyBytes []byte
		if hasBody(r) {
			bodyBytes, err = io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "upstream request failed", http.StatusBadGateway)
				return
			}
			_ = r.Body.Close()
		}
		if !decoded {
			original = bodyBytes
		}
		contentLength := r.ContentLength
		headers := r.Header.Clone()
		stripHopHeaders(headers)
		if relay.enabled() {
			headers.Del("x-relay-target")
			headers.Del("x-relay-path")
		}
		headers.Set("User-Agent", fingerprintUA)
		headers.Set("x-opencode-client", "desktop")
		headers.Set("x-opencode-project", opencodeProjectID)
		headers.Set("x-opencode-session", session)
		headers.Set("x-opencode-session-id", session)
		headers.Set("x-session-affinity", session)
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
		reasoningRepaired := false
		attemptLimit := maxAttempts
		if relay.enabled() {
			attemptLimit = 1
		}
		for attempt := 1; attempt <= attemptLimit; attempt++ {
			attempts = attempt
			if err := loopCtx.Err(); err != nil {
				finalErr = err
				break
			}
			attemptCtx, cancelAttempt := context.WithCancel(r.Context())
			stopAttempt := context.AfterFunc(loopCtx, cancelAttempt)
			requestURL := target
			relayPath := ""
			if relay.enabled() {
				relayRequest, path, relayErr := relay.requestURL(target)
				if relayErr != nil {
					stopAttempt()
					cancelAttempt()
					finalErr = relayErr
					break
				}
				requestURL, relayPath = relayRequest.String(), path
			}
			out, reqErr := http.NewRequestWithContext(attemptCtx, r.Method, requestURL, bytes.NewReader(bodyBytes))
			if reqErr != nil {
				stopAttempt()
				cancelAttempt()
				finalErr = reqErr
				break
			}
			out.Header, out.ContentLength = headers.Clone(), contentLength
			if relay.enabled() {
				out.Header.Set("x-relay-target", relayTarget)
				out.Header.Set("x-relay-path", relayPath)
			}
			resp, doErr := egressClient.Do(out)
			if doErr != nil || !debugDumps.acquire(resp.StatusCode) {
				stopAttempt()
			} else {
				upstreamURL, _ := url.Parse(target)
				captureUpstream4xx(loopCtx, cancelAttempt, resp, original, bodyBytes, map[string]any{
					"request_id":    requestID,
					"session":       session,
					"attempt":       attempt,
					"client_path":   r.URL.Path,
					"upstream_path": upstreamURL.Path,
					"model":         requestedModel,
					"relay":         relay.enabled(),
					"status":        resp.StatusCode,
				}, debugRaw)
				stopAttempt()
				debugDumps.finish()
			}
			if doErr != nil {
				cancelAttempt()
				finalErr = doErr
				if relay.enabled() || r.Context().Err() != nil || attempt == maxAttempts || loopCtx.Err() != nil {
					break
				}
				retryLog(attempt, "transport", session)
				if !waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
					break
				}
				continue
			}
			if retryableStatus(resp.StatusCode) && !relay.enabled() {
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
			var errBytes []byte
			var errorReadErr error
			if resp.StatusCode == http.StatusBadRequest {
				errBytes, errorReadErr = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
				if errorReadErr == nil && !reasoningRepaired && isReasoningRequiredError(errBytes) {
					if repaired, changed := repairReasoningContent(bodyBytes); changed {
						bodyBytes = repaired
						contentLength = int64(len(bodyBytes))
						reasoningRepaired = true
						attemptLimit++
						_ = resp.Body.Close()
						cancelAttempt()
						continue
					}
				}
				resp.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(errBytes), resp.Body), resp.Body}
			}
			stripHopHeaders(resp.Header)
			if responsesToChat && !nativeFallback && resp.StatusCode == http.StatusBadRequest {
				fallbackBody, readErr := errBytes, errorReadErr
				_ = resp.Body.Close()
				cancelAttempt()
				if !relay.enabled() && readErr == nil && bytes.Contains(fallbackBody, []byte("ModelProtocolUnsupported")) {
					nativeFallback = true
					responsesToChat = false
					responsesWire = true
					bodyBytes = nativeResponses
					contentLength = int64(len(nativeResponses))
					nativeRequestURI := "/v1/responses"
					if r.URL.RawQuery != "" {
						nativeRequestURI += "?" + r.URL.RawQuery
					}
					target = base + openAIInferenceRequestURI(upstream, nativeRequestURI)
					retryLog(attempt, "protocol_fallback", session)
					continue
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write(fallbackBody)
				status = http.StatusBadRequest
				return
			}
			if museChat && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					writer := newSerializedStreamWriter(w, clientKeepAliveInterval)
					resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
					attemptStatus, retryable := writeResponsesAsChat(writer, resp, requestedModel, true, replay, toolFilter)
					_ = resp.Body.Close()
					cancelAttempt()
					writer.Close()
					status = attemptStatus
					if retryable && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
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
				status, _ = writeResponsesAsChat(w, resp, requestedModel, false, replay, toolFilter)
				cancelAttempt()
				return
			}
			isSSE := strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
			if responsesToChat && resp.StatusCode >= 200 && resp.StatusCode < 300 && isSSE {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					writer := newSerializedStreamWriter(w, clientKeepAliveInterval)
					resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
					attemptStatus, retryable := writeChatStreamAsResponses(writer, resp.Body, requestedModel, toolFilter)
					_ = resp.Body.Close()
					cancelAttempt()
					writer.Close()
					status = attemptStatus
					if retryable && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "stream_before_content", session)
						continue
					}
					if retryable {
						status = writeStreamInterrupted(w, true)
					}
					return
				}
				resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
				completion, aggErr := aggregateChatStream(resp.Body, requestedModel)
				_ = resp.Body.Close()
				cancelAttempt()
				if aggErr != nil {
					if errors.Is(aggErr, errEmptyChatStream) && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "empty_chat_stream", session)
						continue
					}
					status = writeJSONError(w, aggErr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if toolFilter.enabled() {
					stripChatCompletionToolCalls(completion, toolFilter)
				}
				_ = json.NewEncoder(w).Encode(chatCompletionToResponse(completion))
				return
			}
			if chatWire && !clientStream && isSSE && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
				completion, aggErr := aggregateChatStream(resp.Body, requestedModel)
				_ = resp.Body.Close()
				cancelAttempt()
				if aggErr != nil {
					if errors.Is(aggErr, errEmptyChatStream) && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "empty_chat_stream", session)
						continue
					}
					status = writeJSONError(w, aggErr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if toolFilter.enabled() {
					stripChatCompletionToolCalls(completion, toolFilter)
				}
				_ = json.NewEncoder(w).Encode(completion)
				return
			}
			if responsesWire && !clientStream && isSSE && resp.StatusCode >= 200 && resp.StatusCode < 300 {
				resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
				completion, aggErr := aggregateResponsesStream(resp.Body)
				_ = resp.Body.Close()
				cancelAttempt()
				if aggErr != nil {
					if errors.Is(aggErr, errEmptyResponsesStream) && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
						retryLog(attempt, "empty_responses_stream", session)
						continue
					}
					status = writeJSONError(w, aggErr.Error())
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				if toolFilter.enabled() {
					stripResponsesToolCalls(completion, toolFilter)
				}
				_ = json.NewEncoder(w).Encode(completion)
				status = http.StatusOK
				return
			}

			if streaming && isSSE {
				attemptStatus, retryable := relayPassThroughStream(w, r, resp, session, toolFilter, responsesWire)
				cancelAttempt()
				status = attemptStatus
				if retryable && !relay.enabled() && attempt < maxAttempts && r.Context().Err() == nil && waitRetry(loopCtx, retryDelay(attempt, nil, 0)) {
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

			status = resp.StatusCode
			if toolFilter.enabled() && status >= 200 && status < 300 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
				responseBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					cancelAttempt()
					status = writeJSONError(w, "upstream response failed")
					return
				}
				responseBody, _ = filterToolCallsInJSON(responseBody, toolFilter)
				for key, values := range resp.Header {
					for _, value := range values {
						if !strings.EqualFold(key, "Content-Length") {
							w.Header().Add(key, value)
						}
					}
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
				w.WriteHeader(status)
				_, _ = w.Write(responseBody)
				cancelAttempt()
				return
			}
			for key, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
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

func filterToolCallsInJSON(body []byte, filter toolCallFilter) ([]byte, bool) {
	if !filter.enabled() {
		return body, false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return body, false
	}
	if _, ok := payload["choices"].([]any); ok {
		stripChatCompletionToolCalls(payload, filter)
	} else if _, ok := payload["output"].([]any); ok {
		stripResponsesToolCalls(payload, filter)
	} else {
		return body, false
	}
	filtered, err := json.Marshal(payload)
	if err != nil {
		return body, false
	}
	return filtered, true
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

	relay := parseRelayURL(os.Getenv("ZEN_RELAY_URL"))
	if relay.err != nil {
		log.Fatal(relay.err)
	}
	mint := &idMint{}
	client := newUpstreamClient()
	server := &http.Server{
		Addr:    "0.0.0.0:" + envOr("ZEN_PORT", "8801"),
		Handler: proxyHandlerWithRelay(envOr("ZEN_UPSTREAM", "https://opencode.ai/inference"), client, mint, relay),
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
