//go:build !unix

package store

// lockFile is a no-op where flock is unavailable: run one writer at a time.
func lockFile(string) (func(), error) { return func() {}, nil }
