package conf

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

func dotenvValue(name string) string {
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
