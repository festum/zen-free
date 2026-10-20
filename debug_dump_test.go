package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var debugLogMu sync.Mutex

func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	debugLogMu.Lock()
	previous := log.Writer()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() {
		log.SetOutput(previous)
		debugLogMu.Unlock()
	})
	return &logs
}

func debugRecords(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(logs, "\n") {
		_, body, found := strings.Cut(line, "zen_debug_dump ")
		if !found {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(body), &record); err != nil {
			t.Fatalf("invalid JSON dump: %v", err)
		}
		records = append(records, record)
	}
	return records
}

func TestDebugDumpGate(t *testing.T) {
	logs := captureDebugLogs(t)
	t.Setenv("OPENCODE_GO_API_KEY", "authorization-secret")
	for _, relayMode := range []bool{false, true} {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			for _, status := range []int{http.StatusBadRequest, http.StatusOK} {
				t.Run(fmt.Sprintf("relay=%t%s/status=%d", relayMode, path, status), func(t *testing.T) {
					const inbound = `{"model":"test-model","stream":true,"input":"content-secret","messages":[{"role":"assistant","content":"content-secret","reasoning_content":"reasoning-secret","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"arguments-secret"}}]}]}`
					const response = "{\"error\":\"error-body-secret\\nmessage\"}\n"
					var submitted, ids []string
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						submitted = append(submitted, string(body))
						ids = append(ids, r.Header.Get("x-opencode-request"))
						w.Header().Set("Authorization", "response-auth-secret")
						w.WriteHeader(status)
						_, _ = io.WriteString(w, response)
					}))
					defer server.Close()
					upstream, relay := server.URL, relayConfig{}
					if relayMode {
						upstream, relay = "https://opencode.ai/inference", parseRelayURL(server.URL)
					}
					for _, test := range []struct {
						name string
						main string
						raw  string
					}{
						{name: "off"},
						{name: "raw alone", raw: "1"},
						{name: "summary", main: "0"},
						{name: "raw zero", main: "1", raw: "0"},
						{name: "raw true", main: "1", raw: "true"},
						{name: "raw whitespace", main: "1", raw: " 1"},
						{name: "raw", main: "1", raw: "1"},
					} {
						t.Run(test.name, func(t *testing.T) {
							logs.Reset()
							t.Setenv("ZEN_DEBUG_DUMP", test.main)
							t.Setenv("ZEN_DEBUG_DUMP_RAW", test.raw)
							before := len(submitted)
							handler := proxyHandlerWithRelay(upstream, server.Client(), &idMint{}, relay)
							t.Setenv("ZEN_DEBUG_DUMP", "changed-after-construction")
							t.Setenv("ZEN_DEBUG_DUMP_RAW", "changed-after-construction")
							request := httptest.NewRequest(http.MethodPost, path+"?secret=query-secret", strings.NewReader(inbound))
							request.Header.Set("Content-Type", "application/json")
							request.Header.Set("Authorization", "client-auth-secret")
							request.Header.Set("Proxy-Authorization", "client-proxy-secret")
							recorder := httptest.NewRecorder()
							handler.ServeHTTP(recorder, request)
							if recorder.Code != status || recorder.Body.String() != response || len(submitted) != before+1 {
								t.Fatalf("response = %d %q, attempts=%d", recorder.Code, recorder.Body.String(), len(submitted)-before)
							}
							if submitted[len(submitted)-1] != submitted[0] {
								t.Fatal("gates changed submitted upstream bytes")
							}
							records := debugRecords(t, logs.String())
							if test.main == "" || status == http.StatusOK {
								if len(records) != 0 {
									t.Fatalf("unexpected records: %d", len(records))
								}
								return
							}
							if len(records) != 1 {
								t.Fatalf("records = %d", len(records))
							}
							record := records[0]
							encoded, err := json.Marshal(record)
							if err != nil {
								t.Fatal(err)
							}
							for _, secret := range []string{"authorization-secret", "client-auth-secret", "client-proxy-secret", "response-auth-secret", "query-secret", server.URL} {
								if bytes.Contains(encoded, []byte(secret)) {
									t.Fatalf("diagnostic disclosed %q", secret)
								}
							}
							raw := test.raw == "1"
							if record["schema_version"] != float64(2) || record["raw_enabled"] != raw || record["request_id"] != ids[len(ids)-1] {
								t.Fatalf("schema or correlation mismatch: %v", record)
							}
							if record["client_path"] != path || record["relay"] != relayMode || record["attempt"] != float64(1) || record["status"] != float64(status) || record["model"] != "test-model" || record["session"] == "" {
								t.Fatalf("metadata mismatch: %v", record)
							}
							wantPath := "/v1/chat/completions"
							if relayMode {
								wantPath = "/inference/openai" + wantPath
							}
							if record["upstream_path"] != wantPath {
								t.Fatalf("upstream path = %v", record["upstream_path"])
							}
							if raw {
								if record["client_body"] != inbound || record["upstream_body"] != submitted[len(submitted)-1] || record["error_body"] != response {
									t.Fatal("raw bodies differ from wire bytes")
								}
							} else {
								for _, key := range []string{"client_body", "upstream_body", "error_body"} {
									if _, exists := record[key]; exists {
										t.Fatalf("raw field %s present", key)
									}
								}
								for _, secret := range []string{"content-secret", "reasoning-secret", "arguments-secret", "error-body-secret"} {
									if bytes.Contains(encoded, []byte(secret)) {
										t.Fatalf("summary disclosed %q", secret)
									}
								}
							}
						})
					}
				})
			}
		}
	}
}

