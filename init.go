package main

import "fmt"

// guardPrefix returns the shell guard that precedes the clodhopper invocation
// for the given --guard kind. "command" is the portable POSIX form (the
// default, safe in committed files); "is" uses the personal `is there` helper.
func guardPrefix(kind string) (string, error) {
	switch kind {
	case "command":
		return "command -v clodhopper >/dev/null 2>&1", nil
	case "is":
		return "is there clodhopper", nil
	default:
		return "", fmt.Errorf(`invalid --guard %q (want "command" or "is")`, kind)
	}
}

// ingestCommand builds the full hook command string from a guard prefix and a
// source-app label.
func ingestCommand(guard, sourceApp string) string {
	return guard + " && clodhopper ingest --source-app " + sourceApp + " 2>/dev/null || true"
}
