package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv reads a .env file from path if it exists, and sets any environment
// variables that are not already set in the current process. If path is empty,
// it defaults to ".env". A missing file is ignored without error.
func LoadDotEnv(path string) error {
	if strings.TrimSpace(path) == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return parseAndSetDotEnv(string(data))
}

func parseAndSetDotEnv(content string) error {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		if (strings.HasPrefix(val, `"`) && strings.HasSuffix(val, `"`)) ||
			(strings.HasPrefix(val, `'`) && strings.HasSuffix(val, `'`)) {
			if len(val) >= 2 {
				val = val[1 : len(val)-1]
			}
		}
		// Do not overwrite existing non-empty environment variables set in the environment.
		if envVal, exists := os.LookupEnv(key); !exists || envVal == "" {
			_ = os.Setenv(key, val)
		}
	}
	return scanner.Err()
}
