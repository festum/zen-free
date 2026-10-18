package main

import (
	"fmt"
	"net/url"
	"strings"
	"unicode"
)

const relayTarget = "https://opencode.ai"

type relayConfig struct {
	endpoint *url.URL
	err      error
}

func parseRelayURL(raw string) relayConfig {
	if raw == "" {
		return relayConfig{}
	}
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 || strings.TrimSpace(raw) != raw || strings.Contains(raw, "\\") {
		return relayConfig{err: fmt.Errorf("ZEN_RELAY_URL contains invalid characters")}
	}
	endpoint, err := url.Parse(raw)
	if err != nil {
		return relayConfig{err: fmt.Errorf("invalid ZEN_RELAY_URL")}
	}
	if endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return relayConfig{err: fmt.Errorf("ZEN_RELAY_URL must be an HTTPS URL with a host and optional path, without userinfo, query, or fragment")}
	}
	return relayConfig{endpoint: endpoint}
}

func (c relayConfig) enabled() bool {
	return c.endpoint != nil
}

func (c relayConfig) requestURL(upstreamTarget string) (*url.URL, string, error) {
	upstream, err := url.Parse(upstreamTarget)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		return nil, "", fmt.Errorf("invalid upstream URL")
	}
	path := upstream.EscapedPath()
	if path == "" {
		path = "/"
	}
	if upstream.RawQuery != "" {
		path += "?" + upstream.RawQuery
	}
	if strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return nil, "", fmt.Errorf("invalid upstream request path")
	}
	relayRequest := *c.endpoint
	relayRequest.Path = c.endpoint.Path
	relayRequest.RawPath = c.endpoint.RawPath
	return &relayRequest, path, nil
}
