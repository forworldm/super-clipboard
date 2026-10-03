package config

import (
	"bufio"
	"os"
	"strings"
)

// ParseDotEnvFile reads a minimal `.env` file so that the Go backend keeps the
// `env_file=".env"` behaviour of pydantic-settings.
//
// Supported syntax: KEY=VALUE, optional `export ` prefix, `#` comments, blank
// lines and single/double quoted values (double quotes honour \n, \t and \\
// escapes). A missing file is not an error.
func ParseDotEnvFile(path string) map[string]string {
	values := make(map[string]string)
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if key == "" {
			continue
		}
		raw := strings.TrimSpace(line[idx+1:])
		if len(raw) >= 2 {
			first, last := raw[0], raw[len(raw)-1]
			if first == '\'' && last == '\'' {
				raw = raw[1 : len(raw)-1]
			} else if first == '"' && last == '"' {
				raw = unescapeDoubleQuoted(raw[1 : len(raw)-1])
			} else if hashIdx := strings.Index(raw, " #"); hashIdx >= 0 {
				raw = strings.TrimSpace(raw[:hashIdx])
			}
		}
		values[key] = raw
	}
	return values
}

func unescapeDoubleQuoted(value string) string {
	replacer := strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)
	return replacer.Replace(value)
}
