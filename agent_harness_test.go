package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func manifestTool(name string) map[string]any {
	for _, raw := range agentManifestTools {
		if tool := raw.(map[string]any); toolName(tool) == name {
			return tool
		}
	}
	return nil
}

func containsManifestName(name string) bool {
	return manifestTool(name) != nil
}

func callerTool(tools []any, name string) any {
	for _, raw := range tools {
		if tool, ok := raw.(map[string]any); ok && toolName(tool) == name {
			return raw
		}
	}
	return nil
}

func assertWiredToolSchema(t *testing.T, raw any, name string, nested bool) {
	t.Helper()
	var want map[string]any
	switch {
	case name == "bash":
		want = capturedTool(name, nested)
	case manifestTool(name) != nil:
		want = wireTool(manifestTool(name), nested)
	default:
		return
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("tool %q schema = %s, want %s", name, gotJSON, wantJSON)
	}
}

func TestEmbeddedAgentAssetsMatchReference(t *testing.T) {
	if len(agentPreamble) != 8245 {
		t.Fatalf("agent preamble length = %d bytes, want 8245", len(agentPreamble))
	}
	if !strings.HasPrefix(agentPreamble, preambleMarker) {
		t.Fatalf("agent preamble prefix = %.60q, want prefix %q", agentPreamble, preambleMarker)
	}
	wantNames := []string{"edit", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "execute"}
	if len(agentManifestTools) != len(wantNames) {
		t.Fatalf("manifest tool count = %d, want %d", len(agentManifestTools), len(wantNames))
	}
	for i, want := range wantNames {
		if got := toolName(agentManifestTools[i].(map[string]any)); got != want {
			t.Errorf("manifest tool %d = %q, want %q", i, got, want)
		}
	}
	bridged := []string{"edit", "glob", "grep", "read", "write"}
	if len(bridgedManifestTools) != len(bridged) {
		t.Fatalf("bridged tool count = %d, want %d", len(bridgedManifestTools), len(bridged))
	}
	for i, want := range bridged {
		if got := toolName(bridgedManifestTools[i].(map[string]any)); got != want {
			t.Errorf("bridged tool %d = %q, want %q", i, got, want)
		}
	}
}

func TestInjectAgentPreambleIdempotentAcrossShapes(t *testing.T) {
	t.Run("prepends system message", func(t *testing.T) {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
		injectAgentPreamble(body)
		first := object(t, array(t, body["messages"])[0])
		if first["role"] != "system" || first["content"] != agentPreamble {
			t.Fatalf("first message = %#v, want preamble system message", first)
		}
		injectAgentPreamble(body)
		if got := len(array(t, body["messages"])); got != 2 {
			t.Fatalf("message count after second injection = %d, want 2", got)
		}
	})
	t.Run("prefixes system string content", func(t *testing.T) {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "system", "content": "house rules"},
			map[string]any{"role": "user", "content": "hello"},
		}}
		injectAgentPreamble(body)
		first := object(t, array(t, body["messages"])[0])
		if first["content"] != agentPreamble+"\n\nhouse rules" {
			t.Fatalf("system content = %q, want preamble prefix", first["content"])
		}
		injectAgentPreamble(body)
		if got := first["content"]; got != agentPreamble+"\n\nhouse rules" {
			t.Fatalf("system content after second injection = %q, want unchanged", got)
		}
	})
	t.Run("prepends system array content", func(t *testing.T) {
		body := map[string]any{"messages": []any{map[string]any{
			"role":    "system",
			"content": []any{map[string]any{"type": "text", "text": "house rules"}},
		}}}
		injectAgentPreamble(body)
		content := array(t, object(t, array(t, body["messages"])[0])["content"])
		if got := object(t, content[0])["text"]; got != agentPreamble {
			t.Fatalf("first content part = %q, want preamble", got)
		}
		injectAgentPreamble(body)
		if got := len(array(t, object(t, array(t, body["messages"])[0])["content"])); got != 2 {
			t.Fatalf("content parts after second injection = %d, want 2", got)
		}
	})
	t.Run("prefixes instructions", func(t *testing.T) {
		body := map[string]any{"instructions": "house rules"}
		injectAgentPreamble(body)
		if got := body["instructions"]; got != agentPreamble+"\n\nhouse rules" {
			t.Fatalf("instructions = %q, want preamble prefix", got)
		}
		injectAgentPreamble(body)
		if got := body["instructions"]; got != agentPreamble+"\n\nhouse rules" {
			t.Fatalf("instructions after second injection = %q, want unchanged", got)
		}
	})
	t.Run("sets instructions for input-only responses", func(t *testing.T) {
		body := map[string]any{"input": []any{}}
		injectAgentPreamble(body)
		if got := body["instructions"]; got != agentPreamble {
			t.Fatalf("instructions = %q, want preamble", got)
		}
		injectAgentPreamble(body)
		if got := body["instructions"]; got != agentPreamble {
			t.Fatalf("instructions after second injection = %q, want unchanged", got)
		}
	})
}

func TestApplyChatStreamWireDefaults(t *testing.T) {
	t.Run("adds usage and default budget", func(t *testing.T) {
		body := map[string]any{}
		applyChatStreamWireDefaults(body)
		if got := object(t, body["stream_options"])["include_usage"]; got != true {
			t.Fatalf("include_usage = %#v, want true", got)
		}
		if got := body["max_completion_tokens"]; got != float64(v2DefaultTokenBudget) {
			t.Fatalf("max_completion_tokens = %#v, want %d", got, v2DefaultTokenBudget)
		}
	})
	t.Run("keeps explicit max_tokens budget", func(t *testing.T) {
		body := map[string]any{"max_tokens": float64(4096)}
		applyChatStreamWireDefaults(body)
		if got := body["max_tokens"]; got != float64(4096) {
			t.Fatalf("max_tokens = %#v, want 4096", got)
		}
		if _, ok := body["max_completion_tokens"]; ok {
			t.Fatalf("max_completion_tokens = %#v, want absent when max_tokens set", body["max_completion_tokens"])
		}
	})
	t.Run("keeps explicit completion budget and stream options", func(t *testing.T) {
		body := map[string]any{
			"max_completion_tokens": float64(512),
			"stream_options":        map[string]any{"include_usage": false, "custom": "keep"},
		}
		applyChatStreamWireDefaults(body)
		options := object(t, body["stream_options"])
		if options["include_usage"] != true || options["custom"] != "keep" {
			t.Fatalf("stream_options = %#v, want include_usage true and custom preserved", options)
		}
		if got := body["max_completion_tokens"]; got != float64(512) {
			t.Fatalf("max_completion_tokens = %#v, want 512", got)
		}
	})
}
