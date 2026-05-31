package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScrubString_RedactsSecrets(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		absent  []string
		present []string
	}{
		{"env api key", "MYAPP_ANTHROPIC_API_KEY=sk-ant-supersecret123", []string{"supersecret123"}, []string{"MYAPP_ANTHROPIC_API_KEY", redacted}},
		{"hcloud token", "export HCLOUD_TOKEN=abcdef0123456789", []string{"abcdef0123456789"}, []string{"HCLOUD_TOKEN", redacted}},
		{"single-quoted value", "API_KEY='quotedsecretvalue'", []string{"quotedsecretvalue"}, []string{redacted}},
		{"double-quoted value", `API_KEY="dquotedsecret"`, []string{"dquotedsecret"}, []string{redacted}},
		{"password colon form", "db_password: hunter2hunter2", []string{"hunter2hunter2"}, []string{redacted}},
		{"authorization bearer", "Authorization: Bearer eyJabc.def.ghi", []string{"eyJabc.def.ghi"}, []string{"Authorization", redacted}},
		{"connection string", "postgres://user:supersecretpw@host:5432/db", []string{"supersecretpw"}, []string{"postgres://user:", redacted, "@host"}},
		{"anthropic key bare", "key is sk-ant-api03-ABCDEFGHIJ here", []string{"sk-ant-api03-ABCDEFGHIJ"}, []string{redacted}},
		{"slack token", "tok xoxb-123456789-abcdefABCDEF here", []string{"xoxb-123456789-abcdefABCDEF"}, []string{redacted}},
		{"slack webhook", "post to https://hooks.slack.com/services/T00/B00/XXXXsecret", []string{"XXXXsecret"}, []string{redacted}},
		{"github token", "token ghp_0123456789abcdefABCDEF0123456789zz", []string{"ghp_0123456789abcdefABCDEF0123456789zz"}, []string{redacted}},
		{"aws access key", "key AKIAIOSFODNN7EXAMPLE used", []string{"AKIAIOSFODNN7EXAMPLE"}, []string{redacted}},
		{"harmless text untouched", "git status && go test ./...", nil, []string{"git status && go test ./..."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrubString(tc.in)
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("secret survived: %q still in %q", a, got)
				}
			}
			for _, p := range tc.present {
				if !strings.Contains(got, p) {
					t.Errorf("expected %q to remain in %q", p, got)
				}
			}
		})
	}
}

// mustNotContain fails if any needle appears in s.
func mustNotContain(t *testing.T, s string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(s, n) {
			t.Errorf("leaked %q in: %s", n, s)
		}
	}
}

func TestScrubPayload_DropsSecretsRegardlessOfType(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PreToolUse",
		"tool_name": "Bash",
		"api_key": 1234567890123456,
		"AWS_SECRET_ACCESS_KEY": {"nested": "wJalrXUtnFEMIsupersecret"},
		"real_secret_num": 9876543210,
		"some_token": ["arr-secret-aaa", "arr-secret-bbb"]
	}`)
	out := scrubPayload(raw)
	// All dropped because these keys are not in payloadAllow — the allowlist,
	// not the value's type, is what protects them. (A number under an
	// allowlisted key like duration_ms would be retained verbatim.)
	mustNotContain(t, out,
		"1234567890123456",                 // numeric value under non-allowlisted key
		"wJalrXUtnFEMIsupersecret",         // nested object under non-allowlisted key
		"9876543210",                       // numeric value under non-allowlisted key
		"arr-secret-aaa", "arr-secret-bbb", // array under non-allowlisted key
	)
	// Allowlisted metadata is retained.
	if !strings.Contains(out, "PreToolUse") || !strings.Contains(out, "Bash") {
		t.Errorf("dropped allowlisted metadata: %s", out)
	}
	// Result is valid JSON.
	var v map[string]any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not valid JSON: %v (%s)", err, out)
	}
}

func TestScrubPayload_NeverPersistsChatOrTranscript(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "Stop",
		"transcript_path": "/home/user/.claude/transcripts/secret.jsonl",
		"messages": [{"role": "user", "content": "my whole secret chat history"}],
		"last_assistant_message": "assistant said something private",
		"prompt": "a user prompt that must not be stored verbatim",
		"tool_response": {"stdout": "private file contents here"}
	}`)
	out := scrubPayload(raw)
	mustNotContain(t, out,
		"secret.jsonl",
		"my whole secret chat history",
		"assistant said something private",
		"a user prompt that must not be stored verbatim",
		"private file contents here",
	)
	if !strings.Contains(out, "Stop") {
		t.Errorf("dropped allowlisted event name: %s", out)
	}
}

func TestScrubPayload_ToolInputAllowlistAndScrub(t *testing.T) {
	raw := []byte(`{
		"hook_event_name": "PreToolUse",
		"tool_name": "Edit",
		"tool_input": {
			"file_path": "/repo/main.go",
			"command": "deploy HCLOUD_TOKEN=tok_leak_99",
			"old_string": "secret old content do not store",
			"content": "secret new content do not store"
		}
	}`)
	out := scrubPayload(raw)
	// Allowlisted tool_input fields retained...
	if !strings.Contains(out, "/repo/main.go") {
		t.Errorf("file_path dropped: %s", out)
	}
	// ...but free text within them is scrubbed...
	mustNotContain(t, out, "tok_leak_99")
	// ...and non-allowlisted (content-bearing) fields are dropped entirely.
	mustNotContain(t, out, "secret old content do not store", "secret new content do not store")
}

func TestScrubPayload_UnparseableDropped(t *testing.T) {
	out := scrubPayload([]byte(`not json: API_KEY=leakme123 messages "secret chat"`))
	mustNotContain(t, out, "leakme123", "secret chat")
	if !strings.Contains(out, "_note") {
		t.Errorf("expected omission note, got: %s", out)
	}
}
