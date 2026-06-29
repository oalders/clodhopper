package main

import (
	"net"
	"testing"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// Safe: loopback, private, link-local, ULA, unspecified, CGNAT.
		{"127.0.0.1", false},
		{"::1", false},
		{"10.0.0.1", false},
		{"192.168.1.5", false},
		{"172.16.0.1", false},
		{"169.254.1.1", false},
		{"fe80::1", false},
		{"fc00::1", false},
		{"0.0.0.0", false},
		{"::", false},
		{"100.64.0.1", false},
		{"100.127.255.254", false},
		// Public.
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"2606:4700:4700::1111", true},
		{"100.63.255.255", true},
		{"100.128.0.0", true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("ParseIP(%q) returned nil", c.ip)
		}
		if got := isPublicIP(ip); got != c.want {
			t.Errorf("isPublicIP(%q) = %v, want %v", c.ip, got, c.want)
		}
	}
	if isPublicIP(nil) {
		t.Errorf("isPublicIP(nil) = true, want false")
	}
}

func TestGuardPublicBind(t *testing.T) {
	cases := []struct {
		host        string
		allowPublic bool
		wantErr     bool
	}{
		{"127.0.0.1", false, false},
		{"10.0.0.1", false, false},
		{"100.64.0.1", false, false}, // Tailscale CGNAT allowed.
		{"8.8.8.8", false, true},
		{"8.8.8.8", true, false}, // opt-in bypasses the guard.
	}
	for _, c := range cases {
		err := guardPublicBind(c.host, c.allowPublic)
		if (err != nil) != c.wantErr {
			t.Errorf("guardPublicBind(%q, %v) error = %v, wantErr %v",
				c.host, c.allowPublic, err, c.wantErr)
		}
	}
}