func TestDebugDumpBounds(t *testing.T) {
	logs := captureDebugLogs(t)
	for _, size := range []int{debugDumpLimit - 1, debugDumpLimit, debugDumpLimit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			logs.Reset()
			body := bytes.Repeat([]byte("\n"), size)
			resp := &http.Response{Body: io.NopCloser(bytes.NewReader(body))}
			dumpUpstream4xx(resp, body, body, nil, true)
			records := debugRecords(t, logs.String())
			if len(records) != 1 || strings.Count(logs.String(), "\n") != 1 {
				t.Fatal("dump must occupy one JSON-escaped line")
			}
			for _, name := range []string{"client_body", "upstream_body", "error_body"} {
				limit := debugDumpRawLimit
				if name == "error_body" {
					limit = debugDumpLimit
				}
				if len(records[0][name].(string)) != min(size, limit) || records[0][name+"_bytes"] != float64(size) || records[0][name+"_truncated"] != (size > limit) {
					t.Fatalf("incorrect %s bounds", name)
				}
			}
			replayed, err := io.ReadAll(resp.Body)
			if err != nil || !bytes.Equal(replayed, body) {
				t.Fatalf("replay = %d bytes, %v", len(replayed), err)
			}
		})
	}
	t.Run("observed response prefix", func(t *testing.T) {
		logs.Reset()
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 128*1024)))}
		dumpUpstream4xx(resp, nil, nil, nil, true)
		record := debugRecords(t, logs.String())[0]
		if record["error_body_bytes"] != float64(debugDumpLimit+1) || record["error_body_truncated"] != true {
			t.Fatal("error length must describe the observed prefix")
		}
	})
	t.Run("concurrent allowance", func(t *testing.T) {
		logs.Reset()
		t.Setenv("ZEN_DEBUG_DUMP", "1")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "bounded")
		}))
		defer server.Close()
		handler := proxyHandlerWithRelay(server.URL, server.Client(), &idMint{}, relayConfig{})
		var wg sync.WaitGroup
		for range 48 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("raw"))
				handler.ServeHTTP(httptest.NewRecorder(), request)
			}()
		}
		wg.Wait()
		if len(debugRecords(t, logs.String())) != debugDumpMaxRecords || strings.Count(logs.String(), "zen_debug_dump_limit_reached") != 1 {
			t.Fatal("incorrect concurrent record allowance")
		}
	})
	t.Run("unadmitted responses stay untouched", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			enabled bool
			status  int
		}{
			{name: "disabled", status: 400},
			{name: "success", enabled: true, status: 200},
			{name: "server error", enabled: true, status: 500},
		} {
			t.Run(test.name, func(t *testing.T) {
				logs.Reset()
				allowance := debugDumpAllowance{enabled: test.enabled}
				if allowance.acquire(test.status) || logs.Len() != 0 {
					t.Fatal("unadmitted response acquired diagnostic resources")
				}
			})
		}
		logs.Reset()
		allowance := debugDumpAllowance{enabled: true}
		for range debugDumpMaxRecords {
			if !allowance.acquire(400) {
				t.Fatal("allowance ended early")
			}
			allowance.finish()
		}
		before := logs.String()
		for range 10 {
			if allowance.acquire(400) {
				t.Fatal("exhausted allowance admitted a response")
			}
		}
		if logs.String() != before || strings.Count(before, "zen_debug_dump_limit_reached") != 1 {
			t.Fatal("exhaustion logged more than once")
		}
	})
}

