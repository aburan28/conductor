package integrations

import "strings"

// Codex keeps its configuration in TOML, and this package does not carry a TOML parser. It
// does not need one: the only edit it ever makes is to own one table, `[mcp_servers.conductor]`,
// from its header to the next header. Everything else in the file is passed through byte for
// byte, comments included — which is the property a real parser would lose.

// tomlTableHeader reports whether a line opens the named table, tolerating quoted keys.
func tomlTableHeader(line, table string) bool {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return false
	}
	t = strings.TrimSuffix(strings.TrimPrefix(t, "["), "]")
	t = strings.ReplaceAll(strings.ReplaceAll(t, "\"", ""), "'", "")
	return strings.TrimSpace(t) == table
}

// tomlIsHeader reports whether a line opens any table.
func tomlIsHeader(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "[")
}

// tomlTableSpan locates the named table: its header line and the index of the line after its
// last body line. start is -1 when absent.
func tomlTableSpan(lines []string, table string) (start, end int) {
	start, end = -1, -1
	for i, line := range lines {
		if start < 0 {
			if tomlTableHeader(line, table) {
				start = i
			}
			continue
		}
		if tomlIsHeader(line) {
			end = i
			break
		}
	}
	if start >= 0 && end < 0 {
		end = len(lines)
	}
	return start, end
}

// setTOMLTable replaces the named table's body (or appends the table) and returns the file.
func setTOMLTable(content, table string, body []string) string {
	lines := strings.Split(content, "\n")
	start, end := tomlTableSpan(lines, table)
	section := append([]string{"[" + table + "]"}, body...)

	if start < 0 {
		trimmed := strings.TrimRight(content, "\n")
		if trimmed == "" {
			return strings.Join(section, "\n") + "\n"
		}
		return trimmed + "\n\n" + strings.Join(section, "\n") + "\n"
	}
	// Keep one blank line before the next table, as the file most likely had.
	tail := lines[end:]
	if len(tail) > 0 && strings.TrimSpace(tail[0]) != "" {
		section = append(section, "")
	}
	out := append(append(append([]string{}, lines[:start]...), section...), tail...)
	result := strings.Join(out, "\n")
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// removeTOMLTable drops the named table, header included.
func removeTOMLTable(content, table string) string {
	lines := strings.Split(content, "\n")
	start, end := tomlTableSpan(lines, table)
	if start < 0 {
		return content
	}
	// Also drop blank lines that separated the removed table from its predecessor.
	for start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	out := append(append([]string{}, lines[:start]...), lines[end:]...)
	result := strings.Join(out, "\n")
	if strings.TrimSpace(result) == "" {
		return ""
	}
	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	return result
}

// hasTOMLTable reports whether the named table is present.
func hasTOMLTable(content, table string) bool {
	start, _ := tomlTableSpan(strings.Split(content, "\n"), table)
	return start >= 0
}

// tomlTableBody returns the body lines of the named table.
func tomlTableBody(content, table string) []string {
	lines := strings.Split(content, "\n")
	start, end := tomlTableSpan(lines, table)
	if start < 0 {
		return nil
	}
	return lines[start+1 : end]
}

// tomlString quotes a value as a TOML basic string.
func tomlString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// tomlStringArray renders a TOML array of strings.
func tomlStringArray(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, s := range items {
		quoted = append(quoted, tomlString(s))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
