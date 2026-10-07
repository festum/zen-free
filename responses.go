package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const quartetDescription = "This tool is currently unavailable and must not be used."

var effortSuffix = regexp.MustCompile(`\s*\([^()]*\)$`)

func normalizeModel(model string) string {
	model = strings.TrimPrefix(model, "vendor/")
	return effortSuffix.ReplaceAllString(model, "")
}

func isMuseModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(normalizeModel(model)), "muse-spark") ||
		strings.HasPrefix(strings.ToLower(normalizeModel(model)), "musespark") ||
		strings.HasPrefix(strings.ToLower(normalizeModel(model)), "muse_spark")
}

func isJevModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(normalizeModel(model)), "jev")
}

func buildResponsesRequest(chat map[string]any) map[string]any {
	responses := map[string]any{"stream": true}
	if model, ok := chat["model"]; ok {
		responses["model"] = model
	}

	messages, _ := chat["messages"].([]any)
	var instructions []string
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "system" {
			continue
		}
		if text, ok := message["content"].(string); ok {
			instructions = append(instructions, strings.TrimSpace(text))
		}
	}
	if joined := strings.TrimSpace(strings.Join(instructions, "\n\n")); joined != "" {
		responses["instructions"] = joined
	}

	allowedCalls := pairedToolCalls(messages)
	input := make([]any, 0, len(messages))
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch message["role"] {
		case "system":
			continue
		case "user":
			if item, ok := userInputItem(message["content"]); ok {
				input = append(input, item)
			}
		case "assistant":
			if text, ok := message["content"].(string); ok && text != "" {
				input = append(input, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}})
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok || !allowedCalls[i][toolCallID(call)] {
						continue
					}
					function, _ := call["function"].(map[string]any)
					arguments, exists := function["arguments"]
					if !exists {
						arguments = "{}"
					}
					input = append(input, map[string]any{"type": "function_call", "call_id": call["id"], "name": function["name"], "arguments": arguments})
				}
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if id == "" || !hasEarlierToolCall(messages, i, id) {
				continue
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": id, "output": message["content"]})
		}
	}
	responses["input"] = input

	tools, _ := chat["tools"].([]any)
	flatTools := make([]any, 0, len(tools)+4)
	present := make(map[string]bool)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			flatTools = append(flatTools, raw)
			continue
		}
		if function, ok := tool["function"].(map[string]any); ok {
			flat := map[string]any{"type": "function"}
			if name, exists := function["name"]; exists {
				flat["name"] = name
			}
			for _, key := range []string{"description", "parameters"} {
				if value, exists := function[key]; exists {
					flat[key] = value
				}
			}
			tool = flat
		}
		flatTools = append(flatTools, tool)
		if tool["type"] == "function" {
			if name, ok := tool["name"].(string); ok {
				present[strings.ToLower(name)] = true
			}
		}
	}
	for _, name := range []string{"bash", "glob", "grep", "read"} {
		if present[name] {
			continue
		}
		flatTools = append(flatTools, map[string]any{"type": "function", "name": name, "description": quartetDescription, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}})
	}
	responses["tools"] = flatTools
	if choice, ok := chat["tool_choice"].(string); ok && choice == "auto" {
		responses["tool_choice"] = choice
	}

	for _, key := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens", "stop", "seed", "user", "parallel_tool_calls", "presence_penalty", "frequency_penalty"} {
		if value, exists := chat[key]; exists {
			outKey := key
			if key == "max_tokens" || key == "max_completion_tokens" {
				outKey = "max_output_tokens"
			}
			responses[outKey] = value
		}
	}
	return responses
}

func pairedToolCalls(messages []any) []map[string]bool {
	paired := make([]map[string]bool, len(messages))
	for i, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			id := toolCallID(call)
			if id == "" || !hasLaterToolReply(messages, i, id) {
				continue
			}
			if paired[i] == nil {
				paired[i] = make(map[string]bool)
			}
			paired[i][id] = true
		}
	}
	return paired
}

func toolCallID(call map[string]any) string {
	id, _ := call["id"].(string)
	return id
}

func hasEarlierToolCall(messages []any, before int, id string) bool {
	for i := 0; i < before; i++ {
		message, ok := messages[i].(map[string]any)
		if !ok || message["role"] != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if ok && toolCallID(call) == id {
				return true
			}
		}
	}
	return false
}

func hasLaterToolReply(messages []any, after int, id string) bool {
	for i := after + 1; i < len(messages); i++ {
		message, ok := messages[i].(map[string]any)
		if ok && message["role"] == "tool" && message["tool_call_id"] == id {
			return true
		}
	}
	return false
}