type debugReadStep struct {
	data []byte
	err  error
}

type debugScriptBody struct {
	steps  []debugReadStep
	closed bool
}

func (b *debugScriptBody) Read(p []byte) (int, error) {
	if len(b.steps) == 0 {
		return 0, io.EOF
	}
	step := &b.steps[0]
	n := copy(p, step.data)
	step.data = step.data[n:]
	if len(step.data) != 0 {
		return n, nil
	}
	err := step.err
	b.steps = b.steps[1:]
	return n, err
}

func (b *debugScriptBody) Close() error {
	b.closed = true
	return nil
}

type debugRoundTripper func(*http.Request) (*http.Response, error)

func (f debugRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestDebugDumpReplay(t *testing.T) {
	logs := captureDebugLogs(t)
	readFailure := errors.New("saved read failure")
	for _, immediate := range []bool{false, true} {
		for _, bufferSize := range []int{1, 2, 3, debugDumpLimit} {
			t.Run(fmt.Sprintf("immediate=%t/buffer=%d", immediate, bufferSize), func(t *testing.T) {
				logs.Reset()
				data := []byte("abc")
				if immediate {
					data = nil
				}
				original := &debugScriptBody{steps: []debugReadStep{{data: data, err: readFailure}, {data: []byte("tail"), err: io.EOF}}}
				resp := &http.Response{Body: original}
				dumpUpstream4xx(resp, nil, nil, nil, false)
				var got []byte
				for {
					buf := make([]byte, bufferSize)
					n, err := resp.Body.Read(buf)
					got = append(got, buf[:n]...)
					if err != nil {
						if err != readFailure || !bytes.Equal(got, data) || !immediate && n == 0 {
							t.Fatalf("boundary = %q, n=%d, err=%v", got, n, err)
						}
						break
					}
				}
				tail, err := io.ReadAll(resp.Body)
				if string(tail) != "tail" || err != nil {
					t.Fatalf("delegated tail = %q, %v", tail, err)
				}
				_ = resp.Body.Close()
				capture := debugRecords(t, logs.String())[0]["error_capture"].(map[string]any)
				if !original.closed || capture["read_error_kind"] != "read_error" || capture["complete"] != false || capture["stop_reason"] != "read_error" {
					t.Fatal("read error or original closer lost")
				}
			})
		}
	}
	t.Run("multiple read boundaries", func(t *testing.T) {
		original := &debugScriptBody{steps: []debugReadStep{
			{data: []byte("ab")},
			{data: []byte("cd"), err: readFailure},
			{data: []byte("tail"), err: io.EOF},
		}}
		resp := &http.Response{Body: original}
		dumpUpstream4xx(resp, nil, nil, nil, false)
		buffer := make([]byte, 100)
		if n, err := resp.Body.Read(buffer); n != 2 || err != nil || string(buffer[:n]) != "ab" {
			t.Fatalf("first boundary = %d, %v", n, err)
		}
		if n, err := resp.Body.Read(buffer); n != 2 || err != readFailure || string(buffer[:n]) != "cd" {
			t.Fatalf("error boundary = %d, %v", n, err)
		}
		if n, err := resp.Body.Read(buffer); n != 4 || err != io.EOF || string(buffer[:n]) != "tail" {
			t.Fatalf("delegated boundary = %d, %v", n, err)
		}
	})
	t.Run("protocol fallback retains attempt metadata", func(t *testing.T) {
		t.Setenv("ZEN_DEBUG_DUMP_RAW", "1")
		for _, enabled := range []bool{false, true} {
			logs.Reset()
			t.Setenv("ZEN_DEBUG_DUMP", "")
			if enabled {
				t.Setenv("ZEN_DEBUG_DUMP", "1")
			}
			var paths, bodies []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, string(body))
				w.WriteHeader(400)
				if len(paths) == 1 {
					_, _ = io.WriteString(w, "ModelProtocolUnsupported")
				} else {
					_, _ = io.WriteString(w, "native rejected")
				}
			}))
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay(server.URL, server.Client(), &idMint{}, relayConfig{}).ServeHTTP(recorder, request)
			server.Close()
			if len(paths) != 2 || paths[0] != "/v1/chat/completions" || paths[1] != "/v1/responses" || recorder.Code != 400 || recorder.Body.String() != "native rejected" {
				t.Fatalf("fallback changed: paths=%v status=%d body=%q", paths, recorder.Code, recorder.Body.String())
			}
			if enabled {
				records := debugRecords(t, logs.String())
				if len(records) != 2 {
					t.Fatalf("fallback records = %d", len(records))
				}
				for i, record := range records {
					if record["upstream_path"] != paths[i] || record["upstream_body"] != bodies[i] || record["attempt"] != float64(i+1) {
						t.Fatal("fallback replaced earlier attempt metadata")
					}
				}
			}
		}
	})
	t.Run("exact limit non-EOF fallback", func(t *testing.T) {
		marker := []byte("ModelProtocolUnsupported")
		body := append(marker, bytes.Repeat([]byte("x"), debugDumpLimit-len(marker))...)
		for _, enabled := range []bool{false, true} {
			logs.Reset()
			t.Setenv("ZEN_DEBUG_DUMP", "")
			if enabled {
				t.Setenv("ZEN_DEBUG_DUMP", "1")
			}
			attempts := 0
			original := &debugScriptBody{steps: []debugReadStep{{data: body, err: readFailure}}}
			client := &http.Client{Transport: debugRoundTripper(func(r *http.Request) (*http.Response, error) {
				attempts++
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: original, Request: r}, nil
			})}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"hello"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay("http://upstream.invalid", client, &idMint{}, relayConfig{}).ServeHTTP(recorder, request)
			if attempts != 1 || recorder.Code != 400 || !bytes.Equal(recorder.Body.Bytes(), body) || !original.closed {
				t.Fatalf("enabled=%t: attempts=%d status=%d bytes=%d closed=%t", enabled, attempts, recorder.Code, recorder.Body.Len(), original.closed)
			}
		}
	})
	t.Run("large Chat and bounded Responses", func(t *testing.T) {
		t.Setenv("ZEN_DEBUG_DUMP", "1")
		body := strings.Repeat("x", 128*1024)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			_, _ = io.WriteString(w, body)
		}))
		defer server.Close()
		for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model","input":"hi"}`))
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay(server.URL, server.Client(), &idMint{}, relayConfig{}).ServeHTTP(recorder, request)
			want := len(body)
			if path == "/v1/responses" {
				want = debugDumpLimit
			}
			if recorder.Code != 400 || recorder.Body.String() != body[:want] {
				t.Fatalf("%s: status=%d bytes=%d", path, recorder.Code, recorder.Body.Len())
			}
		}
	})
	t.Run("direct 429 and relay 400", func(t *testing.T) {
		t.Setenv("ZEN_DEBUG_DUMP_RAW", "1")
		previous := maxAttempts
		maxAttempts = 1
		t.Cleanup(func() { maxAttempts = previous })
		t.Setenv("ZEN_DEBUG_DUMP", "1")
		for _, relayMode := range []bool{false, true} {
			logs.Reset()
			status := 429
			if relayMode {
				status = 400
			}
			var hits atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "captured before close")
			}))
			upstream, relay := server.URL, relayConfig{}
			if relayMode {
				upstream, relay = "https://opencode.ai/inference", parseRelayURL(server.URL)
			}
			recorder := httptest.NewRecorder()
			proxyHandlerWithRelay(upstream, server.Client(), &idMint{}, relay).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("raw")))
			server.Close()
			records := debugRecords(t, logs.String())
			if hits.Load() != 1 || len(records) != 1 || records[0]["error_body"] != "captured before close" {
				t.Fatal("4xx attempt was not captured before handling")
			}
			if relayMode && (recorder.Code != 400 || recorder.Body.String() != "captured before close") || !relayMode && recorder.Code != 502 {
				t.Fatalf("unexpected retry/relay behavior: %d %q", recorder.Code, recorder.Body.String())
			}
		}
	})
	t.Run("stalled HTTP 429", func(t *testing.T) {
		t.Setenv("ZEN_DEBUG_DUMP", "1")
		previousBudget, previousAttempts := retryLoopBudget, maxAttempts
		t.Cleanup(func() { retryLoopBudget, maxAttempts = previousBudget, previousAttempts })
		for _, test := range []struct {
			name     string
			budget   time.Duration
			attempts int
			maximum  time.Duration
		}{
			{name: "capture deadline", budget: 5 * time.Second, attempts: 1, maximum: 3 * time.Second},
			{name: "loop deadline", budget: 150 * time.Millisecond, attempts: 3, maximum: time.Second},
		} {
			t.Run(test.name, func(t *testing.T) {
				logs.Reset()
				retryLoopBudget, maxAttempts = test.budget, test.attempts
				var hits atomic.Int32
				closed := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.WriteHeader(429)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(closed)
				}))
				defer server.Close()
				started := time.Now()
				recorder := httptest.NewRecorder()
				proxyHandlerWithRelay(server.URL, server.Client(), &idMint{}, relayConfig{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("raw")))
				if elapsed := time.Since(started); elapsed > test.maximum {
					t.Fatalf("capture exceeded deadline: %s", elapsed)
				}
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("stalled HTTP body was not canceled and closed")
				}
				records := debugRecords(t, logs.String())
				var capture map[string]any
				if len(records) == 1 {
					capture = records[0]["error_capture"].(map[string]any)
				}
				if hits.Load() != 1 || recorder.Code != 502 || len(records) != 1 || capture["complete"] != false || capture["stop_reason"] != "timeout" || capture["read_error_kind"] != "deadline_exceeded" {
					t.Fatalf("stalled capture: hits=%d status=%d records=%v", hits.Load(), recorder.Code, records)
				}
			})
		}
	})
}

