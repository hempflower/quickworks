package agent

import (
	"bufio"
	"fmt"
	"strings"
)

// ParseDotenv accepts only literal KEY=VALUE records. It intentionally does
// not support command substitution, export statements, or shell evaluation.
func ParseDotenv(input string) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(input))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || !validEnvironmentKey(key) {
			return nil, fmt.Errorf("invalid dotenv entry on line %d", lineNumber)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read dotenv data: %w", err)
	}
	return values, nil
}

func validEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for index, character := range key {
		if (character >= 'A' && character <= 'Z') || character == '_' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}
