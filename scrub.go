package main

import (
	"encoding/json"
	"regexp"
)

const redacted = "«redacted»"

// promptPreviewLen caps how much of a user prompt is ever stored.
const promptPreviewLen = 80

// maxFieldLen caps any retained free-text payload field.
const maxFieldLen = 300

// payloadAllow is the set of top-level hook-payload keys we are willing to
// persist. Everything else — notably transcript/chat content (messages,
// content, tool_response, last_assistant_message, transcript_path) — is dropped.
// An allowlist is the only design that can honour "never persist chat content".
var payloadAllow = map[string]bool{
	"hook_event_name":   true,
	"event_type":        true,
	"session_id":        true,
	"tool_use_id":       true,
	"tool_name":         true,
	"agent_id":          true,
	"agent_type":        true,
	"permission_mode":   true,
	"cwd":               true,
	"source":            true,
	"trigger":           true,
	"notification_type": true,
	"matcher":           true,
	"stop_hook_active":  true,
	"duration_ms":       true,
	"tool_input":        true, // recursed into via toolInputAllow
}

// toolInputAllow is the set of tool_input fields we persist. Fields that can
// carry large or sensitive content (content, old_string, new_string, …) are
// intentionally excluded.
var toolInputAllow = map[string]bool{
	"command":       true,
	"file_path":     true,
	"notebook_path": true,
	"path":          true,
	"pattern":       true,
	"glob":          true,
	"url":           true,
	"description":   true,
	"skill":         true, // names the running /command (fix-gh-issue, code-review, …)
	"limit":         true,
	"offset":        true,
	"timeout":       true,
}

// Secret-shaped patterns applied to any free-text we retain (summaries and
// allowlisted free-text fields). Each keeps enough context to stay readable.
var scrubPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// NAME=value / NAME: value where NAME looks like a credential. Handles
	// single- and double-quoted values. This fails closed: prose like
	// "the secret: foo" may over-redact the next word, but it never leaks. We
	// prefer that to tightening the pattern and risking under-redaction of real
	// config (e.g. "password: hunter2").
	{
		regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|PASSWD|API[_-]?KEY|APIKEY|[_-]KEY)[A-Z0-9_]*)(\s*[=:]\s*)(['"]?)([^\s'"]+)(['"]?)`),
		`${1}${2}${3}` + redacted + `${5}`,
	},
	// Authorization: Bearer/Basic <token>
	{
		regexp.MustCompile(`(?i)(authorization\s*:\s*(?:bearer|basic)\s+)(\S+)`),
		`${1}` + redacted,
	},
	// scheme://user:password@host — redact the password segment.
	{
		regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^:/\s@]+:)([^@/\s]+)(@)`),
		`${1}` + redacted + `${3}`,
	},
	// Anthropic / OpenAI-style provider keys.
	{
		regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_\-]{8,}|sk-[A-Za-z0-9]{16,})\b`),
		redacted,
	},
	// Slack tokens.
	{
		regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{8,}\b`),
		redacted,
	},
	// Slack / Discord webhook URLs.
	{
		regexp.MustCompile(`https://hooks\.slack\.com/services/\S+`),
		redacted,
	},
	{
		regexp.MustCompile(`https://discord(?:app)?\.com/api/webhooks/\S+`),
		redacted,
	},
	// AWS access key IDs.
	{
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		redacted,
	},
	// GitHub-style tokens.
	{
		regexp.MustCompile(`\b(gh[posu]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
		redacted,
	},
	// PEM private key blocks.
	{
		regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
		redacted,
	},
}

// scrubString redacts secret-shaped substrings from s.
func scrubString(s string) string {
	for _, p := range scrubPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}

// scrubPayload reduces a raw hook payload to an allowlisted, scrubbed JSON
// object safe to persist. Unparseable input is dropped entirely rather than
// risk a regex-only leak.
func scrubPayload(raw []byte) string {
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return `{"_note":"payload omitted (unparseable)"}`
	}
	out := make(map[string]any, len(p))
	for k, v := range p {
		if !payloadAllow[k] {
			continue
		}
		if k == "tool_input" {
			if ti, ok := v.(map[string]any); ok {
				out[k] = filterToolInput(ti)
			}
			continue
		}
		if sv, keep := sanitizeScalar(v); keep {
			out[k] = sv
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return `{"_note":"payload omitted (marshal error)"}`
	}
	return string(b)
}

// filterToolInput keeps only allowlisted tool_input fields, scrubbing strings.
func filterToolInput(ti map[string]any) map[string]any {
	out := make(map[string]any)
	for k, v := range ti {
		if !toolInputAllow[k] {
			continue
		}
		if sv, keep := sanitizeScalar(v); keep {
			out[k] = sv
		}
	}
	return out
}

// sanitizeScalar returns a persistable form of a value: scrubbed+truncated for
// strings, as-is for numbers/bools, and dropped for objects/arrays (keep=false).
func sanitizeScalar(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		return truncate(scrubString(t), maxFieldLen), true
	case float64, bool, nil:
		return t, true
	default:
		return nil, false
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
