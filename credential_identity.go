package main

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// credentialProfileKey creates a stable, non-secret identity for a host credential.
func credentialProfileKey(provider, baseURL, authIndex string) string {
	if authIndex = strings.TrimSpace(authIndex); authIndex != "" {
		sum := sha256.Sum256([]byte("auth\n" + authIndex))
		return "auth-" + fmt.Sprintf("%x", sum[:6])
	}
	canonical := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if parsed, err := url.Parse(canonical); err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawQuery = ""
		parsed.Fragment = ""
		canonical = strings.TrimRight(parsed.String(), "/")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(provider)) + "\n" + canonical))
	return "site-" + fmt.Sprintf("%x", sum[:6])
}

// configProfileKey creates a stable identity for an API-key entry from config.yaml.
func configProfileKey(provider, baseURL string, index int) string {
	canonical := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if parsed, err := url.Parse(canonical); err == nil {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Path = strings.TrimRight(parsed.Path, "/")
		parsed.RawQuery = ""
		parsed.Fragment = ""
		canonical = strings.TrimRight(parsed.String(), "/")
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(provider)) + "\n" + canonical + "\n" + strconv.Itoa(index)))
	return "site-" + fmt.Sprintf("%x", sum[:6])
}
