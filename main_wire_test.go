package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSuccessfulUpstreamBytesUnchanged(t *testing.T) {
	const chatCanonical = `{
  "unknown": {"text":"雪\n<&>","fraction":1.2500e+01,"integer":9007199254740993},
  "stream": true, "model":"deepseek-v4.1-flash-free",
  "messages":[{"role":"user","content":"查詢 雪"},{"role":"assistant","content":"","reasoning_content":"查核\n雪","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"12.5"},{"role":"user","content":"回答"}],
  "tools":[{"type":"function","function":{"name":"lookup","description":"查詢 <雪>","parameters":{"type":"object","properties":{"n":{"type":"number","default":1.2500e+01}}}},"caller_extension":"保留"}],
  "temperature":1.2500e-01, "max_tokens":1e+02
}`
	const chatMissing = `{
  "unknown": {"text":"雪\n<&>","fraction":1.2500e+01,"integer":9007199254740993},
  "stream": true, "model":"deepseek-v4.1-flash-free",
  "messages":[{"role":"user","content":"查詢 雪"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"12.5"},{"role":"user","content":"回答"}],
  "tools":[{"type":"function","function":{"name":"lookup","description":"查詢 <雪>","parameters":{"type":"object","properties":{"n":{"type":"number","default":1.2500e+01}}}},"caller_extension":"保留"}],
  "temperature":1.2500e-01, "max_tokens":1e+02
}`
	const chatCanonicalGolden = `{"max_tokens":100,"messages":[{"content":"查詢 雪","role":"user"},{"content":"","reasoning_content":"查核\n雪","role":"assistant","tool_calls":[{"function":{"arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}","name":"lookup"},"id":"call_1","type":"function"}]},{"content":"12.5","role":"tool","tool_call_id":"call_1"},{"content":"回答","role":"user"}],"model":"deepseek-v4.1-flash-free","stream":true,"temperature":0.125,"tools":[{"caller_extension":"保留","function":{"description":"查詢 \u003c雪\u003e","name":"lookup","parameters":{"properties":{"n":{"default":12.5,"type":"number"}},"type":"object"}},"type":"function"}],"unknown":{"fraction":12.5,"integer":9007199254740992,"text":"雪\n\u003c\u0026\u003e"}}`
	const chatMissingGolden = `{"max_tokens":100,"messages":[{"content":"查詢 雪","role":"user"},{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}","name":"lookup"},"id":"call_1","type":"function"}]},{"content":"12.5","role":"tool","tool_call_id":"call_1"},{"content":"回答","role":"user"}],"model":"deepseek-v4.1-flash-free","stream":true,"temperature":0.125,"tools":[{"caller_extension":"保留","function":{"description":"查詢 \u003c雪\u003e","name":"lookup","parameters":{"properties":{"n":{"default":12.5,"type":"number"}},"type":"object"}},"type":"function"}],"unknown":{"fraction":12.5,"integer":9007199254740992,"text":"雪\n\u003c\u0026\u003e"}}`
	const responsesCanonical = `{
  "unknown": {"text":"雪\n<&>","integer":9007199254740993},
  "stream": true, "model":"deepseek-v4.1-flash-free", "instructions":"保留 <雪>",
  "input":[{"type":"message","role":"user","content":"查詢 雪"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"查核\n雪"}]},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}"},{"type":"function_call_output","call_id":"call_1","output":"12.5"},{"type":"message","role":"user","content":"回答"}],
  "tools":[{"type":"function","name":"lookup","description":"查詢 <雪>","parameters":{"type":"object","properties":{"n":{"type":"number","default":1.2500e+01}}},"caller_extension":"保留"}],
  "tool_choice":{"type":"function","name":"lookup"}, "temperature":1.2500e-01, "max_output_tokens":1e+02
}`
	const responsesMissing = `{
  "unknown": {"text":"雪\n<&>","integer":9007199254740993},
  "stream": true, "model":"deepseek-v4.1-flash-free", "instructions":"保留 <雪>",
  "input":[{"type":"message","role":"user","content":"查詢 雪"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}"},{"type":"function_call_output","call_id":"call_1","output":"12.5"},{"type":"message","role":"user","content":"回答"}],
  "tools":[{"type":"function","name":"lookup","description":"查詢 <雪>","parameters":{"type":"object","properties":{"n":{"type":"number","default":1.2500e+01}}},"caller_extension":"保留"}],
  "tool_choice":{"type":"function","name":"lookup"}, "temperature":1.2500e-01, "max_output_tokens":1e+02
}`
	const responsesCanonicalGolden = `{"max_tokens":100,"messages":[{"content":"保留 \u003c雪\u003e","role":"system"},{"content":"查詢 雪","role":"user"},{"content":"","reasoning_content":"查核\n雪","role":"assistant","tool_calls":[{"function":{"arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}","name":"lookup"},"id":"call_1","type":"function"}]},{"content":"12.5","role":"tool","tool_call_id":"call_1"},{"content":"回答","role":"user"}],"model":"deepseek-v4.1-flash-free","stream":true,"temperature":0.125,"tool_choice":{"function":{"name":"lookup"},"type":"function"},"tools":[{"function":{"description":"查詢 \u003c雪\u003e","name":"lookup","parameters":{"properties":{"n":{"default":12.5,"type":"number"}},"type":"object"}},"type":"function"}]}`
	const responsesMissingGolden = `{"max_tokens":100,"messages":[{"content":"保留 \u003c雪\u003e","role":"system"},{"content":"查詢 雪","role":"user"},{"content":"","role":"assistant","tool_calls":[{"function":{"arguments":"{\"n\":1.2500e+01,\"q\":\"雪\"}","name":"lookup"},"id":"call_1","type":"function"}]},{"content":"12.5","role":"tool","tool_call_id":"call_1"},{"content":"回答","role":"user"}],"model":"deepseek-v4.1-flash-free","stream":true,"temperature":0.125,"tool_choice":{"function":{"name":"lookup"},"type":"function"},"tools":[{"function":{"description":"查詢 \u003c雪\u003e","name":"lookup","parameters":{"properties":{"n":{"default":12.5,"type":"number"}},"type":"object"}},"type":"function"}]}`
	const nativeCanonical = `{
  "unknown": {"text":"雪\n<&>","fraction":1.2500e+01,"integer":9007199254740993},
  "stream": true, "model":"muse-spark-1.3",
  "input":[{"type":"message","role":"assistant","content":"先前回答","reasoning_content":"查核\n雪"},{"type":"message","role":"user","content":"回答"}],
  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"caller_extension":"保留"}], "temperature":1.2500e-01
}`
	const nativeMissing = `{
  "unknown": {"text":"雪\n<&>","fraction":1.2500e+01,"integer":9007199254740993},
  "stream": true, "model":"muse-spark-1.3",
  "input":[{"type":"message","role":"assistant","content":"先前回答"},{"type":"message","role":"user","content":"回答"}],
  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object"},"caller_extension":"保留"}], "temperature":1.2500e-01
}`
	const nativeCanonicalNonstreamGolden = `{"input":[{"content":"先前回答","reasoning_content":"查核\n雪","role":"assistant","type":"message"},{"content":"回答","role":"user","type":"message"}],"model":"muse-spark-1.3","stream":true,"temperature":0.125,"tools":[{"caller_extension":"保留","name":"lookup","parameters":{"type":"object"},"type":"function"}],"unknown":{"fraction":12.5,"integer":9007199254740992,"text":"雪\n\u003c\u0026\u003e"}}`
	const nativeMissingNonstreamGolden = `{"input":[{"content":"先前回答","role":"assistant","type":"message"},{"content":"回答","role":"user","type":"message"}],"model":"muse-spark-1.3","stream":true,"temperature":0.125,"tools":[{"caller_extension":"保留","name":"lookup","parameters":{"type":"object"},"type":"function"}],"unknown":{"fraction":12.5,"integer":9007199254740992,"text":"雪\n\u003c\u0026\u003e"}}`
	const chatSSE = "data: {\"id\":\"chatcmpl_wire\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_wire\",\"object\":\"chat.completion.chunk\",\"model\":\"deepseek-v4.1-flash-free\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

	fixtures := []struct {
		name          string
		path          string
		inbound       string
		streamWant    string
		nonstreamWant string
		native        bool
	}{
		{"chat canonical", "/v1/chat/completions", chatCanonical, chatCanonicalGolden, chatCanonicalGolden, false},
		{"chat missing", "/v1/chat/completions", chatMissing, chatMissingGolden, chatMissingGolden, false},
		{"responses canonical", "/v1/responses", responsesCanonical, responsesCanonicalGolden, responsesCanonicalGolden, false},
		{"responses missing", "/v1/responses", responsesMissing, responsesMissingGolden, responsesMissingGolden, false},
		{"native responses canonical", "/v1/responses", nativeCanonical, nativeCanonical, nativeCanonicalNonstreamGolden, true},
		{"native responses missing", "/v1/responses", nativeMissing, nativeMissing, nativeMissingNonstreamGolden, true},
	}
	for _, fixture := range fixtures {
		for _, mode := range []struct {
			name   string
			stream bool
		}{
			{"stream", true},
			{"nonstream", false},
		} {
			for _, gate := range []struct {
				name  string
				value string
			}{
				{"debug off", ""},
				{"debug on", "1"},
			} {
				t.Run(fixture.name+"/"+mode.name+"/"+gate.name, func(t *testing.T) {
					t.Setenv("ZEN_DEBUG_DUMP", gate.value)
					inbound, want := fixture.inbound, fixture.streamWant
					if !mode.stream {
						inbound = strings.Replace(inbound, `"stream": true`, `"stream": false`, 1)
						want = fixture.nonstreamWant
					}
					type capturedRequest struct {
						path string
						body []byte
						err  error
					}
					captured := make(chan capturedRequest, 4)
					responseSSE := chatSSE
					if fixture.native {
						responseSSE = sampleResponsesSSE(t)
					}
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						captured <- capturedRequest{r.URL.Path, body, err}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, responseSSE)
					}))
					defer upstream.Close()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, fixture.path, strings.NewReader(inbound)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					if mode.stream {
						request.Header.Set("Accept", "text/event-stream")
					}
					recorder := httptest.NewRecorder()
					proxyHandlerWithRelay(upstream.URL, upstream.Client(), &idMint{}, relayConfig{}).ServeHTTP(recorder, request)
					if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "OK") {
						t.Fatalf("client response = %d %q, want successful completion", recorder.Code, recorder.Body.String())
					}
					select {
					case got := <-captured:
						if got.err != nil {
							t.Fatalf("read upstream request: %v", got.err)
						}
						wantPath := "/v1/chat/completions"
						if fixture.native {
							wantPath = "/v1/responses"
						}
						if got.path != wantPath {
							t.Errorf("upstream path = %q, want %q", got.path, wantPath)
						}
						if !bytes.Equal(got.body, []byte(want)) {
							t.Errorf("upstream bytes changed\ngot:  %q\nwant: %q", got.body, want)
						}
					default:
						t.Fatal("no upstream request captured")
					}
					if extra := len(captured); extra != 0 {
						t.Errorf("unexpected additional upstream attempts: %d", extra)
					}
				})
			}
		}
	}
}
