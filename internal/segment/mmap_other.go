//go:build !unix

package segment

import "os"

// mapFile falls back to reading the whole file on platforms without mmap.
func mapFile(path string) ([]byte, func() error, error) {
	data, err := os.ReadFile(path)
	return data, func() error { return nil }, err
}
