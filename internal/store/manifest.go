// Package store manages the set of live segments: the manifest on disk, the
// registry of open segments, the decoded-lap cache and the query entry points.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const (
	ManifestName = "MANIFEST"
	SegmentDir   = "segments"
)

// Entry is one live segment.
type Entry struct {
	SessionKey int    `json:"session_key"`
	Year       int    `json:"year"`
	File       string `json:"file"` // relative to the segments directory
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"` // of the whole file, hex
	Samples    int64  `json:"samples"`
}

// Manifest lists the live segments. Version increases on every write.
type Manifest struct {
	Version  int64   `json:"version"`
	Segments []Entry `json:"segments"`
	// Skipped records sessions that were examined and hold no telemetry, so
	// reruns do not rebuild them. Key: session key, value: reason.
	Skipped map[int]string `json:"skipped,omitempty"`
}

// LoadManifest reads dir/MANIFEST. A missing file is an empty manifest.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return &Manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	return &m, nil
}

// Save writes the manifest atomically: MANIFEST.tmp, fsync, rename, fsync dir.
func (m *Manifest) Save(dir string) error {
	m.Version++
	sort.Slice(m.Segments, func(i, j int) bool { return m.Segments[i].SessionKey < m.Segments[j].SessionKey })
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ManifestName+".tmp")
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, ManifestName)); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// Find returns the entry for a session.
func (m *Manifest) Find(session int) (Entry, bool) {
	for _, e := range m.Segments {
		if e.SessionKey == session {
			return e, true
		}
	}
	return Entry{}, false
}

// Put adds or replaces the entry for e.SessionKey and returns the file name
// it replaced ("" if none or unchanged).
func (m *Manifest) Put(e Entry) (replaced string) {
	delete(m.Skipped, e.SessionKey)
	for i, old := range m.Segments {
		if old.SessionKey == e.SessionKey {
			m.Segments[i] = e
			if old.File != e.File {
				return old.File
			}
			return ""
		}
	}
	m.Segments = append(m.Segments, e)
	return ""
}

// Skip records a session with no usable data.
func (m *Manifest) Skip(session int, reason string) {
	if m.Skipped == nil {
		m.Skipped = map[int]string{}
	}
	m.Skipped[session] = reason
}

// SegmentFileName names a segment by session and content hash, so a rebuilt
// session gets a new file and the old one stays valid for readers that still
// hold it.
//
// The hash is SHA-256, not CRC32: every block is followed by its own CRC32,
// and a CRC32 computed across "data + crc32(data)" is a constant, so a
// whole-file CRC32 cannot see a same-length change inside any block.
func SegmentFileName(session int, data []byte) (name, sum string) {
	h := sha256.Sum256(data)
	sum = hex.EncodeToString(h[:])
	return fmt.Sprintf("%d-%s.seg", session, sum[:12]), sum
}
