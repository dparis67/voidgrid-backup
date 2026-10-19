package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// loadPassword is the repository password: read from the file the operator
// names with -password-file, or taken from $VB_RECOVER_PASSWORD. It is in its
// own file because that read is the one place this tool opens a path it was
// handed, which is its job; .golangci.yml excuses G304 for this file alone.
func loadPassword(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("read password file: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	if p, ok := os.LookupEnv("VB_RECOVER_PASSWORD"); ok {
		return p, nil
	}
	return "", errors.New("no repository password: set -password-file or $VB_RECOVER_PASSWORD")
}
