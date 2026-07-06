package brain

import (
	"fmt"
	"os"
	"strings"
)

// LoadKey reads a raw Brain key from a key file, trimming whitespace. essaim
// stores only the key-file path in config; the raw key stays in its file.
func LoadKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("essaim: could not read brain key file %q: %w", path, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("essaim: brain key file %q is empty", path)
	}
	return k, nil
}
