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
			var gotSession, gotSessionID, gotSessionAlias, gotAffinity, gotProject, gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotSession = r.Header.Get("x-opencode-session")
				gotSessionID = r.Header.Get("x-opencode-session-id")
				gotAffinity = r.Header.Get("x-session-affinity")
				gotSessionAlias = r.Header.Get("x-session-id")
				gotProject = r.Header.Get("x-opencode-project")
				gotPath = r.URL.RequestURI()
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
			proxyHandler(upstream.URL+"/inference", upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
			}
			if gotPath != "/inference/v1/models" {
				t.Fatalf("upstream catalog path = %q, want /inference/v1/models", gotPath)
			}
			if gotSession == "" || gotSession != gotSessionID || gotSession != gotAffinity || gotSession != gotSessionAlias {
				t.Fatalf("upstream session affinity headers = (%q, %q, %q, %q), want equal non-empty values", gotSession, gotSessionID, gotAffinity, gotSessionAlias)
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
		inbound    string // Authorization to send; empty = omit header
		wantHeader string // upstream Authorization; "" = header must be absent
	}{
		{name: "inbound authorization wins over go key", goKey: "sk-test-env-key", inbound: "Bearer sk-test-inbound-key", wantHeader: "Bearer sk-test-inbound-key"},
		{name: "env fallback when inbound absent", goKey: "sk-test-env-key", inbound: "", wantHeader: "Bearer sk-test-env-key"},
		{name: "no header when both absent", goKey: "", inbound: "", wantHeader: ""},
		{name: "blank inbound and no env key", goKey: "", inbound: " ", wantHeader: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENCODE_GO_API_KEY", tt.goKey)
			var gotAuthorization string
			var hadAuthorization bool
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuthorization = r.Header.Get("Authorization")
				_, hadAuthorization = r.Header["Authorization"]
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list"}`)
			}))
			defer upstream.Close()

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.inbound != "" {
				request.Header.Set("Authorization", tt.inbound)
			}
			recorder := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
			}
			if gotAuthorization != tt.wantHeader {
				t.Fatalf("upstream Authorization = %q, want %q", gotAuthorization, tt.wantHeader)
			}
			if tt.wantHeader == "" && hadAuthorization {
				t.Fatalf("upstream Authorization header present, want absent")
			}
		})
	}
}

func TestUpstreamPreservesDistinctInboundAuthorization(t *testing.T) {
	t.Setenv("OPENCODE_GO_API_KEY", "sk-test-env-key")
	keys := []string{"Bearer sk-test-account-a", "Bearer sk-test-account-b"}
	for _, inboundKey := range keys {
		t.Run(inboundKey, func(t *testing.T) {
			var gotAuthorization string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuthorization = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list"}`)
			}))
			defer upstream.Close()

			request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			request.Header.Set("Authorization", inboundKey)
			recorder := httptest.NewRecorder()
			proxyHandler(upstream.URL, upstream.Client(), &idMint{}).ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("client status = %d body %q", recorder.Code, recorder.Body.String())
			}
			if gotAuthorization != inboundKey {
				t.Fatalf("upstream Authorization = %q, want inbound %q unchanged", gotAuthorization, inboundKey)
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
