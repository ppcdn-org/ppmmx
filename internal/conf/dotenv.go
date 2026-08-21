package conf

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// DotenvValue returns the value of an environment variable, falling back to
// the nearest .env file found by walking up from the working directory.
func DotenvValue(name string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}

	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		file, err := os.Open(filepath.Join(dir, ".env"))
		if err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				key, value, ok := strings.Cut(strings.TrimSpace(scanner.Text()), "=")
				if ok && strings.TrimSpace(key) == name {
					file.Close()
					return strings.Trim(strings.TrimSpace(value), `"'`)
				}
			}
			file.Close()
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// dotenvPath returns the path of the nearest .env file found by walking up
// from the working directory, or the working directory's own .env if none
// exists yet (so callers can create one).
func dotenvPath() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := dir
	for {
		p := filepath.Join(dir, ".env")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Join(start, ".env"), nil
		}
		dir = parent
	}
}

// SetDotenvValue creates or updates name=value in the nearest .env file
// (see dotenvPath), preserving every other line as-is. It does not touch
// the process environment, so a value already set via os.Setenv keeps
// taking priority in DotenvValue until the process restarts. The file's
// existing line ending convention (CRLF or LF - .env files edited on
// Windows commonly have CRLF) is detected and kept for every line,
// including the one being added/replaced, so the result isn't a mix of
// both.
func SetDotenvValue(name, value string) error {
	path, err := dotenvPath()
	if err != nil {
		return err
	}

	eol := "\n"
	var lines []string
	if existing, err := os.ReadFile(path); err == nil {
		if strings.Contains(string(existing), "\r\n") {
			eol = "\r\n"
		}
		normalized := strings.ReplaceAll(string(existing), "\r\n", "\n")
		lines = strings.Split(strings.TrimRight(normalized, "\n"), "\n")
		if len(lines) == 1 && lines[0] == "" {
			lines = nil
		}
	}

	newLine := name + "=" + value
	found := false
	for i, line := range lines {
		key, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(key) == name {
			lines[i] = newLine
			found = true
			break
		}
	}
	if !found {
		lines = append(lines, newLine)
	}

	return os.WriteFile(path, []byte(strings.Join(lines, eol)+eol), 0o600)
}
