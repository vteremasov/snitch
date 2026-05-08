package transcript

import (
	"path/filepath"
	"strings"

	"snitch/internal/state"
)

// EncodeCWD mirrors the encoding Claude Code uses for the projects dir:
// "/Users/foo/bar" -> "-Users-foo-bar". Forward slashes become dashes.
func EncodeCWD(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

// File returns the expected jsonl transcript path for a given session.
func File(cwd, sessionID string) string {
	return filepath.Join(state.ClaudeProjectsDir(), EncodeCWD(cwd), sessionID+".jsonl")
}
