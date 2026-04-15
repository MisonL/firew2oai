package whitelist

import (
	"net"
	"net/http"
	"testing"
)

func TestNewEmpty(t *testing.T) {
	c, err := New("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !c.IsEmpty() {
		t.Error("expected empty checker")
	}
}

func TestNewSingleIPs(t *testing.T) {
	c, err := New("127.0.0.1, ::1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.IsEmpty() {
		t.Error("expected non-empty checker")
	}
	if len(c.ips) != 2 {
		t.Errorf("expected 2 IPs, got %d", len(c.ips))
	}
}

func TestNewCIDR(t *testing.T) {
	c, err := New("192.168.0.0/16, 10.0.0.0/8")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.nets) != 2 {
		t.Errorf("expected 2 CIDRs, got %d", len(c.nets))
	}
}

func TestNewMixed(t *testing.T) {
	c, err := New("127.0.0.1, 192.168.0.0/16, ::1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.ips) != 2 || len(c.nets) != 1 {
		t.Errorf("expected 2 IPs + 1 CIDR, got %d IPs, %d CIDRs", len(c.ips), len(c.nets))
	}
}

func TestNewInvalidCIDR(t *testing.T) {
	_, err := New("192.168.0.0/33")
	if err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

func TestNewInvalidIP(t *testing.T) {
	_, err := New("not-an-ip")
	if err == nil {
		t.Fatal("expected error for invalid IP")
	}
}

func TestAllowedEmpty(t *testing.T) {
	c, _ := New("")
	if !c.Allowed(net.ParseIP("1.2.3.4")) {
		t.Error("empty checker should allow all")
	}
}

func TestAllowedSingleIP(t *testing.T) {
	c, _ := New("127.0.0.1")
	tests := []struct {
		ip      string
		allowed bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", false},
		{"192.168.1.1", false},
	}
	for _, tt := range tests {
		if c.Allowed(net.ParseIP(tt.ip)) != tt.allowed {
			t.Errorf("IP %s: expected %v", tt.ip, tt.allowed)
		}
	}
}

func TestAllowedCIDR(t *testing.T) {
	c, _ := New("192.168.0.0/16")
	tests := []struct {
		ip      string
		allowed bool
	}{
		{"192.168.0.1", true},
		{"192.168.255.255", true},
		{"192.169.0.1", false},
		{"10.0.0.1", false},
	}
	for _, tt := range tests {
		if c.Allowed(net.ParseIP(tt.ip)) != tt.allowed {
			t.Errorf("IP %s: expected %v", tt.ip, tt.allowed)
		}
	}
}

func TestAllowedIPv6Loopback(t *testing.T) {
	c, _ := New("127.0.0.1, ::1")
	tests := []struct {
		ip      string
		allowed bool
	}{
		{"127.0.0.1", true},
		{"::1", true},
		{"::2", false},
	}
	for _, tt := range tests {
		if c.Allowed(net.ParseIP(tt.ip)) != tt.allowed {
			t.Errorf("IP %s: expected %v", tt.ip, tt.allowed)
		}
	}
}

func TestExtractClientIP_RemoteAddr(t *testing.T) {
	r := &http.Request{RemoteAddr: "192.168.1.100:12345"}
	ip, err := ExtractClientIP(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("192.168.1.100")) {
		t.Errorf("expected 192.168.1.100, got %v", ip)
	}
}

func TestExtractClientIP_XRealIP(t *testing.T) {
	r := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header:     map[string][]string{"X-Real-Ip": {"192.168.1.50"}},
	}
	ip, err := ExtractClientIP(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("192.168.1.50")) {
		t.Errorf("expected 192.168.1.50, got %v", ip)
	}
}

func TestExtractClientIP_XForwardedFor(t *testing.T) {
	r := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header:     map[string][]string{"X-Forwarded-For": {"203.0.113.50, 10.0.0.2"}},
	}
	ip, err := ExtractClientIP(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("203.0.113.50")) {
		t.Errorf("expected 203.0.113.50, got %v", ip)
	}
}

func TestExtractClientIP_XRealIPPriority(t *testing.T) {
	r := &http.Request{
		RemoteAddr: "10.0.0.1:12345",
		Header: map[string][]string{
			"X-Real-Ip":        {"192.168.1.50"},
			"X-Forwarded-For": {"203.0.113.50"},
		},
	}
	ip, err := ExtractClientIP(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("192.168.1.50")) {
		t.Errorf("X-Real-IP should take priority, got %v", ip)
	}
}

func TestExtractClientIP_NoPort(t *testing.T) {
	r := &http.Request{RemoteAddr: "127.0.0.1"}
	ip, err := ExtractClientIP(r)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ip.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("expected 127.0.0.1, got %v", ip)
	}
}
