package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const debugDumpLimit = 64 * 1024
const debugDumpRawLimit = 1 << 20
const debugDumpMaxRecords = 16

type debugDumpAllowance struct {
	enabled   bool
	mu        sync.Mutex
	records   int
	active    int
	announced bool
}

func (a *debugDumpAllowance) acquire(status int) bool {
	if !a.enabled || status < 400 || status >= 500 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.records == debugDumpMaxRecords {
		return false
	}
	a.records++
	a.active++
	return true
}

func (a *debugDumpAllowance) finish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.active--
	if a.records == debugDumpMaxRecords && a.active == 0 && !a.announced {
		a.announced = true
		log.Printf("zen_debug_dump_limit_reached")
	}
}

type debugReadBoundary struct {
	end int
	err error
}

type debugReplayBody struct {
	io.ReadCloser
	prefix     []byte
	boundaries []debugReadBoundary
	offset     int
	boundary   int
}

func (b *debugReplayBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.boundary == len(b.boundaries) {
		return b.ReadCloser.Read(p)
	}
	next := b.boundaries[b.boundary]
	n := copy(p, b.prefix[b.offset:next.end])
	b.offset += n
	if b.offset == next.end {
		b.boundary++
		return n, next.err
	}
	return n, nil
}

type debugCaptureBody struct {
	io.ReadCloser
	stop func() error
}

func captureUpstream4xx(
	ctx context.Context,
	cancelAttempt context.CancelFunc,
	resp *http.Response,
	inbound, outbound []byte,
	metadata map[string]any,
	raw bool,
) {
	captureCtx, cancelCapture := context.WithTimeout(ctx, 2*time.Second)
	defer cancelCapture()
	original := resp.Body
	finished := make(chan struct{})
	stop := context.AfterFunc(captureCtx, func() {
		cancelAttempt()
		_ = original.Close()
		close(finished)
	})
	resp.Body = &debugCaptureBody{ReadCloser: original, stop: func() error {
		stopped := stop()
		if !stopped {
			<-finished
		}
		err := captureCtx.Err()
		if stopped && err != nil {
			cancelAttempt()
			_ = original.Close()
		}
		return err
	}}
	dumpUpstream4xx(resp, inbound, outbound, metadata, raw)
}

func dumpUpstream4xx(resp *http.Response, inbound, outbound []byte, metadata map[string]any, raw bool) {
	original := resp.Body
	prefix := make([]byte, debugDumpLimit+1)
	boundaries := make([]debugReadBoundary, 0, 8)
	observed, emptyReads := 0, 0
	stopReason, readErrorKind := "limit", ""
	complete := false
	for observed < len(prefix) {
		n, err := original.Read(prefix[observed:])
		observed += n
		boundaries = append(boundaries, debugReadBoundary{end: observed, err: err})
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads == 100 {
				stopReason, readErrorKind = "no_progress", "no_progress"
				break
			}
		} else {
			emptyReads = 0
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				stopReason, complete = "eof", true
			} else {
				stopReason, readErrorKind = "read_error", debugReadErrorKind(err)
			}
			break
		}
	}
	if body, ok := original.(*debugCaptureBody); ok {
		if err := body.stop(); err != nil {
			stopReason, readErrorKind, complete = "timeout", debugReadErrorKind(err), false
		}
		original = body.ReadCloser
	}
	resp.Body = &debugReplayBody{ReadCloser: original, prefix: prefix[:observed], boundaries: boundaries}
	captured := prefix[:min(observed, debugDumpLimit)]
	record := make(map[string]any, 24)
	for _, key := range []string{"request_id", "session", "attempt", "client_path", "upstream_path", "model", "relay", "status"} {
		record[key] = metadata[key]
	}
	record["schema_version"] = 2
	record["raw_enabled"] = raw
	record["client_summary"] = summarizeDebugRequest(inbound)
	record["upstream_summary"] = summarizeDebugRequest(outbound)
	record["error_capture"] = map[string]any{
		"observed_bytes":     observed,
		"captured_bytes":     len(captured),
		"complete":           complete,
		"stop_reason":        stopReason,
		"read_error_kind":    readErrorKind,
		"reasoning_required": debugReasoningRequired(captured),
	}
	if raw {
		for _, body := range []struct {
			name  string
			data  []byte
			limit int
		}{
			{name: "client_body", data: inbound, limit: debugDumpRawLimit},
			{name: "upstream_body", data: outbound, limit: debugDumpRawLimit},
			{name: "error_body", data: prefix[:observed], limit: debugDumpLimit},
		} {
			record[body.name] = string(body.data[:min(len(body.data), body.limit)])
			record[body.name+"_bytes"] = len(body.data)
			record[body.name+"_truncated"] = len(body.data) > body.limit || body.name == "error_body" && !complete
		}
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		log.Printf("zen_debug_dump_encode_error")
		return
	}
	log.Printf("zen_debug_dump %s", encoded)
}

func debugReadErrorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, io.ErrNoProgress):
		return "no_progress"
	default:
		return "read_error"
	}
}

func debugReasoningRequired(data []byte) bool {
	plain := []byte("The reasoning_content in the thinking mode must be passed back to the API.")
	quoted := []byte("The `reasoning_content` in the thinking mode must be passed back to the API.")
	return bytes.Contains(data, plain) || bytes.Contains(data, quoted)
}

func debugObject(data json.RawMessage) map[string]json.RawMessage {
	var object map[string]json.RawMessage
	token := bytes.TrimSpace(data)
	if len(token) != 0 && token[0] == '{' {
		_ = json.Unmarshal(token, &object)
	}
	return object
}

func debugKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func debugDescriptor(data json.RawMessage) map[string]any {
	token := bytes.TrimSpace(data)
	result := map[string]any{
		"present": len(data) != 0, "type": "missing", "json_bytes": nil,
		"text_bytes": nil, "items": nil, "keys": nil,
	}
	if len(token) == 0 {
		return result
	}
	result["json_bytes"] = len(token)
	switch token[0] {
	case 'n':
		result["type"] = "null"
	case 't', 'f':
		result["type"] = "boolean"
	case '"':
		var text string
		_ = json.Unmarshal(token, &text)
		result["type"], result["text_bytes"] = "string", len(text)
	case '[':
		var items []json.RawMessage
		_ = json.Unmarshal(token, &items)
		result["type"], result["items"] = "array", len(items)
	case '{':
		result["type"], result["keys"] = "object", debugKeys(debugObject(token))
	default:
		result["type"] = "number"
	}
	return result
}

func debugMetadata(data json.RawMessage) map[string]any {
	result := debugDescriptor(data)
	switch result["type"] {
	case "string", "boolean", "number":
		var value any
		_ = json.Unmarshal(data, &value)
		result["value"] = value
	}
	return result
}

