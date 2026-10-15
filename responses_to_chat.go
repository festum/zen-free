package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func mintRespID() string {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(random[:])
}

func mintItemID(prefix string) string {
	return prefix + strings.TrimPrefix(mintRespID(), "resp_")
}

func buildChatCompletionsRequest(responses map[string]any) map[string]any {
	chat := map[string]any{"messages": []any{}}
	if model, ok := responses["model"]; ok {
		chat["model"] = model
	}
	if stream, ok := responses["stream"]; ok {
		chat["stream"] = stream
	}
	messages := chat["messages"].([]any)
	if instructions, ok := responses["instructions"].(string); ok && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	lastAssistant := func() map[string]any {
		if len(messages) == 0 {
			return nil
		}
		last, ok := messages[len(messages)-1].(map[string]any)
		if !ok || last["role"] != "assistant" {
			return nil
		}
		return last
	}

	switch input := responses["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": input})
	case []any:
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch item["type"] {
			case "message":
				role := stringValue(item["role"])
				if role != "user" && role != "assistant" && role != "system" {
					role = "user"
				}
				content := chatContent(item["content"])
				if parts, isParts := content.([]any); isParts && len(parts) == 0 {
					continue
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			case "function_call":
				assistant := lastAssistant()
				if assistant == nil {
					assistant = map[string]any{"role": "assistant", "content": ""}
					messages = append(messages, assistant)
				}
				arguments, ok := item["arguments"].(string)
				if !ok {
					arguments = "{}"
				}
				call := map[string]any{
					"id":       item["call_id"],
					"type":     "function",
					"function": map[string]any{"name": item["name"], "arguments": arguments},
				}
				calls, _ := assistant["tool_calls"].([]any)
				assistant["tool_calls"] = append(calls, call)
			case "function_call_output":
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": item["call_id"], "content": item["output"]})
			}
		}
	}
	chat["messages"] = messages

	flatTools, _ := responses["tools"].([]any)
	tools := make([]any, 0, len(flatTools))
	for _, rawTool := range flatTools {
		tool, ok := rawTool.(map[string]any)
		if !ok || tool["type"] != "function" {
			tools = append(tools, rawTool)
			continue
		}
		function := map[string]any{}
		for _, key := range []string{"name", "description", "parameters"} {
			if value, exists := tool[key]; exists {
				function[key] = value
			}
		}
		tools = append(tools, map[string]any{"type": "function", "function": function})
	}
	chat["tools"] = tools

	switch choice := responses["tool_choice"].(type) {
	case string:
		chat["tool_choice"] = choice
	case map[string]any:
		name := stringValue(choice["name"])
		if nested, ok := choice["function"].(map[string]any); ok && name == "" {
			name = stringValue(nested["name"])
		}
		if name != "" {
			chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}

	if temperature, ok := responses["temperature"]; ok {
		chat["temperature"] = temperature
	}
	if maxTokens, ok := responses["max_output_tokens"]; ok {
		chat["max_tokens"] = maxTokens
	}
	return chat
}

func chatContent(raw any) any {
	switch value := raw.(type) {
	case string:
		return value
	case []any:
		parts := make([]any, 0, len(value))
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "text", "input_text", "output_text":
				if text, ok := part["text"].(string); ok {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "input_image", "image_url":
				imageURL := part["image_url"]
				if image, ok := imageURL.(map[string]any); ok {
					imageURL = image["url"]
				}
				if url, ok := imageURL.(string); ok {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": url})
				}
			}
		}
		return parts
	}
	return raw
}

