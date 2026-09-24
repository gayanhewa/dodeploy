// Package envfile edits KEY=value environment files without disturbing
// anything else in them.
//
// dodeploy writes an app's .env once and never overwrites it, because secrets
// are added there by hand and survive redeploys. A managed integration such as
// Turnstile therefore has to change individual keys in place: comments,
// ordering and every unrelated line must come back out byte for byte, or the
// next integration would quietly rewrite someone's configuration.
package envfile

import (
	"sort"
	"strings"
)

// managedMarker sits above keys this tool appends, so it is obvious where a
// value came from and which line a later run is allowed to rewrite.
const managedMarker = "# Managed by dodeploy. Updated by `dodeploy cloudflare turnstile`."

// Set returns content with every key in kv set to its value.
//
// An existing assignment is replaced where it stands, keeping its position and
// any leading whitespace or "export ". Keys that are absent are appended
// together under a marker comment. The bool reports whether anything changed,
// so a caller can skip restarting a service when nothing did.
func Set(content string, kv map[string]string) (string, bool) {
	if len(kv) == 0 {
		return content, false
	}

	// Work in lines, remembering whether the file ended with a newline so a
	// well-formed file stays well-formed and a changed one gains the newline it
	// needs.
	trailingNewline := strings.HasSuffix(content, "\n")
	lines := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil // an empty file is no lines, not one empty line
	}

	remaining := make(map[string]string, len(kv))
	for k, v := range kv {
		remaining[k] = v
	}

	changed := false
	for i, line := range lines {
		key, prefix, ok := splitAssignment(line)
		if !ok {
			continue
		}
		want, managed := remaining[key]
		if !managed {
			continue
		}
		if replacement := prefix + key + "=" + want; line != replacement {
			lines[i] = replacement
			changed = true
		}
		delete(remaining, key)
	}

	if len(remaining) > 0 {
		// A blank line separates the managed block from whatever preceded it,
		// unless the file was empty.
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, "")
		}
		lines = append(lines, managedMarker)

		keys := make([]string, 0, len(remaining))
		for k := range remaining {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			lines = append(lines, k+"="+remaining[k])
		}
		changed = true
	}

	out := strings.Join(lines, "\n")
	if trailingNewline || changed {
		out += "\n"
	}
	return out, changed
}

// splitAssignment parses a KEY=value line, returning the key and the prefix
// that must be preserved when the line is rewritten (indentation and an
// optional "export ").
//
// Comments, blanks and lines without a name are not assignments.
func splitAssignment(line string) (key, prefix string, ok bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
		return "", "", false
	}

	prefix = line[:len(line)-len(trimmed)]
	rest := trimmed
	if strings.HasPrefix(rest, "export ") {
		prefix += "export "
		rest = strings.TrimPrefix(rest, "export ")
	}

	eq := strings.IndexByte(rest, '=')
	if eq <= 0 {
		return "", "", false
	}
	key = strings.TrimSpace(rest[:eq])
	if key == "" || strings.ContainsAny(key, " \t#") {
		return "", "", false
	}
	return key, prefix, true
}
