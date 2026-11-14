// Command bundle copies a subset of the data directory that fits a size
// budget: the newest sessions first. It exists for hosts that ship data
// inside the deployment (a Vercel Function is limited to 250 MB).
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

func main() {
	src := flag.String("data", "data", "source data directory")
	out := flag.String("out", "", "destination directory (receives MANIFEST and segments/)")
	budgetMB := flag.Int64("max-mb", 200, "total size of segments to include, in MiB")
	flag.Parse()
	if *out == "" {
		fmt.Fprintln(os.Stderr, "bundle: -out is required")
		os.Exit(2)
	}
	man, err := store.LoadManifest(*src)
	if err != nil {
		fatal(err)
	}
	type item struct {
		e     store.Entry
		start time.Time
		name  string
	}
	var items []item
	for _, e := range man.Segments {
		seg, err := segment.Open(filepath.Join(*src, store.SegmentDir, e.File))
		if err != nil {
			fmt.Fprintln(os.Stderr, "bundle: skipping", e.File, err)
			continue
		}
		s := seg.Meta.Session
		items = append(items, item{e, s.Start, fmt.Sprintf("%d %s %s", s.Year, s.MeetingName, s.Name)})
		seg.Close()
	}
	sort.Slice(items, func(i, j int) bool { return items[i].start.After(items[j].start) })

	if err := os.MkdirAll(filepath.Join(*out, store.SegmentDir), 0o755); err != nil {
		fatal(err)
	}
	var used int64
	var picked []store.Entry
	for _, it := range items {
		if used+it.e.Size > *budgetMB<<20 {
			continue // too big for what is left; an older, smaller session may still fit
		}
		if err := copyFile(filepath.Join(*src, store.SegmentDir, it.e.File), filepath.Join(*out, store.SegmentDir, it.e.File)); err != nil {
			fatal(err)
		}
		used += it.e.Size
		picked = append(picked, it.e)
		fmt.Printf("  %s (%.1f MiB)\n", it.name, float64(it.e.Size)/(1<<20))
	}
	if len(picked) == 0 {
		fatal(fmt.Errorf("no sessions fit in %d MiB", *budgetMB))
	}
	// A fresh manifest in the destination: remove any old one so versions start clean.
	os.Remove(filepath.Join(*out, store.ManifestName))
	if err := store.Update(*out, func(m *store.Manifest) error {
		for _, e := range picked {
			m.Put(e)
		}
		return nil
	}); err != nil {
		fatal(err)
	}
	os.Remove(filepath.Join(*out, store.ManifestName+".lock"))
	fmt.Printf("bundled %d of %d sessions, %.1f MiB\n", len(picked), len(items), float64(used)/(1<<20))
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bundle:", err)
	os.Exit(1)
}