func userInputItem(content any) (map[string]any, bool) {
	parts := make([]any, 0)
	switch value := content.(type) {
	case string:
		parts = append(parts, map[string]any{"type": "input_text", "text": value})
	case []any:
		for _, raw := range value {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				if text, ok := part["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "input_text", "text": text})
				}
			case "image_url":
				imageURL := part["image_url"]
				if image, ok := imageURL.(map[string]any); ok {
					imageURL = image["url"]
				}
				if url, ok := imageURL.(string); ok {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
				}
			}
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	return map[string]any{"type": "message", "role": "user", "content": parts}, true
}
func sanitizeResponsesToolChoice(body map[string]any) bool {
	choice, exists := body["tool_choice"]
	if !exists {
		return false
	}
	if value, ok := choice.(string); ok && value == "auto" {
		return false
	}
	delete(body, "tool_choice")
	return true
}

type responseToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type responseReplayState struct {
	id              string
	created         int64
	rolePreludeSent bool
}

type responseSSEState struct {
	id          string
	created     int64
	roleSent    bool
	anyToolCall bool
	toolIndexes map[string]int
	itemIndexes map[string]int
	toolCalls   []*responseToolCall
	text        strings.Builder
	usage       map[string]any
	finish      string
	terminal    bool
	failure     string
	contentSent bool
}

func newResponseSSEState() (*responseSSEState, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, fmt.Errorf("generate response id: %w", err)
	}
	return &responseSSEState{id: "chatcmpl-" + hex.EncodeToString(random[:]), created: time.Now().Unix(), toolIndexes: make(map[string]int), itemIndexes: make(map[string]int)}, nil
}

