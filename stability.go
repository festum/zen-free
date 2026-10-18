package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	dialTimeout             = 15 * time.Second
	tlsHandshakeTimeout     = 10 * time.Second
	responseHeaderTimeout   = 300 * time.Second
	idleConnTimeout         = 50 * time.Second
	expectContinueTimeout   = 1 * time.Second
	maxIdleConns            = 32
	maxIdleConnsPerHost     = 16
	upstreamIdleTimeout     = 300 * time.Second
	clientKeepAliveInterval = 15 * time.Second
	maxAttempts             = 8
	retryBackoffs           = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	retryAfterCap           = 30 * time.Second
	retryLoopBudget         = 300 * time.Second
	maxSSELineBytes         = 16 * 1024 * 1024
	sseErrorScanWindow      = 64 * 1024
)

const interruptedStreamPair = "data: {\"error\":{\"message\":\"oc-zen: upstream stream interrupted\",\"type\":\"upstream_interrupted\"}}\n\ndata: [DONE]\n\n"

func newUpstreamClient() *http.Client {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 15 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport}
}

type idleWatchBody struct {
	body          io.ReadCloser
	timeout       time.Duration
	timer         *time.Timer
	done          chan struct{}
	once          sync.Once
	bodyCloseOnce sync.Once
	expired       chan struct{}
}

func watchUpstreamBody(body io.ReadCloser, timeout time.Duration, session string) *idleWatchBody {
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	w := &idleWatchBody{body: body, timeout: timeout, done: make(chan struct{}), expired: make(chan struct{})}
	w.timer = time.AfterFunc(timeout, func() {
		select {
		case <-w.done:
			return
		default:
		}
		close(w.expired)
		log.Printf("idle_watchdog after=%s ses=%s", timeout, session)
		w.closeBody()
	})
	return w
}

func (w *idleWatchBody) Read(p []byte) (int, error) {
	n, err := w.body.Read(p)
	if n > 0 && w.timer != nil {
		w.timer.Reset(w.timeout)
	}
	select {
	case <-w.expired:
		if err == nil {
			err = context.DeadlineExceeded
		}
	default:
	}
	return n, err
}

func (w *idleWatchBody) closeBody() { w.bodyCloseOnce.Do(func() { _ = w.body.Close() }) }

func (w *idleWatchBody) Close() error {
	w.once.Do(func() {
		close(w.done)
		if w.timer != nil {
			w.timer.Stop()
		}
	})
	w.closeBody()
	return nil
}

type serializedStreamWriter struct {
	w           http.ResponseWriter
	mu          sync.Mutex
	reset       chan struct{}
	stop        chan struct{}
	done        chan struct{}
	interval    time.Duration
	atBoundary  bool
	doneTracker sseDoneTracker
}

func newSerializedStreamWriter(w http.ResponseWriter, interval time.Duration) *serializedStreamWriter {
	s := &serializedStreamWriter{w: w, reset: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), interval: interval, atBoundary: true}
	go s.heartbeat()
	return s
}

func (s *serializedStreamWriter) Header() http.Header { return s.w.Header() }

func (s *serializedStreamWriter) WriteHeader(status int) {
	s.mu.Lock()
	s.w.WriteHeader(status)
	s.mu.Unlock()
}
func (s *serializedStreamWriter) heartbeat() {
	defer close(s.done)
	if s.interval <= 0 {
		<-s.stop
		return
	}
	timer := time.NewTimer(s.interval)
	defer timer.Stop()
	for {
		select {
		case <-timer.C:
			s.mu.Lock()
			if s.atBoundary && !s.doneTracker.sawDone {
				_, _ = io.WriteString(s.w, ": keep-alive\n\n")
				if f, ok := s.w.(http.Flusher); ok {
					f.Flush()
				}
			}
			s.mu.Unlock()
			timer.Reset(s.interval)
		case <-s.reset:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.interval)
		case <-s.stop:
			return
		}
	}
}

