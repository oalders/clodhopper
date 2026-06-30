package main

import (
	"errors"
	"testing"
)

func TestParseTailscaleIP(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{"single", "100.64.0.1\n", "100.64.0.1", false},
		{"no trailing newline", "100.64.0.1", "100.64.0.1", false},
		{"surrounding whitespace", "  100.64.0.1  \n", "100.64.0.1", false},
		// `tailscale ip -4` prints one IPv4; if it ever emits more, take the first.
		{"multiple lines", "100.64.0.1\n100.64.0.2\n", "100.64.0.1", false},
		{"empty", "", "", true},
		{"whitespace only", "  \n", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseTailscaleIP([]byte(c.out))
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseTailscaleIP(%q) = %q, want error", c.out, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTailscaleIP(%q) unexpected error: %v", c.out, err)
			}
			if got != c.want {
				t.Errorf("parseTailscaleIP(%q) = %q, want %q", c.out, got, c.want)
			}
		})
	}
}

// stubTailscaleLookup swaps the --tailscale resolver for the duration of a test
// so the serve path can be exercised without a real tailscale binary.
func stubTailscaleLookup(t *testing.T, fn func() (string, error)) {
	t.Helper()
	prev := tailscaleLookup
	tailscaleLookup = fn
	t.Cleanup(func() { tailscaleLookup = prev })
}

func TestRunServeTailscaleConflictsWithHost(t *testing.T) {
	stubTailscaleLookup(t, func() (string, error) {
		t.Fatal("lookup should not run when --host conflicts with --tailscale")
		return "", nil
	})
	if got := runServe([]string{"--tailscale", "--host", "1.2.3.4"}); got != 2 {
		t.Errorf("runServe(--tailscale --host) = %d, want 2", got)
	}
}

func TestRunServeTailscaleLookupFailureIsFatal(t *testing.T) {
	stubTailscaleLookup(t, func() (string, error) {
		return "", errors.New("tailscale not up")
	})
	if got := runServe([]string{"--tailscale"}); got != 1 {
		t.Errorf("runServe(--tailscale) with lookup failure = %d, want 1", got)
	}
}