func writeResponsesAsChat(w http.ResponseWriter, upstream *http.Response, model string, streaming bool, replay *responseReplayState) (int, bool) {
	state, err := newResponseSSEState()
	if err != nil {
		return http.StatusBadGateway, false
	}
	if replay.id == "" {
		replay.id, replay.created = state.id, state.created
	}
	state.id, state.created = replay.id, replay.created
	state.roleSent = replay.rolePreludeSent
	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}

	process := func(payload []byte) error {
		var event map[string]any
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil
		}
		eventType, _ := event["type"].(string)
		if eventType == "" {
			return nil
		}
		data, _ := event["delta"].(string)
		if response, ok := event["response"].(map[string]any); ok && (eventType == "response.created" || eventType == "response.completed") {
			if id, ok := response["id"].(string); ok && id != "" && !replay.rolePreludeSent {
				state.id, replay.id = id, id
			}
			if created, ok := response["created_at"].(float64); ok && !replay.rolePreludeSent {
				state.created, replay.created = int64(created), int64(created)
			}
			if eventType == "response.completed" {
				state.usage = responseUsage(response["usage"])
				if incomplete, ok := response["incomplete_details"]; ok && incomplete != nil {
					state.finish = "length"
				} else if state.anyToolCall {
					state.finish = "tool_calls"
				} else {
					state.finish = "stop"
				}
				state.terminal = true
			}
		}
		if eventErr, exists := event["error"]; exists && eventErr != nil {
			state.failure = upstreamErrorMessage(eventErr)
			state.terminal = true
		} else if eventType == "response.failed" {
			state.failure = upstreamErrorMessage(event["response"])
			state.terminal = true
		}
		if state.failure != "" {
			if streaming {
				if err := writeSSE(w, map[string]any{"error": map[string]any{"message": state.failure, "type": "upstream_error"}}); err != nil {
					return err
				}
				return writeDone(w)
			}
			return nil
		}
		switch eventType {
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			switch item["type"] {
			case "message":
				return sendRolePrelude(w, state, model, streaming, replay)
			case "function_call":
				callID, _ := item["call_id"].(string)
				if callID == "" {
					return nil
				}
				itemID, _ := item["id"].(string)
				index := state.registerTool(callID, itemID, item["name"])
				if streaming {
					state.contentSent = true
					return writeChatChunk(w, state, model, map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": callID, "type": "function", "function": map[string]any{"name": stringValue(item["name"]), "arguments": ""}}}}, nil, nil)
				}
			}
		case "response.output_text.delta":
			if data == "" {
				return nil
			}
			if err := sendRolePrelude(w, state, model, streaming, replay); err != nil {
				return err
			}
			state.text.WriteString(data)
			if streaming {
				state.contentSent = true
				return writeChatChunk(w, state, model, map[string]any{"content": data}, nil, nil)
			}
		case "response.function_call_arguments.delta":
			callID, _ := event["call_id"].(string)
			if callID == "" {
				callID, _ = event["item_id"].(string)
			}
			index, ok := state.toolIndexes[callID]
			if !ok {
				index, ok = state.itemIndexes[callID]
			}
			if !ok {
				return nil
			}
			state.toolCalls[index].Function.Arguments += data
			if streaming && data != "" {
				state.contentSent = true
				return writeChatChunk(w, state, model, map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": data}}}}, nil, nil)
			}
		case "response.completed":
			if streaming {
				if err := writeChatChunk(w, state, model, map[string]any{}, state.finish, state.usage); err != nil {
					return err
				}
				return writeDone(w)
			}
		}
		return nil
	}

	defer upstream.Body.Close()
	scanner := bufio.NewScanner(upstream.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataLines []string
	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := []byte(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		return process(payload)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := processEvent(); err != nil {
				if streaming && !state.contentSent {
					return http.StatusBadGateway, true
				}
				return writeSSEFailure(w, streaming, "upstream response failed"), false
			}
			if state.terminal {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			dataLines = append(dataLines, strings.TrimPrefix(value, " "))
		}
	}
	if !state.terminal && len(dataLines) > 0 {
		if err := processEvent(); err != nil {
			if streaming && !state.contentSent {
				return http.StatusBadGateway, true
			}
			return writeSSEFailure(w, streaming, "upstream response failed"), false
		}
	}
	if scanner.Err() != nil || !state.terminal && state.failure == "" {
		if streaming && !state.contentSent {
			return http.StatusBadGateway, true
		}
		return writeStreamInterrupted(w, streaming), false
	}
	if state.failure != "" {
		if streaming {
			return http.StatusOK, false
		}
		return writeJSONError(w, state.failure), false
	}
	if streaming {
		return http.StatusOK, false
	}
	completion := map[string]any{"id": state.id, "object": "chat.completion", "created": state.created, "model": model, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": state.text.String(), "tool_calls": state.toolCalls}, "finish_reason": state.finish}}, "usage": state.usage}
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(completion); err != nil {
		return http.StatusOK, false
	}
	return http.StatusOK, false
}

func writeStreamInterrupted(w http.ResponseWriter, streaming bool) int {
	if !streaming {
		return writeJSONError(w, "upstream response ended before completion")
	}
	_, _ = io.WriteString(w, interruptedStreamPair)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return http.StatusBadGateway
}

func (state *responseSSEState) registerTool(callID, itemID string, name any) int {
	if index, ok := state.toolIndexes[callID]; ok {
		return index
	}
	index := len(state.toolCalls)
	state.toolIndexes[callID] = index
	if itemID != "" {
		state.itemIndexes[itemID] = index
	}
	state.anyToolCall = true
	call := &responseToolCall{ID: callID, Type: "function"}
	call.Function.Name = stringValue(name)
	state.toolCalls = append(state.toolCalls, call)
	return index

}

func responseUsage(raw any) map[string]any {
	usageMap, ok := raw.(map[string]any)
	if !ok {
		return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	usage := map[string]any{"prompt_tokens": usageMap["input_tokens"], "completion_tokens": usageMap["output_tokens"], "total_tokens": usageMap["total_tokens"]}
	if usage["prompt_tokens"] == nil {
		usage["prompt_tokens"] = 0
	}
	if usage["completion_tokens"] == nil {
		usage["completion_tokens"] = 0
	}
	if usage["total_tokens"] == nil {
		usage["total_tokens"] = 0
	}
	if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
		if cached, exists := details["cached_tokens"]; exists {
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := usageMap["output_tokens_details"].(map[string]any); ok {
		if reasoning, exists := details["reasoning_tokens"]; exists {
			usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	return usage
}

func sendRolePrelude(w http.ResponseWriter, state *responseSSEState, model string, streaming bool, replay *responseReplayState) error {
	if state.roleSent || !streaming {
		return nil
	}
	state.roleSent = true
	replay.rolePreludeSent = true
	return writeChatChunk(w, state, model, map[string]any{"role": "assistant", "content": ""}, nil, nil)
}

func writeChatChunk(w http.ResponseWriter, state *responseSSEState, model string, delta map[string]any, finish any, usage map[string]any) error {
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = finish
	}
	chunk := map[string]any{"id": state.id, "object": "chat.completion.chunk", "created": state.created, "model": model, "choices": []any{choice}}
	if finish != nil {
		chunk["usage"] = usage
	}
	return writeSSE(w, chunk)
}

func writeSSE(w http.ResponseWriter, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode chat event: %w", err)
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return fmt.Errorf("write chat event: %w", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeDone(w http.ResponseWriter) error {
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return fmt.Errorf("write completion marker: %w", err)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeSSEFailure(w http.ResponseWriter, streaming bool, message string) int {
	if streaming {
		_ = writeSSE(w, map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
		_ = writeDone(w)
		return http.StatusBadGateway
	}
	return writeJSONError(w, message)
}

func writeJSONError(w http.ResponseWriter, message string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": "upstream_error"}})
	return http.StatusBadGateway
}

func upstreamErrorMessage(raw any) string {
	if value, ok := raw.(map[string]any); ok {
		if nested, ok := value["error"]; ok {
			return upstreamErrorMessage(nested)
		}
		if message, ok := value["message"].(string); ok && message != "" {
			return message
		}
	}
	return "upstream response failed"
}

func stringValue(raw any) string {
	value, _ := raw.(string)
	return value
}
