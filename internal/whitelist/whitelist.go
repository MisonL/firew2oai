// Package whitelist provides IP-based access control with support for
// CIDR ranges and automatic handling of reverse proxy headers.
package whitelist

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Checker provides IP whitelist validation.
type Checker struct {
	nets []*net.IPNet
	ips  []net.IP
}

// New creates a new Checker from a comma-separated list of IPs and CIDR ranges.
// Examples: "127.0.0.1,192.168.0.0/16,10.0.0.0/8"
func New(whitelist string) (*Checker, error) {
	if whitelist == "" {
		return &Checker{}, nil
	}

	c := &Checker{
		nets: make([]*net.IPNet, 0),
		ips:  make([]net.IP, 0),
	}

	for _, s := range strings.Split(whitelist, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		// Try CIDR first
		if strings.Contains(s, "/") {
			_, ipnet, err := net.ParseCIDR(s)
			if err != nil {
				return nil, fmt.Errorf("invalid CIDR %q: %w", s, err)
			}
			c.nets = append(c.nets, ipnet)
			continue
		}

		// Single IP
		ip := net.ParseIP(s)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP %q", s)
		}
		c.ips = append(c.ips, ip)
	}

	return c, nil
}

// IsEmpty returns true if no whitelist entries are configured.
func (c *Checker) IsEmpty() bool {
	return len(c.nets) == 0 && len(c.ips) == 0
}

// Allowed reports whether the given IP address is in the whitelist.
func (c *Checker) Allowed(ip net.IP) bool {
	if c.IsEmpty() {
		return true
	}

	// Check single IPs
	for _, allowed := range c.ips {
		if allowed.Equal(ip) {
			return true
		}
	}

	// Check CIDR ranges
	for _, ipnet := range c.nets {
		if ipnet.Contains(ip) {
			return true
		}
	}

	return false
}

// ExtractClientIP extracts the client IP from an HTTP request,
// handling X-Forwarded-For and X-Real-IP headers safely.
func ExtractClientIP(r *http.Request) (net.IP, error) {
	// Trust X-Real-IP first (single value, set by reverse proxy)
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		ip := net.ParseIP(xri)
		if ip != nil {
			return ip, nil
		}
	}

	// Fallback to X-Forwarded-For (take the first, leftmost value)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For: client, proxy1, proxy2
		parts := strings.Split(xff, ",")
		for _, part := range parts {
			ip := net.ParseIP(strings.TrimSpace(part))
			if ip != nil {
				return ip, nil
			}
		}
	}

	// Fallback to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr might not have port in some test scenarios
		host = r.RemoteAddr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("unable to parse client IP from %q", r.RemoteAddr)
	}
	return ip, nil
}

// Middleware returns an HTTP middleware that enforces IP whitelist.
// Returns 403 Forbidden if the client IP is not in the whitelist.
func (c *Checker) Middleware() func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c.IsEmpty() {
				next(w, r)
				return
			}

			ip, err := ExtractClientIP(r)
			if err != nil {
				http.Error(w, `{"error":{"message":"unable to determine client IP","type":"authentication_error","code":"ip_unavailable"}}`, http.StatusForbidden)
				return
			}

			if !c.Allowed(ip) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(fmt.Sprintf(`{"error":{"message":"IP %s is not in whitelist","type":"authentication_error","code":"ip_not_allowed"}}`, ip)))
				return
			}

			next(w, r)
		}
	}
}
