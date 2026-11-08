package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// claudeNameRe is the dashboard copy handler's pathAllow, whole string, so the page never shows a name it would refuse to copy.
var claudeNameRe = regexp.MustCompile(`^[-\p{L}\p{M}\p{N} /._+=,:@~%]+$`)

const (
	claudeNameMaxRunes   = 64
	claudeSessionFileMax = 64 << 10
)

// newClaudeNameCache memoises lookupClaudeNames per sessions dir for paneTTL, since a name only matters while its session is live.
func newClaudeNameCache() *cwdCache[map[string]string] {
	return &cwdCache[map[string]string]{m: map[string]cwdEntry[map[string]string]{}, lookup: lookupClaudeNames, ttl: paneTTL}
}

// lookupClaudeNames maps session id to the peer name in each live Claude Code
// process's dir/<pid>.json. The format is undocumented, so this is strictly
// best-effort: anything unreadable, malformed or invalid is skipped.
func lookupClaudeNames(dir string) map[string]string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := map[string]string{}
	newest := map[string]time.Time{}
	for _, e := range entries {
		// Type() comes from the dirent, so a symlink is skipped without being followed.
		if !strings.HasSuffix(e.Name(), ".json") || !e.Type().IsRegular() {
			continue
		}
		id, name, mtime, ok := readClaudeSession(filepath.Join(dir, e.Name()))
		// A hard-killed pid's leftover file can share a resumed session's id; the newest wins, an invalid name included.
		if !ok || (!newest[id].IsZero() && !mtime.After(newest[id])) {
			continue
		}
		newest[id], names[id] = mtime, name
	}
	for id, name := range names {
		if name == "" {
			delete(names, id)
		}
	}
	return names
}

// readClaudeSession decodes one sessions file's id and validated name ("" if invalid).
func readClaudeSession(path string) (id, name string, mtime time.Time, ok bool) {
	// O_NONBLOCK so a FIFO swapped in after ReadDir cannot hang the render; Stat then refuses it.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", "", time.Time{}, false
	}
	var s struct {
		SessionID string `json:"sessionId"`
		Name      string `json:"name"`
	}
	if json.NewDecoder(io.LimitReader(f, claudeSessionFileMax)).Decode(&s) != nil || s.SessionID == "" {
		return "", "", time.Time{}, false
	}
	return s.SessionID, validClaudeName(s.Name), st.ModTime(), true
}

// validClaudeName returns s, or "" when it is too long, secret-shaped, padded
// (so a blank name cannot render as an empty button) or outside claudeNameRe:
// refused, never repaired, so a copied name is the real one.
func validClaudeName(s string) string {
	if utf8.RuneCountInString(s) > claudeNameMaxRunes || strings.TrimSpace(s) != s || scrubString(s) != s || !claudeNameRe.MatchString(s) {
		return ""
	}
	return s
}
