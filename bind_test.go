package main

import (
	"errors"
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
		// IPv4-mapped IPv6 is classified by its embedded v4 address.
		{"::ffff:8.8.8.8", true},
		{"::ffff:10.0.0.1", false},
		{"::ffff:100.64.0.1", false},
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

// TestGuardPublicBindWith exercises the injected-resolver paths deterministically,
// with no real network — in particular the fail-closed invariant when resolution
// errors.
func TestGuardPublicBindWith(t *testing.T) {
	boom := func(string) ([]net.IP, error) { return nil, errors.New("boom") }
	public := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("8.8.8.8")}, nil }
	private := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.0.0.1")}, nil }

	cases := []struct {
		name        string
		resolve     func(string) ([]net.IP, error)
		allowPublic bool
		wantErr     bool
	}{
		{"resolver error fails closed", boom, false, true},
		{"public IP refused", public, false, true},
		{"allow-public bypasses guard", public, true, false},
		{"private IP allowed", private, false, false},
	}
	for _, c := range cases {
		err := guardPublicBindWith("host", c.allowPublic, c.resolve)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: guardPublicBindWith error = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}
