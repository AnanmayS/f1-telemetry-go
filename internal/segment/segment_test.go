package segment

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/model"
)

func testMeta() Meta {
	return Meta{
		Session: model.Session{Key: 9492, Year: 2024, MeetingName: "Japanese Grand Prix", Name: "Qualifying",
			Start: time.Date(2024, 4, 6, 6, 0, 0, 0, time.UTC)},
		Drivers:        []model.Driver{{Number: 1, Acronym: "VER"}, {Number: 4, Acronym: "NOR"}},
		CircuitLengthM: 5807,
	}
}

func testLap(rng *rand.Rand, driver, number, n int) *model.LapData {
	ld := &model.LapData{Lap: model.Lap{
		Driver: driver, Number: number, Flags: model.FlagValidTime,
		StartMs: int64(number) * 90_000, EndMs: int64(number+1) * 90_000,
		OfficialMs: 88_197, SectorMs: [3]int32{30_777, 39_850, model.NoTime}, Samples: n,
	}}
	t, speed := int64(number)*90_000, int64(200)
	for c := range ld.Values {
		ld.Values[c] = make([]int64, n)
	}
	ld.Valid[model.ChThrottle] = make([]bool, n)
	for i := 0; i < n; i++ {
		t += 200 + int64(rng.Intn(80))
		speed += int64(rng.Intn(11) - 5)
		ld.Values[model.ChT][i] = t
		ld.Values[model.ChSpeed][i] = speed
		ld.Values[model.ChRPM][i] = 9000 + int64(rng.Intn(3000))
		ld.Values[model.ChGear][i] = int64(i / 40 % 9)
		ld.Values[model.ChThrottle][i] = int64(rng.Intn(101))
		ld.Valid[model.ChThrottle][i] = i%7 != 0
		ld.Values[model.ChBrake][i] = int64(i / 25 % 2)
		ld.Values[model.ChDRS][i] = 8
		ld.Values[model.ChX][i] = int64(rng.Intn(20000) - 10000)
		ld.Values[model.ChY][i] = int64(rng.Intn(20000) - 10000)
		ld.Values[model.ChZ][i] = int64(rng.Intn(900))
		ld.Values[model.ChDistance][i] = int64(i) * 1500
	}
	return ld
}

func buildTest(t testing.TB) ([]byte, []*model.LapData) {
	rng := rand.New(rand.NewSource(7))
	var laps []*model.LapData
	for _, d := range []int{4, 1} { // deliberately unsorted
		for n := 3; n >= 1; n-- {
			laps = append(laps, testLap(rng, d, n, 300+n))
		}
	}
	laps = append(laps, testLap(rng, 81, 1, 0)) // empty lap
	data, err := Build(testMeta(), laps)
	if err != nil {
		t.Fatal(err)
	}
	return data, laps
}

func TestRoundTrip(t *testing.T) {
	data, laps := buildTest(t)
	path := filepath.Join(t.TempDir(), "9492.seg")
	if err := WriteFile(path, data); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.SessionKey != 9492 || s.EpochMs != testMeta().Session.Start.UnixMilli() || s.Meta.CircuitLengthM != 5807 || len(s.Meta.Drivers) != 2 {
		t.Fatalf("header mismatch: %+v", s.Meta)
	}
	if len(s.AllLaps()) != len(laps) || len(s.Laps(1)) != 3 || len(s.Laps(99)) != 0 {
		t.Fatalf("lap table: all=%d driver1=%d", len(s.AllLaps()), len(s.Laps(1)))
	}
	for _, want := range laps {
		got, err := s.ReadLap(want.Lap.Driver, want.Lap.Number)
		if err != nil {
			t.Fatal(err)
		}
		if got.Lap != want.Lap {
			t.Fatalf("lap row: got %+v want %+v", got.Lap, want.Lap)
		}
		for c := model.Channel(0); c < model.NumChannels; c++ {
			for i, v := range want.Values[c] {
				present := want.Valid[c] == nil || want.Valid[c][i]
				gotPresent := got.Valid[c] == nil || got.Valid[c][i]
				if present != gotPresent || (present && got.Values[c][i] != v) {
					t.Fatalf("driver %d lap %d %s[%d] mismatch", want.Lap.Driver, want.Lap.Number, c, i)
				}
			}
		}
	}
	if _, _, err := s.ReadChannel(1, 99, model.ChSpeed); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing lap: %v", err)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, _ := buildTest(t)
	b, _ := buildTest(t)
	if !slices.Equal(a, b) {
		t.Fatal("same input produced different bytes")
	}
}

