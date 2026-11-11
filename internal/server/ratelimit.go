package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/SeRGei93/b24-mcp/internal/config"
	"github.com/SeRGei93/b24-mcp/internal/mcp"
)

type rateLimitEntry struct {
	count   int
	resetAt time.Time
}

type WindowRateLimiter struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]*rateLimitEntry
}

func NewWindowRateLimiter(maxEntries int) *WindowRateLimiter {
	if maxEntries <= 0 {
		maxEntries = 8192
	}
	return &WindowRateLimiter{
		maxEntries: maxEntries,
		entries:    make(map[string]*rateLimitEntry),
	}
}

func (l *WindowRateLimiter) Consume(key string, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cur, ok := l.entries[key]
	if !ok || !cur.resetAt.After(now) {
		if !ok && len(l.entries) >= l.maxEntries {
			for k, entry := range l.entries {
				if !entry.resetAt.After(now) {
					delete(l.entries, k)
				}
			}
			if len(l.entries) >= l.maxEntries {
				return false
			}
		}
		l.entries[key] = &rateLimitEntry{
			count:   1,
			resetAt: now.Add(window),
		}
		return true
	}
	cur.count++
	return cur.count <= limit
}

func (s *Server) enforceRateLimit(r *http.Request, bucket string, limit int, window time.Duration) error {
	clientIP := resolveClientIP(r, s.cfg.TrustProxy)
	if !s.limiter.Consume(fmt.Sprintf("%s:source:%s", bucket, clientIP), limit, window) {
		return mcp.NewProtocolError("temporarily_unavailable", "Too many OAuth requests", http.StatusTooManyRequests)
	}
	globalLimit := max(limit*25, 100)
	if !s.limiter.Consume(fmt.Sprintf("%s:global", bucket), globalLimit, window) {
		return mcp.NewProtocolError("temporarily_unavailable", "Too many OAuth requests", http.StatusTooManyRequests)
	}
	return nil
}

func resolveClientIP(r *http.Request, trustProxy config.TrustProxyConfig) string {
	remoteIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		remoteIP = host
	}
	if !trustProxy.Enabled {
		return remoteIP
	}
	if len(trustProxy.Addresses) > 0 && !isTrustedProxyAddr(remoteIP, trustProxy.Addresses) {
		return remoteIP
	}
	xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For"))
	if xff == "" {
		return remoteIP
	}
	var parts []string
	for _, p := range strings.Split(xff, ",") {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	if len(parts) == 0 {
		return remoteIP
	}
	if len(trustProxy.Addresses) > 0 {
		for i := len(parts) - 1; i >= 0; i-- {
			if !isTrustedProxyAddr(parts[i], trustProxy.Addresses) {
				return parts[i]
			}
		}
		return parts[0]
	}
	hops := trustProxy.Hops
	if hops <= 0 {
		hops = 1
	}
	idx := len(parts) - hops
	if idx < 0 {
		idx = 0
	}
	return parts[idx]
}

func isTrustedProxyAddr(ipStr string, trusted []string) bool {
	parsedIP := net.ParseIP(ipStr)
	for _, entry := range trusted {
		if entry == ipStr {
			return true
		}
		if parsedIP != nil {
			if _, cidr, err := net.ParseCIDR(entry); err == nil && cidr.Contains(parsedIP) {
				return true
			}
		}
	}
	return false
}