func responsesUsageFromChat(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	if usage == nil {
		usage = map[string]any{}
	}
	out := map[string]any{
		"input_tokens":  usage["prompt_tokens"],
		"output_tokens": usage["completion_tokens"],
		"total_tokens":  usage["total_tokens"],
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if out[key] == nil {
			out[key] = 0
		}
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if cached, exists := details["cached_tokens"]; exists {
			out["input_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		if reasoning, exists := details["reasoning_tokens"]; exists {
			out["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
		}
	}
	return out
}

type responsesEventWriter struct {
	w        http.ResponseWriter
	sequence int
}

func (e *responsesEventWriter) emit(eventType string, payload map[string]any) error {
	body := map[string]any{"type": eventType, "sequence_number": e.sequence}
	e.sequence++
	for key, value := range payload {
		body[key] = value
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode responses event: %w", err)
	}
	if _, err := fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", eventType, encoded); err != nil {
		return fmt.Errorf("write responses event: %w", err)
	}
	if flusher, ok := e.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

type responsesStreamState struct {
	writer     *responsesEventWriter
	responseID string
	created    int64
	model      string
	output     []any
	messageID  string
	msgIndex   int
	msgOpen    bool
	text       strings.Builder
	toolCalls  []*responseToolCall
	toolItems  []map[string]any
	toolIndex  map[int]int
	usage      map[string]any
	finish     string
	failure    string
	sawEvent   bool
	terminal   bool
}

func (s *responsesStreamState) snapshot(status string, incomplete bool) map[string]any {
	response := map[string]any{
		"id":         s.responseID,
		"object":     "response",
		"created_at": s.created,
		"status":     status,
		"model":      s.model,
		"output":     s.output,
	}
	if s.usage != nil {
		response["usage"] = s.usage
	}
	if incomplete {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return response
}

func (s *responsesStreamState) ensureMessage() error {
	if s.msgOpen {
		return nil
	}
	s.msgOpen = true
	s.messageID = mintItemID("msg_")
	s.msgIndex = len(s.output)
	item := map[string]any{"type": "message", "id": s.messageID, "status": "in_progress", "role": "assistant", "content": []any{}}
	s.output = append(s.output, item)
	if err := s.writer.emit("response.output_item.added", map[string]any{"item": item, "output_index": s.msgIndex}); err != nil {
		return err
	}
	return s.writer.emit("response.content_part.added", map[string]any{"item_id": s.messageID, "output_index": s.msgIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": ""}})
}

func (s *responsesStreamState) ensureTool(chunk map[string]any) (int, error) {
	slot := 0
	if value, ok := chunk["index"].(float64); ok && int(value) > 0 {
		slot = int(value)
	}
	if index, ok := s.toolIndex[slot]; ok {
		return index, nil
	}
	call := &responseToolCall{ID: stringValue(chunk["id"]), Type: "function"}
	if function, ok := chunk["function"].(map[string]any); ok {
		call.Function.Name = stringValue(function["name"])
	}
	index := len(s.toolCalls)
	s.toolCalls = append(s.toolCalls, call)
	s.toolIndex[slot] = index
	item := map[string]any{"type": "function_call", "id": mintItemID("fc_"), "call_id": call.ID, "name": call.Function.Name, "arguments": "", "status": "in_progress"}
	s.toolItems = append(s.toolItems, item)
	outputIndex := len(s.output)
	s.output = append(s.output, item)
	if err := s.writer.emit("response.output_item.added", map[string]any{"item": item, "output_index": outputIndex}); err != nil {
		return index, err
	}
	return index, nil
}

func (s *responsesStreamState) finishStream() error {
	incomplete := s.finish == "length"
	status := "completed"
	if incomplete {
		status = "incomplete"
	}
	if s.msgOpen {
		if err := s.writer.emit("response.output_text.done", map[string]any{"item_id": s.messageID, "output_index": s.msgIndex, "content_index": 0, "text": s.text.String()}); err != nil {
			return err
		}
		item := s.output[s.msgIndex].(map[string]any)
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "text": s.text.String()}}
		if err := s.writer.emit("response.output_item.done", map[string]any{"item": item, "output_index": s.msgIndex}); err != nil {
			return err
		}
	}
	for index := range s.toolCalls {
		item := s.toolItems[index]
		outputIndex := len(s.output) - len(s.toolItems) + index
		if err := s.writer.emit("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": outputIndex, "call_id": s.toolCalls[index].ID, "name": s.toolCalls[index].Function.Name, "arguments": s.toolCalls[index].Function.Arguments}); err != nil {
			return err
		}
		item["status"] = "completed"
		item["arguments"] = s.toolCalls[index].Function.Arguments
		if err := s.writer.emit("response.output_item.done", map[string]any{"item": item, "output_index": outputIndex}); err != nil {
			return err
		}
	}
	if s.usage == nil {
		s.usage = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	}
	return s.writer.emit("response.completed", map[string]any{"response": s.snapshot(status, incomplete)})
}

func writeChatStreamAsResponses(w http.ResponseWriter, body io.Reader, model string) (int, bool) {
	if model == "" {
		model = "unknown"
	}
	state := &responsesStreamState{
		writer:     &responsesEventWriter{w: w},
		responseID: mintRespID(),
		created:    time.Now().Unix(),
		model:      model,
		output:     []any{},
		toolIndex:  make(map[int]int),
	}
	if err := state.writer.emit("response.created", map[string]any{"response": state.snapshot("in_progress", false)}); err != nil {
		return http.StatusBadGateway, true
	}
	if err := state.writer.emit("response.in_progress", map[string]any{"response": state.snapshot("in_progress", false)}); err != nil {
		return http.StatusBadGateway, true
	}

	process := func(payload []byte) error {
		if string(payload) == "[DONE]" {
			state.terminal = true
			return nil
		}
		var chunk map[string]any
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil
		}
		state.sawEvent = true
		if eventErr, exists := chunk["error"]; exists && eventErr != nil {
			state.failure = upstreamErrorMessage(eventErr)
			return nil
		}
		if rawUsage, exists := chunk["usage"]; exists && rawUsage != nil {
			if _, ok := rawUsage.(map[string]any); ok {
				state.usage = responsesUsageFromChat(rawUsage)
			}
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			return nil
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			return nil
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta = map[string]any{}
		}
		if content := stringValue(delta["content"]); content != "" {
			if err := state.ensureMessage(); err != nil {
				return err
			}
			state.text.WriteString(content)
			if err := state.writer.emit("response.output_text.delta", map[string]any{"item_id": state.messageID, "output_index": state.msgIndex, "content_index": 0, "delta": content}); err != nil {
				return err
			}
		} else if !state.msgOpen && stringValue(delta["role"]) == "assistant" {
			if err := state.ensureMessage(); err != nil {
				return err
			}
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				index, err := state.ensureTool(call)
				if err != nil {
					return err
				}
				arguments := ""
				if function, ok := call["function"].(map[string]any); ok {
					arguments = stringValue(function["arguments"])
				}
				if arguments == "" {
					continue
				}
				state.toolCalls[index].Function.Arguments += arguments
				item := state.toolItems[index]
				outputIndex := len(state.output) - len(state.toolItems) + index
				if err := state.writer.emit("response.function_call_arguments.delta", map[string]any{"item_id": item["id"], "output_index": outputIndex, "call_id": state.toolCalls[index].ID, "name": state.toolCalls[index].Function.Name, "delta": arguments}); err != nil {
					return err
				}
			}
		}
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			state.finish = reason
		}
		return nil
	}

	reader := bufio.NewReaderSize(body, 64*1024)
	var dataLines []string
	var scanErr error
	processEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		payload := []byte(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		return process(payload)
	}
	for {
		line, readErr := readSSELine(reader)
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if err := processEvent(); err != nil {
					return writeStreamInterrupted(w, true), false
				}
				if state.terminal || state.failure != "" {
					break
				}
			} else if strings.HasPrefix(trimmed, "data:") {
				value := strings.TrimPrefix(trimmed, "data:")
				dataLines = append(dataLines, strings.TrimPrefix(value, " "))
			}
		}
		if readErr != nil {
			if errors.Is(readErr, errSSELineTooLong) {
				if !state.sawEvent {
					return http.StatusBadGateway, true
				}
				return writeStreamInterrupted(w, true), false
			}
			if !errors.Is(readErr, io.EOF) {
				scanErr = readErr
			}
			break
		}
	}
	if !state.terminal && state.failure == "" && len(dataLines) > 0 {
		if err := processEvent(); err != nil {
			return writeStreamInterrupted(w, true), false
		}
	}
	if state.failure != "" {
		if err := writeSSE(w, map[string]any{"error": map[string]any{"message": state.failure, "type": "upstream_error"}}); err != nil {
			return http.StatusBadGateway, false
		}
		_ = writeDone(w)
		return http.StatusOK, false
	}
	if scanErr != nil || (!state.terminal && state.failure == "" && state.finish == "") {
		if !state.sawEvent {
			return http.StatusBadGateway, true
		}
		return writeStreamInterrupted(w, true), false
	}
	if err := state.finishStream(); err != nil {
		return http.StatusBadGateway, false
	}
	if err := writeDone(w); err != nil {
		return http.StatusBadGateway, false
	}
	return http.StatusOK, false
}