func TestBuildRejectsBadInput(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	dup := []*model.LapData{testLap(rng, 1, 1, 5), testLap(rng, 1, 1, 5)}
	if _, err := Build(testMeta(), dup); err == nil {
		t.Error("duplicate lap accepted")
	}
	ragged := testLap(rng, 1, 1, 5)
	ragged.Values[model.ChSpeed] = ragged.Values[model.ChSpeed][:3]
	if _, err := Build(testMeta(), []*model.LapData{ragged}); err == nil {
		t.Error("ragged channels accepted")
	}
	if _, err := Build(testMeta(), []*model.LapData{testLap(rng, 300, 1, 5)}); err == nil {
		t.Error("out-of-range driver accepted")
	}
}

// A flipped byte inside one block must fail only that block.
func TestCorruptBlockIsIsolated(t *testing.T) {
	data, _ := buildTest(t)
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	var target BlockInfo
	for _, b := range s.Blocks() {
		if b.Driver == 1 && b.Lap == 2 && b.Channel == model.ChSpeed {
			target = b
		}
	}
	bad := slices.Clone(data)
	bad[target.Offset+uint64(target.Length)/2] ^= 0x40
	s, err = Parse(bad)
	if err != nil {
		t.Fatalf("corrupt block must not fail open: %v", err)
	}
	if _, _, err := s.ReadChannel(1, 2, model.ChSpeed); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt block: %v", err)
	}
	if _, err := s.ReadLap(1, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt lap: %v", err)
	}
	if _, err := s.ReadLap(1, 3); err != nil {
		t.Fatalf("neighbouring lap: %v", err)
	}
	if _, _, err := s.ReadChannel(1, 2, model.ChRPM); err != nil {
		t.Fatalf("neighbouring channel: %v", err)
	}
	if s.Verify() == nil {
		t.Fatal("Verify missed the corrupt block")
	}
}

func TestOpenRejectsDamage(t *testing.T) {
	data, _ := buildTest(t)
	mutate := func(name string, f func(b []byte) []byte, want error) {
		t.Helper()
		_, err := Parse(f(slices.Clone(data)))
		if err == nil || (want != nil && !errors.Is(err, want)) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	mutate("truncated", func(b []byte) []byte { return b[:len(b)-7] }, ErrCorrupt)
	mutate("empty", func(b []byte) []byte { return nil }, ErrCorrupt)
	mutate("magic", func(b []byte) []byte { b[0] = 'X'; return b }, ErrCorrupt)
	mutate("version", func(b []byte) []byte { b[4] = 9; return b }, ErrVersion)
	mutate("header byte", func(b []byte) []byte { b[30] ^= 1; return b }, ErrCorrupt)
	mutate("index byte", func(b []byte) []byte { b[len(b)-footerSize-5] ^= 1; return b }, ErrCorrupt)
	mutate("index offset", func(b []byte) []byte { b[len(b)-footerSize] ^= 1; return b }, ErrCorrupt)
}

func TestRefCounting(t *testing.T) {
	data, _ := buildTest(t)
	path := filepath.Join(t.TempDir(), "s.seg")
	if err := WriteFile(path, data); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Acquire() {
		t.Fatal("acquire on open segment failed")
	}
	s.Close() // an in-flight reader still holds a reference
	if _, err := s.ReadLap(1, 1); err != nil {
		t.Fatalf("read while referenced: %v", err)
	}
	s.Release()
	if s.Acquire() {
		t.Fatal("acquire after final release succeeded")
	}
	if _, _, err := s.ReadChannel(1, 1, model.ChSpeed); !errors.Is(err, ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
}

func TestConcurrentReads(t *testing.T) {
	data, laps := buildTest(t)
	s, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				l := laps[i%len(laps)].Lap
				if _, err := s.ReadLap(l.Driver, l.Number); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// FuzzParse: arbitrary bytes must never panic the reader, at open or on read.
func FuzzParse(f *testing.F) {
	data, _ := buildTest(f)
	f.Add(data)
	f.Add(data[:len(data)/2])
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := Parse(b)
		if err != nil {
			return
		}
		for _, l := range s.AllLaps() {
			s.ReadLap(l.Driver, l.Number)
		}
		s.Blocks()
		s.Verify()
	})
}

// A block offset near 2^64 must not wrap the bounds check. The index CRC is
// recomputed, so this is a deliberately crafted file, not random damage.
func TestHugeBlockOffsetIsRejected(t *testing.T) {
	data, _ := buildTest(t)
	le := binary.LittleEndian
	foot := len(data) - footerSize
	indexOff := int(le.Uint64(data[foot:]))
	for _, off := range []uint64{^uint64(0) - 4, ^uint64(0), uint64(foot) + 1} {
		bad := slices.Clone(data)
		le.PutUint64(bad[indexOff+4+4:], off) // first index entry's offset field
		le.PutUint32(bad[foot+12:], crc32.ChecksumIEEE(bad[indexOff:foot]))
		s, err := Parse(bad)
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("offset %#x: Parse err = %v; reading it would panic", off, err)
		}
		_ = s
	}
}