func (s *serializedStreamWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, err := s.w.Write(p)
	if n > 0 {
		s.doneTracker.observe(p[:n])
		s.atBoundary = bytes.HasSuffix(p[:n], []byte("\n\n"))
	}
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	s.mu.Unlock()
	select {
	case s.reset <- struct{}{}:
	default:
	}
	return n, err
}

func (s *serializedStreamWriter) Flush() {
	s.mu.Lock()
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
	s.mu.Unlock()
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

func (s *serializedStreamWriter) Close() {
	close(s.stop)
	<-s.done
}

type sseDoneTracker struct {
	sawDone bool
	tail    []byte
}

func (t *sseDoneTracker) observe(b []byte) {
	if t.sawDone || len(b) == 0 {
		return
	}
	if isSSEDone(b) || (len(t.tail) > 0 && isSSEDone(append(t.tail, b...))) {
		t.sawDone = true
		return
	}
	if len(b) >= 64 {
		t.tail = append(t.tail[:0], b[len(b)-64:]...)
	} else {
		t.tail = append(t.tail, b...)
		if len(t.tail) > 64 {
			t.tail = t.tail[len(t.tail)-64:]
		}
	}
}

type sseDoneWriter struct {
	w       io.Writer
	tracker sseDoneTracker
}

func (s *sseDoneWriter) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if n > 0 {
		s.tracker.observe(p[:n])
	}
	return n, err
}

func isSSEDone(b []byte) bool {
	return bytes.Contains(b, []byte("data: [DONE]")) ||
		bytes.Contains(b, []byte("data:[DONE]")) ||
		bytes.Contains(b, []byte(`"finish_reason":"stop"`)) ||
		bytes.Contains(b, []byte(`"finish_reason": "stop"`)) ||
		bytes.Contains(b, []byte(`"finish_reason":"length"`)) ||
		bytes.Contains(b, []byte(`"finish_reason": "length"`)) ||
		bytes.Contains(b, []byte(`"finish_reason":"tool_calls"`)) ||
		bytes.Contains(b, []byte(`"finish_reason": "tool_calls"`)) ||
		bytes.Contains(b, []byte(`"finish_reason":"content_filter"`)) ||
		bytes.Contains(b, []byte(`"finish_reason": "content_filter"`)) ||
		bytes.Contains(b, []byte(`"response.completed"`))
}

func isSSEError(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("event:")) {
			if strings.TrimSpace(string(line[len("event:"):])) == "error" {
				return true
			}
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.HasPrefix(payload, []byte(`{"error":`)) ||
				bytes.HasPrefix(payload, []byte(`{"type":"response.failed"`)) ||
				bytes.HasPrefix(payload, []byte(`{"type": "response.failed"`)) {
				return true
			}
		}
	}
	return false
}

func hasSSEData(b []byte) bool {
	for _, line := range bytes.Split(b, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			val := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(val) > 0 {
				return true
			}
		}
	}
	return false
}

var errSSELineTooLong = errors.New("upstream SSE line exceeds configured limit")

func readSSELine(reader *bufio.Reader) (string, error) {
	limit := maxSSELineBytes
	var line []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > limit {
			return "", fmt.Errorf("oc-zen: upstream SSE line exceeds %d bytes: %w", limit, errSSELineTooLong)
		}
		if err == nil {
			return string(line), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return string(line), err
		}
	}
}

type clientWriteTracker struct {
	w        io.Writer
	writeErr error
}

func (c *clientWriteTracker) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if err != nil && c.writeErr == nil {
		c.writeErr = err
	}
	return n, err
}

func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "broken pipe") || strings.Contains(s, "connection reset") || strings.Contains(s, "closed network connection")
}