func TestDebugDumpFullHistory(t *testing.T) {
	logs := captureDebugLogs(t)
	const secret = "private-body-sentinel"
	prompt := strings.Repeat("s", 300*1024) + secret
	for _, field := range []string{"messages", "input"} {
		t.Run(field, func(t *testing.T) {
			logs.Reset()
			items := []any{
				map[string]any{"role": "system", "content": prompt},
				map[string]any{
					"role": "assistant", "content": "世界\n", "reasoning_content": "canonical-" + secret,
					"reasoning": "alternate-" + secret, "reasoning_details": []any{map[string]any{"text": secret}},
					"VendorTHINKING": nil,
					"tool_calls": []any{map[string]any{
						"id": "tool-id", "type": "function",
						"function": map[string]any{"name": "lookup", "arguments": "arguments-" + secret},
					}, nil},
				},
				map[string]any{"role": "tool", "tool_call_id": "tool-id", "content": "output-" + secret},
				map[string]any{
					"role": "assistant", "reasoning_content": "", "reasoning": nil, "reasoning_details": false,
					"content": []any{
						map[string]any{"type": "thinking", "thinking": "think-" + secret},
						map[string]any{"type": "reasoning_text", "text": "reason-" + secret},
						map[string]any{"type": "text", "text": "text-" + secret}, nil, 42,
					},
				},
				nil, "malformed-" + secret, 7, false, []any{},
				map[string]any{
					"type": "reasoning", "id": "reasoning-id",
					"content": []any{map[string]any{"type": "reasoning_text", "text": secret}},
					"summary": []any{map[string]any{"type": "summary_text", "text": secret}, nil},
				},
				map[string]any{"type": "function_call", "id": "item-id", "call_id": "call-id", "name": "lookup", "arguments": secret},
				map[string]any{"type": "function_call_output", "call_id": "call-id", "output": secret},
			}
			body, err := json.Marshal(map[string]any{
				field: items, "model": "model-id", "stream": true, "max_tokens": 10,
				"max_completion_tokens": nil, "max_output_tokens": 20, "instructions": secret,
				"tools":            []any{map[string]any{"private": secret}},
				"stream_options":   map[string]any{"include_usage": true, "private": secret},
				"tool_choice":      map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "private": secret}},
				"thinking":         map[string]any{"type": "enabled", "budget_tokens": 1024, "private": secret},
				"reasoning":        map[string]any{"effort": "high", "summary": "auto", "private": secret},
				"reasoning_effort": "medium",
			})
			if err != nil {
				t.Fatal(err)
			}
			resp := &http.Response{Body: io.NopCloser(strings.NewReader("error-" + secret))}
			dumpUpstream4xx(resp, body, body, nil, false)
			records := debugRecords(t, logs.String())
			if len(records) != 1 || strings.Contains(logs.String(), secret) {
				t.Fatal("full history summary missing or disclosed private text")
			}
			for _, name := range []string{"client_summary", "upstream_summary"} {
				summary := records[0][name].(map[string]any)
				if summary["body_bytes"] != float64(len(body)) || summary["parse_status"] != "ok" {
					t.Fatal("summary did not parse full request")
				}
				entries := summary[field].(map[string]any)["entries"].([]any)
				if len(entries) != len(items) {
					t.Fatalf("history length = %d", len(entries))
				}
				for index, entry := range entries {
					if entry.(map[string]any)["index"] != float64(index) {
						t.Fatalf("lost original index %d", index)
					}
				}
				first := entries[0].(map[string]any)
				if first["content"].(map[string]any)["text_bytes"] != float64(len(prompt)) {
					t.Fatal("large prompt length lost")
				}
				assistant := entries[1].(map[string]any)
				content := assistant["content"].(map[string]any)
				if content["text_bytes"] != float64(len("世界\n")) || content["json_bytes"] != float64(len(`"世界\n"`)) {
					t.Fatal("decoded UTF-8 and escaped token lengths differ")
				}
				for _, key := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
					if assistant[key].(map[string]any)["present"] != true {
						t.Fatalf("missing reasoning descriptor %s", key)
					}
				}
				if assistant["reasoning_fields"].(map[string]any)["VendorTHINKING"].(map[string]any)["type"] != "null" {
					t.Fatal("case-insensitive reasoning field omitted")
				}
				calls := assistant["tool_calls"].(map[string]any)
				call := calls["entries"].([]any)[0].(map[string]any)
				if calls["count"] != float64(2) || call["index"] != float64(0) || call["id"].(map[string]any)["value"] != "tool-id" || call["function_name"].(map[string]any)["value"] != "lookup" || call["arguments"].(map[string]any)["text_bytes"] != float64(len("arguments-"+secret)) {
					t.Fatal("ordered tool call metadata lost")
				}
				if entries[2].(map[string]any)["tool_call_id"].(map[string]any)["value"] != "tool-id" {
					t.Fatal("tool result association lost")
				}
				last := entries[3].(map[string]any)
				if last["reasoning_content"].(map[string]any)["text_bytes"] != float64(0) || last["reasoning"].(map[string]any)["type"] != "null" || last["reasoning_details"].(map[string]any)["type"] != "boolean" {
					t.Fatal("empty, null and wrong-type reasoning collapsed")
				}
				parts := last["content"].(map[string]any)["parts"].([]any)
				if len(parts) != 5 || parts[0].(map[string]any)["thinking"].(map[string]any)["text_bytes"] != float64(len("think-"+secret)) || parts[1].(map[string]any)["text"].(map[string]any)["text_bytes"] != float64(len("reason-"+secret)) {
					t.Fatal("content parts incomplete")
				}
				for index, want := range []string{"null", "string", "number", "boolean", "array"} {
					if entries[index+4].(map[string]any)["shape"].(map[string]any)["type"] != want {
						t.Fatalf("malformed entry %d lost shape", index+4)
					}
				}
				reasoning := entries[9].(map[string]any)
				if len(reasoning["summary"].(map[string]any)["parts"].([]any)) != 2 {
					t.Fatal("Responses summary parts omitted")
				}
				function := entries[10].(map[string]any)
				for key, want := range map[string]string{"id": "item-id", "call_id": "call-id", "name": "lookup"} {
					if function[key].(map[string]any)["value"] != want {
						t.Fatalf("function %s lost", key)
					}
				}
				if function["arguments"].(map[string]any)["text_bytes"] != float64(len(secret)) || entries[11].(map[string]any)["output"].(map[string]any)["text_bytes"] != float64(len(secret)) {
					t.Fatal("Responses argument/output lengths lost")
				}
				params := summary["params"].(map[string]any)
				if params["model"].(map[string]any)["value"] != "model-id" || params["tools"].(map[string]any)["count"] != float64(1) || params["instructions"].(map[string]any)["text_bytes"] != float64(len(secret)) {
					t.Fatal("parameter summary incomplete")
				}
				if params["stream_options"].(map[string]any)["include_usage"].(map[string]any)["value"] != true || params["thinking"].(map[string]any)["controls"].(map[string]any)["budget_tokens"].(map[string]any)["value"] != float64(1024) || params["reasoning"].(map[string]any)["controls"].(map[string]any)["effort"].(map[string]any)["value"] != "high" {
					t.Fatal("allowlisted nested controls omitted")
				}
			}
		})
	}
}

