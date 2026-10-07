package main

import (
	"os"
	"strings"
	"testing"
)

func TestFingerprintUAContract(t *testing.T) {
	const suffix = " ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

	if expectedVersion := os.Getenv("EXPECTED_OPENCODE_VERSION"); expectedVersion != "" {
		if got, want := opencodeVersion, expectedVersion; got != want {
			t.Fatalf("opencodeVersion = %q, want linker-injected %q", got, want)
		}
	}

	if !strings.HasPrefix(fingerprintUA, "opencode/") {
		t.Fatalf("fingerprintUA = %q, want prefix opencode/", fingerprintUA)
	}

	version := strings.TrimPrefix(fingerprintUA, "opencode/")
	version, _, _ = strings.Cut(version, " ")
	if version == "" {
		t.Fatal("fingerprintUA has an empty opencode version")
	}

	if !strings.HasSuffix(fingerprintUA, suffix) {
		t.Fatalf("fingerprintUA = %q, want suffix %q", fingerprintUA, suffix)
	}
	if got, want := fingerprintUA, "opencode/"+opencodeVersion+suffix; got != want {
		t.Fatalf("fingerprintUA = %q, want %q", got, want)
	}
}

func TestBuildFingerprintUAUsesOpencodeVersion(t *testing.T) {
	originalVersion := opencodeVersion
	t.Cleanup(func() {
		opencodeVersion = originalVersion
	})

	opencodeVersion = "9.9.9"
	const expected = "opencode/9.9.9 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"
	if got := buildFingerprintUA(); got != expected {
		t.Fatalf("buildFingerprintUA() = %q, want %q", got, expected)
	}
}