func relayPassThroughStream(w http.ResponseWriter, r *http.Request, resp *http.Response, session string, filter toolCallFilter, responsesWire bool) (int, bool) {
	resp.Body = watchUpstreamBody(resp.Body, upstreamIdleTimeout, session)
	defer resp.Body.Close()

	prelude := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	var readErr error
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			prelude = append(prelude, chunk[:n]...)
		}
		if isSSEError(prelude) {
			return http.StatusBadGateway, true
		}
		if len(prelude) >= sseErrorScanWindow {
			break
		}
		if err != nil {
			readErr = err
			break
		}
	}
	if len(prelude) == 0 && readErr != nil {
		return http.StatusBadGateway, true
	}
	if readErr != nil && !hasSSEData(prelude) {
		return http.StatusBadGateway, true
	}

	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	writer := newSerializedStreamWriter(w, clientKeepAliveInterval)
	defer writer.Close()
	w.WriteHeader(resp.StatusCode)
	if !filter.enabled() {
		tracker := &clientWriteTracker{w: writer}
		doneWriter := &sseDoneWriter{w: tracker}
		written, writeErr := 0, error(nil)
		if len(prelude) > 0 {
			count, err := doneWriter.Write(prelude)
			written += count
			writeErr = err
		}
		if readErr == nil && writeErr == nil {
			count, err := io.Copy(doneWriter, resp.Body)
			written += int(count)
			writeErr = err
		} else if readErr != io.EOF && writeErr == nil {
			writeErr = readErr
		}
		if writeErr != nil && !doneWriter.tracker.sawDone {
			if tracker.writeErr != nil || isClientDisconnect(writeErr) {
				log.Printf("client_disconnected bytes_written=%d err=%v ses=%s", written, writeErr, session)
				return resp.StatusCode, false
			}
			if r.Context().Err() == nil {
				log.Printf("stream_interrupted bytes_written=%d err=%v ses=%s", written, writeErr, session)
				_, _ = io.WriteString(w, interruptedStreamPair)
			}
		}
		return resp.StatusCode, false
	}
	toolState := &streamToolFilter{filter: filter, responsesWire: responsesWire, hiddenIDs: make(map[string]bool), hiddenIndexes: make(map[int]bool), pendingTools: make(map[int]*pendingChatStreamTool)}
	tracker := &clientWriteTracker{w: writer}
	doneWriter := &sseDoneWriter{w: tracker}
	writeEvent := func(lines []string, raw []byte) (int, error) {
		dataLines := make([]string, 0, len(lines))
		metadata := make([]string, 0, len(lines))
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				value := strings.TrimPrefix(line, "data:")
				dataLines = append(dataLines, strings.TrimPrefix(value, " "))
			} else {
				metadata = append(metadata, line)
			}
		}
		data := []byte(strings.Join(dataLines, "\n"))
		if filter.enabled() && len(data) > 0 {
			filtered, keep := transformInjectedToolEvent(data, toolState)
			if !keep {
				return 0, nil
			}
			if bytes.Equal(filtered, data) {
				return doneWriter.Write(raw)
			}
			data = filtered
		} else if filter.enabled() {
			return doneWriter.Write(raw)
		}
		var event strings.Builder
		for _, line := range metadata {
			event.WriteString(line)
			event.WriteString("\n")
		}
		if len(dataLines) > 0 {
			event.WriteString("data: ")
			event.Write(data)
			event.WriteString("\n")
		}
		event.WriteString("\n")
		return doneWriter.Write([]byte(event.String()))
	}

	reader := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(prelude), resp.Body), 64*1024)
	var lines []string
	var rawEvent []byte
	written := 0
	var writeErr error
	for {
		line, err := readSSELine(reader)
		if len(line) > 0 {
			rawEvent = append(rawEvent, line...)
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				count, eventErr := writeEvent(lines, rawEvent)
				written += count
				lines = lines[:0]
				rawEvent = rawEvent[:0]
				if eventErr != nil {
					writeErr = eventErr
					break
				}
			} else {
				lines = append(lines, trimmed)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				writeErr = err
			}
			break
		}
	}
	if writeErr == nil && (len(lines) > 0 || len(rawEvent) > 0) {
		count, err := writeEvent(lines, rawEvent)
		written += count
		writeErr = err
	}
	if writeErr != nil && !doneWriter.tracker.sawDone {
		if tracker.writeErr != nil || isClientDisconnect(writeErr) {
			log.Printf("client_disconnected bytes_written=%d err=%v ses=%s", written, writeErr, session)
			return resp.StatusCode, false
		}
		if r.Context().Err() == nil {
			log.Printf("stream_interrupted bytes_written=%d err=%v ses=%s", written, writeErr, session)
			_, _ = io.WriteString(w, interruptedStreamPair)
		}
	}
	return resp.StatusCode, false

}

