package server

import (
	"os"
	"path/filepath"
)

// osMkdirWrite creates dir (and parents) and writes a file under it.
func osMkdirWrite(dir, name, content string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
}
