package main

import "testing"

// A realistic command string used across merge/orchestration tests. It MUST
// contain "clodhopper ingest" so the idempotency check recognizes it.
const testCmd = "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app x 2>/dev/null || true"

func TestGuardPrefix(t *testing.T) {
	cmd, err := guardPrefix("command")
	if err != nil || cmd != "command -v clodhopper >/dev/null 2>&1" {
		t.Fatalf("command: got %q, err %v", cmd, err)
	}
	is, err := guardPrefix("is")
	if err != nil || is != "is there clodhopper" {
		t.Fatalf("is: got %q, err %v", is, err)
	}
	if _, err := guardPrefix("nope"); err == nil {
		t.Error("invalid guard: want error, got nil")
	}
}

func TestIngestCommand(t *testing.T) {
	got := ingestCommand("command -v clodhopper >/dev/null 2>&1", "mmir")
	want := "command -v clodhopper >/dev/null 2>&1 && clodhopper ingest --source-app mmir 2>/dev/null || true"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
