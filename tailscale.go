package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// tailscaleLookup resolves this machine's Tailscale IPv4 for the --tailscale
// flag. It is a package var so tests can stub the shell-out.
var tailscaleLookup = tailscaleIP

// tailscaleIP returns this machine's Tailscale IPv4 address via `tailscale ip
// -4`. Unlike the capture path's best-effort git/CI lookups, a failure here is
// FATAL by design: the user asked to bind the dashboard to the Tailscale
// interface, so a missing CLI, a node that is down, or a timeout must surface
// loudly rather than silently fall back to a different (possibly public) bind.
func tailscaleIP() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").Output()
	if err != nil {
		return "", fmt.Errorf("`tailscale ip -4` failed (is Tailscale installed and up?): %w", err)
	}
	return parseTailscaleIP(out)
}

// parseTailscaleIP extracts the IPv4 address from `tailscale ip -4` output. Split
// from tailscaleIP so the parsing is unit-testable without a real tailscale
// binary. The command prints a single IPv4 line; if it ever emits more, the
// first is taken.
func parseTailscaleIP(out []byte) (string, error) {
	s := strings.TrimSpace(string(out))
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return "", fmt.Errorf("`tailscale ip -4` returned no address (is Tailscale up?)")
	}
	return s, nil
}