func TestDebugDumpDescriptors(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      string
		kind      string
		jsonBytes any
		textBytes any
		items     any
	}{
		{name: "missing", kind: "missing"},
		{name: "null", data: " null \n", kind: "null", jsonBytes: 4},
		{name: "boolean", data: "true", kind: "boolean", jsonBytes: 4},
		{name: "number", data: "1.20e+3", kind: "number", jsonBytes: 7},
		{name: "empty string", data: `""`, kind: "string", jsonBytes: 2, textBytes: 0},
		{name: "unicode escape", data: `"\u4e16\u754c\n"`, kind: "string", jsonBytes: 16, textBytes: 7},
		{name: "array", data: `[null,{}]`, kind: "array", jsonBytes: 9, items: 2},
		{name: "object", data: `{"z":null,"a":1}`, kind: "object", jsonBytes: 16},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := debugDescriptor(json.RawMessage(test.data))
			if got["type"] != test.kind || got["present"] != (test.data != "") || got["json_bytes"] != test.jsonBytes || got["text_bytes"] != test.textBytes || got["items"] != test.items {
				t.Fatalf("descriptor = %v", got)
			}
			if test.kind == "object" && fmt.Sprint(got["keys"]) != "[a z]" {
				t.Fatal("object keys not sorted")
			}
		})
	}
	for _, test := range []struct {
		name   string
		data   string
		status string
	}{
		{name: "empty", status: "invalid_json"},
		{name: "syntax", data: `{"private":"syntax-secret",`, status: "invalid_json"},
		{name: "multiple roots", data: `{} {}`, status: "invalid_json"},
		{name: "null root", data: `null`, status: "non_object"},
		{name: "array root", data: `[]`, status: "non_object"},
		{name: "string root", data: `"syntax-secret"`, status: "non_object"},
		{name: "object", data: `{"z":null,"input":"syntax-secret","a":true}`, status: "ok"},
	} {
		t.Run(test.name, func(t *testing.T) {
			summary := summarizeDebugRequest([]byte(test.data))
			encoded, err := json.Marshal(summary)
			if err != nil || summary["parse_status"] != test.status || bytes.Contains(encoded, []byte("syntax-secret")) {
				t.Fatalf("unsafe or incorrect parse summary: %s, %v", encoded, err)
			}
			if test.status == "invalid_json" && (summary["parse_error_kind"] != "syntax_error" || summary["parse_error_offset"] == nil) {
				t.Fatal("fixed parse error classification missing")
			}
			if test.name == "object" && (fmt.Sprint(summary["top_level_keys"]) != "[a input z]" || summary["input"].(map[string]any)["type"] != "string") {
				t.Fatal("top-level keys or string input descriptor lost")
			}
		})
	}
}