func summarizeDebugRequest(data []byte) map[string]any {
	result := map[string]any{
		"body_bytes": len(data), "parse_status": "ok", "top_level_keys": []string{},
		"params": map[string]any{}, "messages": debugDescriptor(nil), "input": debugDescriptor(nil),
	}
	var root json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		var syntax *json.SyntaxError
		var offset int64
		if errors.As(err, &syntax) {
			offset = syntax.Offset
		}
		result["parse_status"], result["parse_error_kind"], result["parse_error_offset"] = "invalid_json", "syntax_error", offset
		return result
	}
	result["shape"] = debugDescriptor(root)
	object := debugObject(root)
	if object == nil {
		result["parse_status"] = "non_object"
		return result
	}
	result["top_level_keys"] = debugKeys(object)
	params := make(map[string]any, 13)
	for _, key := range []string{
		"model", "max_tokens", "max_completion_tokens", "max_output_tokens", "stream",
		"stream_options", "tools", "tool_choice", "thinking", "reasoning_effort", "reasoning", "instructions",
	} {
		value := object[key]
		descriptor := debugDescriptor(value)
		switch key {
		case "model", "max_tokens", "max_completion_tokens", "max_output_tokens", "stream", "reasoning_effort":
			descriptor = debugMetadata(value)
		case "tools":
			descriptor["count"] = descriptor["items"]
		case "stream_options":
			descriptor["include_usage"] = debugMetadata(debugObject(value)["include_usage"])
		case "tool_choice":
			descriptor = debugMetadata(value)
			choice := debugObject(value)
			fields := map[string]any{}
			for _, name := range []string{"type", "name"} {
				fields[name] = debugMetadata(choice[name])
			}
			fields["function_name"] = debugMetadata(debugObject(choice["function"])["name"])
			descriptor["controls"] = fields
		case "thinking", "reasoning":
			controls := debugObject(value)
			fields := map[string]any{}
			for _, name := range []string{"type", "budget_tokens", "effort", "summary"} {
				fields[name] = debugMetadata(controls[name])
			}
			descriptor["controls"] = fields
		}
		params[key] = descriptor
	}
	result["params"] = params
	for _, name := range []string{"messages", "input"} {
		value := object[name]
		descriptor := debugDescriptor(value)
		if descriptor["type"] == "array" {
			var items []json.RawMessage
			_ = json.Unmarshal(value, &items)
			entries := make([]any, 0, len(items))
			for index, item := range items {
				entries = append(entries, debugHistoryItem(item, index))
			}
			descriptor["entries"] = entries
		}
		result[name] = descriptor
	}
	return result
}

func debugHistoryItem(data json.RawMessage, index int) map[string]any {
	object := debugObject(data)
	item := map[string]any{"index": index, "shape": debugDescriptor(data), "keys": debugKeys(object)}
	for _, name := range []string{"role", "type", "id", "call_id", "name", "tool_call_id"} {
		item[name] = debugMetadata(object[name])
	}
	for _, name := range []string{"content", "reasoning_content", "reasoning", "reasoning_details", "arguments", "output"} {
		item[name] = debugDescriptor(object[name])
	}
	alternates := make(map[string]any)
	for key, value := range object {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "thinking") || strings.Contains(lower, "reasoning") {
			alternates[key] = debugDescriptor(value)
		}
	}
	item["reasoning_fields"] = alternates
	for _, name := range []string{"content", "summary"} {
		value := object[name]
		descriptor := debugDescriptor(value)
		if descriptor["type"] == "array" {
			var parts []json.RawMessage
			_ = json.Unmarshal(value, &parts)
			entries := make([]any, 0, len(parts))
			for partIndex, part := range parts {
				fields := debugObject(part)
				entry := map[string]any{
					"index": partIndex, "shape": debugDescriptor(part), "keys": debugKeys(fields),
					"type": debugMetadata(fields["type"]),
				}
				for _, field := range []string{"text", "thinking", "reasoning"} {
					entry[field] = debugDescriptor(fields[field])
				}
				entries = append(entries, entry)
			}
			descriptor["parts"] = entries
		}
		item[name] = descriptor
	}
	calls := debugDescriptor(object["tool_calls"])
	entries := []any{}
	if calls["type"] == "array" {
		var values []json.RawMessage
		_ = json.Unmarshal(object["tool_calls"], &values)
		for callIndex, call := range values {
			fields := debugObject(call)
			entries = append(entries, map[string]any{
				"index": callIndex, "shape": debugDescriptor(call), "keys": debugKeys(fields),
				"id": debugMetadata(fields["id"]), "type": debugMetadata(fields["type"]),
				"function_name": debugMetadata(debugObject(fields["function"])["name"]),
				"arguments":     debugDescriptor(debugObject(fields["function"])["arguments"]),
			})
		}
	}
	calls["count"], calls["entries"] = calls["items"], entries
	item["tool_calls"] = calls
	return item
}