type streamToolFilter struct {
	filter        toolCallFilter
	responsesWire bool
	hiddenIDs     map[string]bool
	hiddenIndexes map[int]bool
	pendingTools  map[int]*pendingChatStreamTool
	pendingOrder  []int
	callerSeen    bool
	hiddenCall    bool
}

type pendingChatStreamTool struct {
	call             map[string]any
	name             strings.Builder
	arguments        strings.Builder
	visible          bool
	emittedName      int
	emittedArguments int
}

func transformInjectedToolEvent(data []byte, state *streamToolFilter) ([]byte, bool) {
	if string(data) == "[DONE]" {
		return data, true
	}
	var event map[string]any
	if json.Unmarshal(data, &event) != nil {
		return data, true
	}
	modified := false
	if state.responsesWire {
		switch event["type"] {
		case "response.output_item.added", "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" {
				callID := stringValue(item["call_id"])
				itemID := stringValue(item["id"])
				if state.filter.hides(stringValue(item["name"])) {
					if callID != "" {
						state.hiddenIDs[callID] = true
					}
					if itemID != "" {
						state.hiddenIDs[itemID] = true
					}
					return nil, false
				}
				state.callerSeen = true
			}
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			callID := stringValue(event["call_id"])
			if callID == "" {
				callID = stringValue(event["item_id"])
			}
			if state.hiddenIDs[callID] {
				return nil, false
			}
		case "response.completed":
			response, _ := event["response"].(map[string]any)
			if response != nil {
				output, _ := response["output"].([]any)
				filtered := output[:0]
				for _, raw := range output {
					item, _ := raw.(map[string]any)
					if item != nil && item["type"] == "function_call" && state.filter.hides(stringValue(item["name"])) {
						modified = true
						continue
					}
					filtered = append(filtered, raw)
				}
				if modified {
					response["output"] = filtered
				}
			}
		}
	} else {
		modified = transformChatToolEvents(event, state)
	}
	if !modified {
		return data, true
	}
	filtered, err := json.Marshal(event)
	if err != nil {
		return data, true
	}
	return filtered, true
}
func transformChatToolEvents(event map[string]any, state *streamToolFilter) bool {
	choices, _ := event["choices"].([]any)
	modified := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil {
			continue
		}
		final := choice["finish_reason"] != nil
		removed := false
		for _, key := range []string{"delta", "message"} {
			body, _ := choice[key].(map[string]any)
			calls, _ := body["tool_calls"].([]any)
			if body == nil || len(calls) == 0 {
				continue
			}
			filtered := make([]any, 0, len(calls))
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				if call == nil {
					filtered = append(filtered, rawCall)
					continue
				}
				slot, hasSlot := integerValue(call["index"])
				function, _ := call["function"].(map[string]any)
				if function == nil {
					function = map[string]any{}
				}
				if !hasSlot {
					if state.filter.hides(stringValue(function["name"])) {
						removed, state.hiddenCall = true, true
						continue
					}
					filtered = append(filtered, rawCall)
					continue
				}
				if state.hiddenIndexes[slot] {
					removed, state.hiddenCall = true, true
					continue
				}
				pending := state.pendingTools[slot]
				if pending == nil {
					state.pendingOrder = append(state.pendingOrder, slot)
					pending = &pendingChatStreamTool{call: map[string]any{"index": slot}}
					state.pendingTools[slot] = pending
				}
				for key, value := range call {
					if key != "index" && key != "function" {
						pending.call[key] = value
					}
				}
				functionFields, _ := pending.call["function"].(map[string]any)
				if functionFields == nil {
					functionFields = make(map[string]any)
					pending.call["function"] = functionFields
				}
				for key, value := range function {
					if key != "name" && key != "arguments" {
						functionFields[key] = value
					}
				}
				pending.name.WriteString(stringValue(function["name"]))
				pending.arguments.WriteString(stringValue(function["arguments"]))
				name := pending.name.String()
				if state.filter.hides(name) && final {
					state.hiddenIndexes[slot] = true
					if id := stringValue(pending.call["id"]); id != "" {
						state.hiddenIDs[id] = true
					}
					delete(state.pendingTools, slot)
					removed, state.hiddenCall = true, true
					continue
				}
				if !pending.visible {
					possibleInjectedName := false
					for injectedName := range state.filter.injected {
						if strings.HasPrefix(injectedName, name) {
							possibleInjectedName = true
							break
						}
					}
					if name == "" || possibleInjectedName && !final {
						removed = true
						continue
					}
					pending.visible = true
					state.callerSeen = true
				}
				out := map[string]any{"index": slot}
				for key, value := range pending.call {
					if key != "index" && key != "function" {
						out[key] = value
					}
				}
				functionFields, _ = pending.call["function"].(map[string]any)
				outFunction := make(map[string]any, len(functionFields)+2)
				for key, value := range functionFields {
					outFunction[key] = value
				}
				if pending.emittedName == 0 && name != "" {
					outFunction["name"] = name
					pending.emittedName = pending.name.Len()
				} else if pending.name.Len() > pending.emittedName {
					outFunction["name"] = name[pending.emittedName:]
					pending.emittedName = pending.name.Len()
				}
				if pending.arguments.Len() > pending.emittedArguments {
					outFunction["arguments"] = pending.arguments.String()[pending.emittedArguments:]
					pending.emittedArguments = pending.arguments.Len()
				}
				out["function"] = outFunction
				filtered = append(filtered, out)
			}
			modified = true
			if len(filtered) == 0 {
				delete(body, "tool_calls")
			} else {
				body["tool_calls"] = filtered
			}
		}
		if final {
			for _, slot := range state.pendingOrder {
				pending := state.pendingTools[slot]
				if pending == nil || pending.visible {
					continue
				}
				name := pending.name.String()
				delete(state.pendingTools, slot)
				if name == "" {
					continue
				}
				if state.filter.hides(name) {
					state.hiddenIndexes[slot] = true
					if id := stringValue(pending.call["id"]); id != "" {
						state.hiddenIDs[id] = true
					}
					removed, state.hiddenCall = true, true
					continue
				}
				body, _ := choice["delta"].(map[string]any)
				if body == nil {
					body = map[string]any{}
					choice["delta"] = body
				}
				calls, _ := body["tool_calls"].([]any)
				call := map[string]any{"index": slot}
				for key, value := range pending.call {
					if key != "index" && key != "function" {
						call[key] = value
					}
				}
				functionFields, _ := pending.call["function"].(map[string]any)
				function := make(map[string]any, len(functionFields)+2)
				for key, value := range functionFields {
					function[key] = value
				}
				function["name"] = name
				function["arguments"] = pending.arguments.String()
				call["function"] = function
				body["tool_calls"] = append(calls, call)
				state.callerSeen = true
				modified = true
			}
		}
		if (removed || state.hiddenCall) && choice["finish_reason"] == "tool_calls" && !state.callerSeen {
			choice["finish_reason"] = "stop"
			modified = true
		}
	}
	return modified
}

func integerValue(value any) (int, bool) {
	switch number := value.(type) {
	case float64:
		return int(number), true
	case int:
		return number, true
	default:
		return 0, false
	}
}
