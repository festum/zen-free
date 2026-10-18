package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamSessionHeadersPaired(t *testing.T) {
	const (
		validSession   = "ses_ec4cedf23ffep73id5VZlrta6q"
		contextSession = "ses_aaaaaaaaaaaaBBBBBBBBBBBBBBBB"
	)
	tests := []struct {
		name            string
		inbound         func(*http.Request)
		useContextValue bool
		wantSession     string
		wantMinted      bool
	}{
		{
			name:       "no inbound session",
			wantMinted: true,
		},
		{
			name: "inbound session",
			inbound: func(r *http.Request) {
				r.Header.Set("x-opencode-session", validSession)
			},
			wantSession: validSession,
		},
		{
			name: "inbound session id only",
			inbound: func(r *http.Request) {
				r.Header.Set("x-opencode-session-id", validSession)
			},
			wantSession: validSession,
		},
		{
			name: "invalid inbound session is replaced",
			inbound: func(r *http.Request) {
				r.Header.Set("x-opencode-session", "not-a-session")
			},
			wantMinted: true,
		},
		{
			name: "conflicting pair prefers session",
			inbound: func(r *http.Request) {
				r.Header.Set("x-opencode-session", validSession)
				r.Header.Set("x-opencode-session-id", contextSession)
			},
			wantSession: validSession,
		},
		{
			name: "context session wins",
			inbound: func(r *http.Request) {
				r.Header.Set("x-opencode-session", validSession)
			},
			useContextValue: true,
			wantSession:     contextSession,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotSession, gotSessionID, gotAffinity, gotProject string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSession = r.Header.Get("x-opencode-session")
				gotSessionID = r.Header.Get("x-opencode-session-id")
				gotAffinity = r.Header.Get("x-session-affinity")
				gotProject = r.Header.Get("x-opencode-project")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list"}`)
			}))
			defer upstream.Close()

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.inbound != nil {
				tt.inbound(request)
			}
			if tt.useContextValue {
				request = request.WithContext(context.WithValue(request.Context(), contextKey{}, contextSession))
			}
			recorder := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
			}
			if gotSession == "" || gotSession != gotSessionID || gotSession != gotAffinity {
				t.Fatalf("upstream session affinity headers = (%q, %q, %q), want equal non-empty values", gotSession, gotSessionID, gotAffinity)
			}
			if len(gotProject) != 40 || strings.Trim(gotProject, "0123456789abcdef") != "" {
				t.Fatalf("upstream project ID = %q, want 40 lowercase hexadecimal characters", gotProject)
			}
			if gotProject != opencodeProjectID {
				t.Fatalf("upstream project ID = %q, want process ID %q", gotProject, opencodeProjectID)
			}
			if tt.wantSession != "" && gotSession != tt.wantSession {
				t.Fatalf("upstream session = %q, want %q", gotSession, tt.wantSession)
			}
			if tt.wantMinted && !validSessionID(gotSession) {
				t.Fatalf("upstream session = %q, want minted ses_ id", gotSession)
			}
		})
	}
}

func TestUpstreamAuthorizationFromGoKeyEnv(t *testing.T) {
	tests := []struct {
		name       string
		goKey      string
		wantHeader string
	}{
		{name: "go key overrides inbound", goKey: "sk-test-go-key", wantHeader: "Bearer sk-test-go-key"},
		{name: "empty key forwards inbound", goKey: "", wantHeader: "Bearer inbound-key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE_GO_API_KEY", tt.goKey)
			var gotAuthorization string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuthorization = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list"}`)
			}))
			defer upstream.Close()

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer inbound-key")
			recorder := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
			}
			if gotAuthorization != tt.wantHeader {
				t.Fatalf("upstream Authorization = %q, want %q", gotAuthorization, tt.wantHeader)
			}
		})
	}
}

func TestUnavailableModelErrorSurfacesToClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"Upstream request failed: Model is unavailable."}}`)
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"not-in-go-catalog","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Model is unavailable.") {
		t.Fatalf("client body = %q, want upstream model error surfaced verbatim", recorder.Body.String())
	}
}
