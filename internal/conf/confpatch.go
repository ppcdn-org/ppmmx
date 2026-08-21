package conf

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var ingestSourcesLineRe = regexp.MustCompile(`(?m)^ingestSources:\s*(.*)$`)

// ReadIngestSources extracts the current value of the ingestSources line
// from the YAML file at path, without a full config load/validate pass.
// Returns an empty slice if the key isn't present.
func ReadIngestSources(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := ingestSourcesLineRe.FindStringSubmatch(string(data))
	if m == nil {
		return []string{}, nil
	}
	// Written by PatchIngestSources as a JSON-compatible array of quoted
	// strings (e.g. ["a", "b"]), so a plain JSON decode round-trips it.
	var sources []string
	if err := json.Unmarshal([]byte(strings.TrimSpace(m[1])), &sources); err != nil {
		return nil, fmt.Errorf("parse ingestSources line: %w", err)
	}
	return sources, nil
}

// PatchIngestSources rewrites the ingestSources line of the YAML file at
// path in place, preserving every other line (including comments) exactly
// as-is - a full YAML marshal/unmarshal round-trip would strip the file's
// extensive inline documentation (see conf/record.yml). If the key isn't
// already present as a top-level, unindented line, it's inserted right
// before the "paths:" section (or appended at EOF if there's none).
func PatchIngestSources(path string, sources []string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	content := string(data)

	quoted := make([]string, len(sources))
	for i, s := range sources {
		quoted[i] = strconv.Quote(s)
	}
	line := "ingestSources: [" + strings.Join(quoted, ", ") + "]"

	if ingestSourcesLineRe.MatchString(content) {
		content = ingestSourcesLineRe.ReplaceAllLiteralString(content, line)
	} else if idx := strings.Index(content, "\npaths:"); idx != -1 {
		content = content[:idx+1] + line + "\n" + content[idx+1:]
	} else {
		content = strings.TrimRight(content, "\n") + "\n" + line + "\n"
	}

	return os.WriteFile(path, []byte(content), 0o644)
}
