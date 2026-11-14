// Command segdump prints a segment's header, lap table and blocks.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/AnanmayS/f1-telemetry-go/internal/encoding"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
)

var encNames = map[byte]string{encoding.Delta: "delta", encoding.DoD: "dod", encoding.RLE: "rle", encoding.BoolRLE: "boolrle"}

func main() {
	driver := flag.Int("driver", -1, "only this driver")
	lap := flag.Int("lap", -1, "print this lap's samples (needs -driver)")
	blocks := flag.Bool("blocks", false, "list every block")
	verify := flag.Bool("verify", false, "decode every block and check CRCs")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: segdump [flags] file.seg")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	s, err := segment.Open(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "segdump:", err)
		os.Exit(1)
	}
	defer s.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()

	if *lap >= 0 {
		ld, err := s.ReadLap(*driver, *lap)
		if err != nil {
			fmt.Fprintln(os.Stderr, "segdump:", err)
			os.Exit(1)
		}
		for c := model.Channel(0); c < model.NumChannels; c++ {
			fmt.Fprintf(w, "%s\t", c)
		}
		fmt.Fprintln(w)
		for i := 0; i < ld.Lap.Samples; i++ {
			for c := model.Channel(0); c < model.NumChannels; c++ {
				if ld.Valid[c] != nil && !ld.Valid[c][i] {
					fmt.Fprint(w, "-\t")
				} else {
					fmt.Fprintf(w, "%d\t", ld.Values[c][i])
				}
			}
			fmt.Fprintln(w)
		}
		return
	}

	meta, _ := json.MarshalIndent(s.Meta.Session, "", "  ")
	fmt.Fprintf(w, "file\t%s (%d bytes)\n", flag.Arg(0), s.Size())
	fmt.Fprintf(w, "format\tGLAP v%d\n", segment.Version)
	fmt.Fprintf(w, "epoch_unix_ms\t%d\n", s.EpochMs)
	fmt.Fprintf(w, "circuit_length_m\t%.2f\n", s.Meta.CircuitLengthM)
	fmt.Fprintf(w, "samples\t%d stored of %d fetched\n", s.Meta.Samples, s.Meta.RawSamples)
	fmt.Fprintf(w, "raw_json_bytes\t%d\n", s.Meta.RawJSONBytes)
	fmt.Fprintf(w, "drivers\t%d\n", len(s.Meta.Drivers))
	fmt.Fprintf(w, "laps\t%d\n", len(s.AllLaps()))
	fmt.Fprintf(w, "blocks\t%d\n", len(s.Blocks()))
	fmt.Fprintf(w, "session\t%s\n\n", meta)

	// Bytes per channel across the whole file.
	var bytes, counts [model.NumChannels]int64
	encs := [model.NumChannels]map[string]int{}
	for _, b := range s.Blocks() {
		bytes[b.Channel] += int64(b.Length)
		counts[b.Channel] += int64(b.Count)
		if encs[b.Channel] == nil {
			encs[b.Channel] = map[string]int{}
		}
		name := encNames[b.Encoding&^encoding.FlagValidity]
		if b.Encoding&encoding.FlagValidity != 0 {
			name += "+bitmap"
		}
		encs[b.Channel][name]++
	}
	fmt.Fprintln(w, "CHANNEL\tBYTES\tBYTES/SAMPLE\tENCODINGS")
	for c := model.Channel(0); c < model.NumChannels; c++ {
		bps := 0.0
		if counts[c] > 0 {
			bps = float64(bytes[c]) / float64(counts[c])
		}
		fmt.Fprintf(w, "%s\t%d\t%.3f\t%v\n", c, bytes[c], bps, encs[c])
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "DRIVER\tLAP\tSTART_MS\tEND_MS\tOFFICIAL_MS\tS1\tS2\tS3\tSAMPLES\tFLAGS")
	for _, l := range s.AllLaps() {
		if *driver >= 0 && l.Driver != *driver {
			continue
		}
		fmt.Fprintf(w, "%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%s\n", l.Driver, l.Number, l.StartMs, l.EndMs, l.OfficialMs,
			l.SectorMs[0], l.SectorMs[1], l.SectorMs[2], l.Samples, flags(l.Flags))
	}
	if *blocks {
		fmt.Fprintln(w, "\nDRIVER\tLAP\tCHANNEL\tOFFSET\tLENGTH\tENC\tCOUNT")
		for _, b := range s.Blocks() {
			if *driver >= 0 && b.Driver != *driver {
				continue
			}
			fmt.Fprintf(w, "%d\t%d\t%s\t%d\t%d\t%#02x\t%d\n", b.Driver, b.Lap, b.Channel, b.Offset, b.Length, b.Encoding, b.Count)
		}
	}
	if *verify {
		w.Flush()
		if err := s.Verify(); err != nil {
			fmt.Fprintln(os.Stderr, "verify FAILED:", err)
			os.Exit(1)
		}
		fmt.Println("\nverify: all blocks OK")
	}
}

func flags(f uint8) string {
	out := ""
	for _, x := range []struct {
		bit  uint8
		name string
	}{{model.FlagPitOut, "pit-out"}, {model.FlagPitIn, "pit-in"}, {model.FlagValidTime, "valid-time"}, {model.FlagDistanceOK, "dist-ok"}} {
		if f&x.bit != 0 {
			if out != "" {
				out += ","
			}
			out += x.name
		}
	}
	if out == "" {
		return "-"
	}
	return out
}
