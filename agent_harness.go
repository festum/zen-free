package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

const preambleMarker = "You are an AI agent running in OpenCode"

const v2DefaultTokenBudget = 32000

//go:embed testdata/agent-preamble.txt
var agentPreambleRaw string

//go:embed testdata/agent-tools.json
var agentToolsJSON []byte

var agentPreamble = strings.TrimSpace(agentPreambleRaw)

var agentManifestTools = mustParseAgentManifest(agentToolsJSON)

var bridgedManifestTools = bridgeManifestTools(agentManifestTools, capturedCoreToolNames())

func mustParseAgentManifest(raw []byte) []any {
	var tools []any
	if err := json.Unmarshal(raw, &tools); err != nil {
		panic(fmt.Errorf("parse agent tool manifest: %w", err))
	}
	if len(tools) == 0 {
		panic("agent tool manifest is empty")
	}
	for _, tool := range tools {
		object, ok := tool.(map[string]any)
		if !ok || toolName(object) == "" {
			panic(fmt.Errorf("agent tool manifest entry lacks a function name: %#v", tool))
		}
	}
	return tools
}

func capturedCoreToolNames() map[string]bool {
	names := make(map[string]bool, len(capturedOpenCodeTools))
	for _, raw := range capturedOpenCodeTools {
		names[toolName(raw.(map[string]any))] = true
	}
	return names
}

func bridgeManifestTools(tools []any, coreNames map[string]bool) []any {
	bridged := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if !coreNames[toolName(tool)] {
			continue
		}
		bridged = append(bridged, tool)
	}
	return bridged
}

func wireTool(raw any, nested bool) map[string]any {
	tool := cloneToolValue(raw).(map[string]any)
	if nested {
		return tool
	}
	function, _ := tool["function"].(map[string]any)
	flat := map[string]any{"type": "function"}
	for key, value := range cloneToolValue(function).(map[string]any) {
		flat[key] = value
	}
	return flat
}

func injectAgentPreamble(body map[string]any) {
	if agentPreamble == "" {
		return
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || message["role"] != "system" {
				continue
			}
			switch content := message["content"].(type) {
			case string:
				if strings.HasPrefix(content, preambleMarker) {
					return
				}
				message["content"] = agentPreamble + "\n\n" + content
			case []any:
				if len(content) > 0 {
					if part, ok := content[0].(map[string]any); ok {
						if text, ok := part["text"].(string); ok && strings.HasPrefix(text, preambleMarker) {
							return
						}
					}
				}
				message["content"] = append([]any{map[string]any{"type": "text", "text": agentPreamble}}, content...)
			default:
				message["content"] = agentPreamble
			}
			return
		}
		body["messages"] = append([]any{map[string]any{"role": "system", "content": agentPreamble}}, messages...)
		return
	}
	if instructions, ok := body["instructions"].(string); ok {
		if strings.HasPrefix(instructions, preambleMarker) {
			return
		}
		if instructions == "" {
			body["instructions"] = agentPreamble
		} else {
			body["instructions"] = agentPreamble + "\n\n" + instructions
		}
		return
	}
	if _, ok := body["input"]; ok {
		if _, exists := body["instructions"]; !exists {
			body["instructions"] = agentPreamble
		}
	}
}

func applyChatStreamWireDefaults(body map[string]any) {
	streamOptions, ok := body["stream_options"].(map[string]any)
	if !ok {
		streamOptions = map[string]any{}
		body["stream_options"] = streamOptions
	}
	streamOptions["include_usage"] = true
	if !positiveTokenBudget(body["max_tokens"]) && !positiveTokenBudget(body["max_completion_tokens"]) {
		body["max_completion_tokens"] = float64(v2DefaultTokenBudget)
	}
}

func positiveTokenBudget(value any) bool {
	number, ok := value.(float64)
	return ok && number > 0
}
