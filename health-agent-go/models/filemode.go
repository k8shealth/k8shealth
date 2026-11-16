package models

import "strings"

// NormalizeFileMode maps every spelling of a file-check mode to the two values the agent and script use:
//
//	"ro" - must be readable and must NOT be writable ("read", "readable", "ro", "", anything unknown)
//	"rw" - must be readable and writable ("write", "writable", "rw", "read-write", ...)
//
// The manager UI/DB stores "read"/"write" while the script reports "ro"/"rw"; comparing the raw strings made a
// UI-defined "read" check (or its Test button) always fail.
func NormalizeFileMode(mode string) string {
	if strings.Contains(strings.ToLower(strings.TrimSpace(mode)), "w") {
		return "rw"
	}
	return "ro"
}

// FileAccessPassed applies the pass rule for a mode to what the script measured.
func FileAccessPassed(mode string, readable, writable bool) bool {
	if NormalizeFileMode(mode) == "rw" {
		return readable && writable
	}
	return readable && !writable
}