func TestDebugDumpRawRequestBounds(t *testing.T) {
	logs := captureDebugLogs(t)
	for _, size := range []int{debugDumpRawLimit - 1, debugDumpRawLimit, debugDumpRawLimit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			logs.Reset()
			body := bytes.Repeat([]byte("x"), size)
			resp := &http.Response{Body: io.NopCloser(strings.NewReader("error"))}
			dumpUpstream4xx(resp, body, body, nil, true)
			record := debugRecords(t, logs.String())[0]
			for _, key := range []string{"client_body", "upstream_body"} {
				if len(record[key].(string)) != min(size, debugDumpRawLimit) || record[key+"_bytes"] != float64(size) || record[key+"_truncated"] != (size > debugDumpRawLimit) {
					t.Fatalf("incorrect %s raw request bound", key)
				}
			}
		})
	}
}

func TestDebugDumpErrorCapture(t *testing.T) {
	logs := captureDebugLogs(t)
	for _, test := range []struct {
		name     string
		steps    []debugReadStep
		reason   string
		kind     string
		complete bool
		observed int
	}{
		{name: "EOF", steps: []debugReadStep{{data: []byte("abc"), err: io.EOF}}, reason: "eof", complete: true, observed: 3},
		{name: "short non EOF", steps: []debugReadStep{{data: []byte("abc"), err: errors.New("read-secret")}}, reason: "read_error", kind: "read_error", observed: 3},
		{name: "limit", steps: []debugReadStep{{data: bytes.Repeat([]byte("x"), debugDumpLimit+2)}}, reason: "limit", observed: debugDumpLimit + 1},
		{name: "no progress", steps: make([]debugReadStep, 100), reason: "no_progress", kind: "no_progress"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs.Reset()
			resp := &http.Response{Body: &debugScriptBody{steps: test.steps}}
			dumpUpstream4xx(resp, nil, nil, nil, false)
			record := debugRecords(t, logs.String())[0]
			capture := record["error_capture"].(map[string]any)
			if capture["stop_reason"] != test.reason || capture["read_error_kind"] != test.kind || capture["complete"] != test.complete || capture["observed_bytes"] != float64(test.observed) || capture["captured_bytes"] != float64(min(test.observed, debugDumpLimit)) || strings.Contains(logs.String(), "read-secret") {
				t.Fatalf("error capture = %v", capture)
			}
		})
	}
	for _, text := range []string{
		"The reasoning_content in the thinking mode must be passed back to the API.",
		"The `reasoning_content` in the thinking mode must be passed back to the API.",
	} {
		logs.Reset()
		resp := &http.Response{Body: io.NopCloser(strings.NewReader(text))}
		dumpUpstream4xx(resp, nil, nil, nil, false)
		if debugRecords(t, logs.String())[0]["error_capture"].(map[string]any)["reasoning_required"] != true {
			t.Fatal("reasoning requirement not classified")
		}
	}
	logs.Reset()
	resp := &http.Response{Body: io.NopCloser(strings.NewReader("encode-secret"))}
	dumpUpstream4xx(resp, nil, nil, map[string]any{"model": make(chan int)}, false)
	if len(debugRecords(t, logs.String())) != 0 || !strings.Contains(logs.String(), "zen_debug_dump_encode_error") || strings.Contains(logs.String(), "encode-secret") {
		t.Fatal("encoding failure must emit only fixed marker")
	}
	if replay, err := io.ReadAll(resp.Body); err != nil || string(replay) != "encode-secret" {
		t.Fatal("encoding failure changed forwarding")
	}
}

