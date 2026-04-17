// Package whitelist provides IP-based access control with support for
// CIDR ranges and automatic handling of reverse proxy headers.
package whitelist

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// Checker provides IP whitelist validation.
type Checker struct {
	nets  []*net.IPNet
	ipSet map[string]net.IP // fast O(1) lookup by string key
	ips   []net.IP          // preserved for iteration compatibility
}

// New creates a new Checker from a comma-separated list of IPs and CIDR ranges.
// Examples: "127.0.0.1,192.168.0.0/16,10.0.0.0/8"
func New(whitelist string) (*Checker, error) {
	if whitelist == "" {
		return &Checker{}, nil
	}

	c := &Checker{
		nets:  make([]*net.IPNet, 0),
		ips:   make([]net.IP, 0),
		ipSet: make(map[string]net.IP),
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
		c.ipSet[ip.String()] = ip
	}

	return c, nil
}

// IsEmpty returns true if no whitelist entries are configured.
func (c *Checker) IsEmpty() bool {
	return len(c.nets) == 0 && len(c.ipSet) == 0
}

// Allowed reports whether the given IP address is in the whitelist.
func (c *Checker) Allowed(ip net.IP) bool {
	if c.IsEmpty() {
		return true
	}

	// Check single IPs via map (O(1) average)
	if _, ok := c.ipSet[ip.String()]; ok {
		return true
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
// handling X-Forwarded-For and X-Real-IP headers based on trusted proxy count.
//
// Security: When trustedProxyCount is 0 (default), only RemoteAddr is used.
// When trustedProxyCount == 1, X-Real-IP is trusted directly (single reverse proxy).
// When trustedProxyCount > 1, only X-Forwarded-For is used with precise rightward
// hop counting to prevent IP spoofing via multi-layer proxy setups.
// X-Real-IP is NOT trusted when trustedProxyCount > 1 because multi-layer
// proxies may preserve and forward the original X-Real-IP from the attacker.
func ExtractClientIP(r *http.Request, trustedProxyCount int) (net.IP, error) {
	if trustedProxyCount <= 0 {
		// Trust no proxy headers — use only RemoteAddr
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("unable to parse client IP from %q", r.RemoteAddr)
		}
		return ip, nil
	}

	// Trust X-Real-IP only for single-layer reverse proxy (trustedProxyCount == 1).
	// For multi-layer setups, an attacker could set X-Real-IP and a naive proxy
	// might forward it, bypassing the whitelist. Use X-Forwarded-For instead.
	if trustedProxyCount == 1 {
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			ip := net.ParseIP(xri)
			if ip != nil {
				return ip, nil
			}
		}
	}

	// Trust X-Forwarded-For: the client IP is at position (len - 1 - trustedProxyCount)
	// XFF format: client, proxy1, proxy2 (left=client, right=last proxy)
	// Skip the rightmost trustedProxyCount entries (they are our trusted proxies).
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Client IP index: skip rightmost trustedProxyCount entries
		clientIdx := len(parts) - 1 - trustedProxyCount
		if clientIdx < 0 {
			// Not enough hops in XFF to satisfy trustedProxyCount — the header
			// is unreliable (client may have injected a fake XFF value).
			// Fall back to RemoteAddr instead of trusting potentially spoofed values.
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			if ip != nil {
				return ip, nil
			}
			return nil, fmt.Errorf("unable to parse client IP from %q", r.RemoteAddr)
		}
		// Walk from clientIdx leftward to find a valid IP
		for i := clientIdx; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip != nil {
				return ip, nil
			}
		}
	}

	// Fallback to RemoteAddr
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("unable to parse client IP from %q", r.RemoteAddr)
	}
	return ip, nil
}

// Middleware returns an HTTP middleware that enforces IP whitelist.
// trustedProxyCount controls how many proxy hops to trust for X-Forwarded-For.
// Returns 403 Forbidden if the client IP is not in the whitelist.
func (c *Checker) Middleware(trustedProxyCount int) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c.IsEmpty() {
				next(w, r)
				return
			}

			ip, err := ExtractClientIP(r, trustedProxyCount)
			if err != nil {
				http.Error(w, `{"error":{"message":"unable to determine client IP","type":"authentication_error","code":"ip_unavailable"}}`, http.StatusForbidden)
				return
			}

			if !c.Allowed(ip) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusForbidden)
				resp := map[string]interface{}{
					"error": map[string]string{
						"message": fmt.Sprintf("IP %s is not in whitelist", ip),
						"type":    "authentication_error",
						"code":    "ip_not_allowed",
					},
				}
				data, _ := json.Marshal(resp)
				w.Write(data)
				w.Write([]byte("\n"))
				return
			}

			next(w, r)
		}
	}
}