func chatCompletionToResponse(chat map[string]any) map[string]any {
	responseID := mintRespID()
	created := time.Now().Unix()
	switch value := chat["created"].(type) {
	case int64:
		if value > 0 {
			created = value
		}
	case float64:
		if value > 0 {
			created = int64(value)
		}
	}
	model, _ := chat["model"].(string)
	finish := "stop"
	var message map[string]any
	var toolCalls []*responseToolCall
	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if reason := stringValue(choice["finish_reason"]); reason != "" {
				finish = reason
			}
			message, _ = choice["message"].(map[string]any)
		}
	}
	if message != nil {
		if calls, ok := message["tool_calls"].([]*responseToolCall); ok {
			toolCalls = calls
		} else if rawCalls, ok := message["tool_calls"].([]any); ok {
			for _, rawCall := range rawCalls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				converted := &responseToolCall{ID: stringValue(call["id"]), Type: "function"}
				if function, ok := call["function"].(map[string]any); ok {
					converted.Function.Name = stringValue(function["name"])
					converted.Function.Arguments = stringValue(function["arguments"])
				}
				toolCalls = append(toolCalls, converted)
			}
		}
	}
	output := []any{}
	if message != nil {
		if content, _ := message["content"].(string); content != "" {
			output = append(output, map[string]any{"type": "message", "id": mintItemID("msg_"), "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": content}}})
		}
	}
	for _, call := range toolCalls {
		output = append(output, map[string]any{"type": "function_call", "id": mintItemID("fc_"), "call_id": call.ID, "name": call.Function.Name, "arguments": call.Function.Arguments, "status": "completed"})
	}
	response := map[string]any{
		"id":         responseID,
		"object":     "response",
		"status":     "completed",
		"created_at": created,
		"model":      model,
		"output":     output,
		"usage":      responsesUsageFromChat(chat["usage"]),
	}
	if finish == "length" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return response
}