func TestDebugDumpHistoryShapes(t *testing.T) {
	for _, test := range []struct {
		name         string
		data         string
		messagesType string
		inputType    string
	}{
		{name: "missing", data: `{}`, messagesType: "missing", inputType: "missing"},
		{name: "null", data: `{"messages":null,"input":null}`, messagesType: "null", inputType: "null"},
		{name: "wrong types", data: `{"messages":{},"input":true}`, messagesType: "object", inputType: "boolean"},
		{name: "empty", data: `{"messages":[],"input":[]}`, messagesType: "array", inputType: "array"},
		{name: "duplicate last wins", data: `{"messages":null,"messages":[],"input":42}`, messagesType: "array", inputType: "number"},
	} {
		t.Run(test.name, func(t *testing.T) {
			summary := summarizeDebugRequest([]byte(test.data))
			if summary["messages"].(map[string]any)["type"] != test.messagesType || summary["input"].(map[string]any)["type"] != test.inputType {
				t.Fatalf("history shape = %v", summary)
			}
		})
	}
	summary := summarizeDebugRequest([]byte(`{"messages":[{}, {"content":null}, {"content":""}, {"content":false}]}`))
	entries := summary["messages"].(map[string]any)["entries"].([]any)
	for index, want := range []string{"missing", "null", "string", "boolean"} {
		entry := entries[index].(map[string]any)
		if entry["content"].(map[string]any)["type"] != want || entry["reasoning_content"].(map[string]any)["type"] != "missing" {
			t.Fatalf("entry %d descriptors collapsed", index)
		}
	}
}
