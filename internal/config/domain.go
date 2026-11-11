package config

import (
	"net"
	"net/url"
	"regexp"
	"strings"
)

var hostedBitrix24Pattern = regexp.MustCompile(`(?i)^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+bitrix24\.(?:com|ru|de|eu|ua|by|kz|pl|fr|es|it|br|in|cn|vn|tr|jp|uk|mx|co|id|uz|az|am|ge|kg|lv|lt|ee|com\.br|co\.uk|com\.tr|com\.mx|com\.ua)$`)

func IsLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	default:
		return false
	}
}

func ParsePortalHost(input string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	if trimmed == "" {
		return "", NewAppError("Invalid Bitrix24 portal domain", "INVALID_PORTAL_DOMAIN", 400)
	}
	rawURL := trimmed
	if !strings.Contains(trimmed, "://") {
		rawURL = "https://" + trimmed
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "", NewAppError("Invalid Bitrix24 portal domain", "INVALID_PORTAL_DOMAIN", 400)
	}
	if parsed.Scheme != "https" ||
		parsed.User != nil ||
		parsed.Port() != "" ||
		(parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", NewAppError("Portal must be an HTTPS Bitrix24 domain without a path, port, query, or credentials", "INVALID_PORTAL_DOMAIN", 400)
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if net.ParseIP(host) != nil || host == "localhost" {
		return "", NewAppError("IP addresses and localhost are not valid Bitrix24 portal domains", "INVALID_PORTAL_DOMAIN", 400)
	}
	return host, nil
}

func NormalizePortalDomain(input string, allowedPortalDomains []string) (string, error) {
	host, err := ParsePortalHost(input)
	if err != nil {
		return "", err
	}
	var nonEmptyAllowed []string
	for _, allowed := range allowedPortalDomains {
		if strings.TrimSpace(allowed) != "" {
			nonEmptyAllowed = append(nonEmptyAllowed, allowed)
		}
	}
	if len(nonEmptyAllowed) > 0 {
		for _, allowed := range nonEmptyAllowed {
			if allowedHost, err := ParsePortalHost(allowed); err == nil && allowedHost == host {
				return host, nil
			}
		}
		return "", NewAppError("Only hosted or explicitly configured Bitrix24 portal domains are allowed", "INVALID_PORTAL_DOMAIN", 400)
	}
	if !hostedBitrix24Pattern.MatchString(host) {
		return "", NewAppError("Only hosted or explicitly configured Bitrix24 portal domains are allowed", "INVALID_PORTAL_DOMAIN", 400)
	}
	return host, nil
}
