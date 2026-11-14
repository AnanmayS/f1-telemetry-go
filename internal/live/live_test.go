package live

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AnanmayS/f1-telemetry-go/internal/ingest"
	"github.com/AnanmayS/f1-telemetry-go/internal/laps"
	"github.com/AnanmayS/f1-telemetry-go/internal/model"
	"github.com/AnanmayS/f1-telemetry-go/internal/openf1"
	"github.com/AnanmayS/f1-telemetry-go/internal/segment"
	"github.com/AnanmayS/f1-telemetry-go/internal/store"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	testSession = 4242
	testYear    = 2024
	lapMs       = 20_000 // 1 km laps at 180 km/h
	numLaps     = 6
)

var testEpoch = time.Date(testYear, 6, 1, 12, 0, 0, 0, time.UTC)

// seedRawCache writes a small recorded session into dataDir/raw in the same
// form the OpenF1 client caches it: 3 drivers, 6 laps each, ~4 Hz telemetry.
func seedRawCache(t testing.TB, dataDir string) {
	t.Helper()
	c := openf1.New(filepath.Join(dataDir, "raw"), 0, 0)
	put := func(path string, rows any) {
		b, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.WriteCache(path, b); err != nil {
			t.Fatal(err)
		}
	}
	type row = map[string]any
	ts := func(ms int64) string {
		return testEpoch.Add(time.Duration(ms) * time.Millisecond).Format("2006-01-02T15:04:05.000000+00:00")
	}
	put(fmt.Sprintf("_meta/sessions_%d.json.gz", testYear), []row{{
		"session_key": testSession, "meeting_key": 9, "session_name": "Qualifying", "session_type": "Qualifying", "year": testYear,
		"date_start": testEpoch.Format(time.RFC3339), "date_end": testEpoch.Add(time.Hour).Format(time.RFC3339),
		"circuit_short_name": "Testring", "country_name": "Nowhere", "is_cancelled": false}})
	put(fmt.Sprintf("_meta/meetings_%d.json.gz", testYear), []row{{"meeting_key": 9, "meeting_name": "Test Grand Prix"}})
	drivers := []int{1, 4, 16}
	var drv, lapRows []row
	rng := rand.New(rand.NewSource(3))
	for i, d := range drivers {
		drv = append(drv, row{"driver_number": d, "name_acronym": fmt.Sprintf("D%02d", d), "full_name": fmt.Sprintf("Driver %d", d), "team_name": "Team", "team_colour": "FF8000"})
		off := int64(5_000 + i*731)
		for n := 1; n <= numLaps; n++ {
			start := off + int64(n-1)*lapMs
			r := row{"driver_number": d, "lap_number": n, "date_start": ts(start), "is_pit_out_lap": n == 1,
				"lap_duration": float64(lapMs) / 1000, "duration_sector_1": 6.5, "duration_sector_2": 7.0, "duration_sector_3": 6.5}
			if n == 1 {
				r["lap_duration"], r["duration_sector_1"] = nil, nil
			}
			lapRows = append(lapRows, r)
		}
		var car, loc []row
		for tm := int64(0); tm <= off+numLaps*lapMs+3_000; tm += 200 + int64(rng.Intn(100)) {
			thr := 100
			if tm < 3_000 {
				thr = 104
			}
			car = append(car, row{"date": ts(tm), "speed": 170 + rng.Intn(20), "rpm": 10000 + rng.Intn(1500), "n_gear": 6 + rng.Intn(3),
				"throttle": thr, "brake": []int{0, 0, 0, 100}[rng.Intn(4)], "drs": []int{8, 8, 12}[rng.Intn(3)]})
			loc = append(loc, row{"date": ts(tm + 90), "x": tm / 7, "y": -tm / 9, "z": 12})
		}
		put(fmt.Sprintf("%d/car_data_%d.json.gz", testSession, d), car)
		put(fmt.Sprintf("%d/location_%d.json.gz", testSession, d), loc)
	}
	put(fmt.Sprintf("%d/drivers_all.json.gz", testSession), drv)
	put(fmt.Sprintf("%d/laps_all.json.gz", testSession), lapRows)
}

func loadRaw(t testing.TB, dataDir string) *ingest.Raw {
	t.Helper()
	c := openf1.New(filepath.Join(dataDir, "raw"), 0, 0)
	c.Offline, c.Log = true, quiet
	in := &ingest.Ingester{Client: c, DataDir: dataDir, Workers: 2, Log: quiet}
	sessions, err := in.Discover(context.Background(), testYear)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discover: %v %v", sessions, err)
	}
	raw, err := in.Fetch(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// sameLaps fails unless both segments hold exactly the same laps and samples.
func sameLaps(t testing.TB, got, want *segment.Segment) {
	t.Helper()
	if got.Meta.CircuitLengthM != want.Meta.CircuitLengthM || got.Meta.Samples != want.Meta.Samples {
		t.Fatalf("meta: circuit %.2f/%.2f samples %d/%d", got.Meta.CircuitLengthM, want.Meta.CircuitLengthM, got.Meta.Samples, want.Meta.Samples)
	}
	if !slices.Equal(got.AllLaps(), want.AllLaps()) {
		t.Fatalf("lap tables differ:\n got %+v\nwant %+v", got.AllLaps(), want.AllLaps())
	}
	for _, l := range want.AllLaps() {
		a, err := got.ReadLap(l.Driver, l.Number)
		if err != nil {
			t.Fatal(err)
		}
		b, err := want.ReadLap(l.Driver, l.Number)
		if err != nil {
			t.Fatal(err)
		}
		for c := model.Channel(0); c < model.NumChannels; c++ {
			if !slices.Equal(a.Values[c], b.Values[c]) || !slices.Equal(a.Valid[c], b.Valid[c]) {
				t.Fatalf("driver %d lap %d channel %s differs (lost or duplicated samples)", l.Driver, l.Number, c)
			}
		}
	}
}

func batchSegment(t testing.TB, raw *ingest.Raw) *segment.Segment {
	t.Helper()
	data, _, _, err := ingest.BuildSegment(raw.Meta, raw.Timing, raw.Car, raw.Loc)
	if err != nil || data == nil {
		t.Fatalf("batch build: %v", err)
	}
	seg, err := segment.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return seg
}

func sealedSegment(t testing.TB, dataDir string) *segment.Segment {
	t.Helper()
	man, err := store.LoadManifest(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	e, ok := man.Find(testSession)
	if !ok {
		t.Fatal("session not in manifest")
	}
	// Stronger than lap equality: the sealed file is byte-for-byte what batch
	// ingest writes for the same recording.
	raw := loadRaw(t, dataDir)
	batch, _, _, err := ingest.BuildSegment(raw.Meta, raw.Timing, raw.Car, raw.Loc)
	if err != nil {
		t.Fatal(err)
	}
	if _, sum := store.SegmentFileName(testSession, batch); sum != e.SHA256 {
		t.Fatalf("sealed segment hash %s differs from the batch build %s", e.SHA256[:12], sum[:12])
	}
	seg, err := segment.Open(filepath.Join(dataDir, store.SegmentDir, e.File))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(seg.Close)
	return seg
}

func TestEventRoundTrip(t *testing.T) {
	events := []Event{
		{Seq: 1, Kind: KindCar, Driver: 44, Car: laps.Car{T: -500, Speed: 312, RPM: 11800, Gear: 8, Throttle: laps.Missing, Brake: 1, DRS: 12}},
		{Seq: 1 << 40, Kind: KindLoc, Driver: 1, Loc: laps.Loc{T: 99, X: -13708, Y: 2836, Z: 945}},
		{Seq: 3, Kind: KindLap, Driver: 255, Lap: laps.Timing{Number: 57, StartMs: 5_400_000, HasStart: true, DurationMs: 88_197, SectorMs: [3]int32{model.NoTime, 39_850, 17_570}, PitOut: true}},
		{Seq: 4, Kind: KindLap, Driver: 2, Lap: laps.Timing{Number: 1, DurationMs: model.NoTime, SectorMs: [3]int32{-1, -1, -1}}},
	}
	for _, e := range events {
		got, err := decodeEvent(e.append(nil))
		if err != nil || got != e {
			t.Errorf("round trip: got %+v err %v, want %+v", got, err, e)
		}
	}
	for _, bad := range [][]byte{nil, {recEvent}, {recEvent, 9, 1, 1}, {recMeta, 1}, append(events[0].append(nil), 0), events[0].append(nil)[:6]} {
		if _, err := decodeEvent(bad); err == nil {
			t.Errorf("accepted malformed event %v", bad)
		}
	}
}

func FuzzDecodeEvent(f *testing.F) {
	f.Add(Event{Seq: 7, Kind: KindCar, Driver: 4, Car: laps.Car{T: 1, Speed: 2}}.append(nil))
	f.Add([]byte{recEvent, 3, 1, 1})
	f.Fuzz(func(t *testing.T, b []byte) {
		e, err := decodeEvent(b)
		if err != nil {
			return
		}
		if again, err := decodeEvent(e.append(nil)); err != nil || again != e {
			t.Fatalf("re-encode mismatch: %+v vs %+v (%v)", again, e, err)
		}
	})
}

func TestEventsAreDeterministicAndOrdered(t *testing.T) {
	dir := t.TempDir()
	seedRawCache(t, dir)
	raw := loadRaw(t, dir)
	a, b := Events(raw), Events(raw)
	if !slices.Equal(a, b) {
		t.Fatal("same recording produced different event streams")
	}
	lapEvents := 0
	lastT := int64(-1 << 62)
	for i, e := range a {
		if e.Seq != uint64(i+1) {
			t.Fatalf("seq %d at position %d", e.Seq, i)
		}
		if e.Kind == KindLap {
			lapEvents++
			continue
		}
		if at := eventTime(e, 0); at < lastT {
			t.Fatalf("event %d goes back in time", e.Seq)
		} else {
			lastT = at
		}
	}
	if want := 3 * numLaps * 2; lapEvents != want { // a start row and a completed row per lap
		t.Fatalf("lap events = %d, want %d", lapEvents, want)
	}
}

func TestSessionRecoversAndIgnoresResends(t *testing.T) {
	dir := t.TempDir()
	seedRawCache(t, dir)
	raw := loadRaw(t, dir)
	events := Events(raw)
	liveDir := filepath.Join(dir, "live")

	s, err := Open(liveDir, raw.Meta, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	half := len(events) / 2
	for _, e := range events[:half] {
		if ok, err := s.Apply(e); err != nil || !ok {
			t.Fatalf("apply %d: ok=%v err=%v", e.Seq, ok, err)
		}
	}
	if d := s.DurableSeq(); d != 0 {
		t.Fatalf("durable = %d before any sync", d)
	}
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	if d := s.DurableSeq(); d != uint64(half) {
		t.Fatalf("durable = %d after sync, want %d", d, half)
	}
	s.Close() // simulates a clean stop; the crash test covers kill -9

	// Reopen: state comes back from the WAL, and a source that resends
	// everything from the start creates no duplicates.
	s, err = Open(liveDir, segment.Meta{Session: raw.Meta.Session}, time.Hour) // drivers come from the WAL
	if err != nil {
		t.Fatal(err)
	}
	if s.LastSeq() != uint64(half) || s.DurableSeq() != uint64(half) || len(s.Meta().Drivers) != 3 {
		t.Fatalf("recovered: last=%d durable=%d drivers=%d", s.LastSeq(), s.DurableSeq(), len(s.Meta().Drivers))
	}
	resent := 0
	for _, e := range events {
		ok, err := s.Apply(e)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			resent++
		}
	}
	if resent != half {
		t.Fatalf("ignored %d resends, want %d", resent, half)
	}
	if _, err := s.Apply(Event{Seq: 1 << 50, Kind: 9}); err == nil {
		t.Error("bad event kind accepted")
	}
	if _, err := s.Seal(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(WALPath(liveDir, testSession)); !os.IsNotExist(err) {
		t.Fatal("WAL still present after seal")
	}
	sameLaps(t, sealedSegment(t, dir), batchSegment(t, raw))

	// A WAL for another session is refused rather than mixed in.
	other := raw.Meta
	other.Session.Key = 777
	s2, _ := Open(liveDir, other, 0)
	s2.Close()
	os.Rename(WALPath(liveDir, 777), WALPath(liveDir, testSession))
	if _, err := Open(liveDir, raw.Meta, 0); err == nil {
		t.Fatal("opened a WAL that belongs to another session")
	}
}

// A lap must be queryable shortly after the car crosses the line, through
// the same store the API uses, and the sealed file must take over afterwards.
func TestLapVisibleSoonAfterCrossingTheLine(t *testing.T) {
	dir := t.TempDir()
	seedRawCache(t, dir)
	raw := loadRaw(t, dir)
	st, err := store.Open(dir, 0, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const speed = 20.0
	wallStart := time.Now()
	type seen struct {
		lap   int
		after time.Duration // wall time between the lap ending in the feed and being queryable
	}
	visible := make(chan seen, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Poll like a client would: wait for driver 1's laps to gain official times.
		next := 2
		for ctx.Err() == nil && next <= numLaps {
			if sess, err := st.Acquire(testSession); err == nil {
				if l, ok := sess.Lap(1, next); ok && l.ValidTime() && l.Samples > 0 {
					if _, err := st.Lap(sess, 1, next); err == nil {
						feedEnd := float64(l.StartMs+int64(l.OfficialMs)) - float64(Events(raw)[0].Car.T)
						due := wallStart.Add(time.Duration(feedEnd/speed) * time.Millisecond)
						visible <- seen{next, time.Since(due)}
						next++
					}
				}
				sess.Release()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	r := &Runner{Store: st, DataDir: dir, LiveDir: filepath.Join(dir, "live"), SyncEvery: 20 * time.Millisecond, Refresh: 50 * time.Millisecond, Log: quiet}
	if err := r.Replay(ctx, raw, speed); err != nil {
		t.Fatal(err)
	}
	cancel()
	close(visible)
	n := 0
	for s := range visible {
		n++
		// The PRD allows 10 s; with a 50 ms refresh this should be well under 1 s.
		if s.after > time.Second {
			t.Errorf("lap %d became queryable %s after the car crossed the line", s.lap, s.after)
		}
		t.Logf("lap %d queryable %s after crossing the line", s.lap, s.after.Round(time.Millisecond))
	}
	if n < numLaps-2 {
		t.Fatalf("only %d laps were seen live", n)
	}
	// After sealing, the store serves the file and the result equals batch ingest.
	sess, err := st.Acquire(testSession)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Release()
	if strings.HasPrefix(sess.Gen, "live-") {
		t.Fatal("store still serves the in-memory segment after seal")
	}
	sameLaps(t, sess.Segment, batchSegment(t, raw))
}

// TestCrashRecovery replays a session through cmd/replay, killing the
// process with SIGKILL at random moments. After every restart the WAL must
// hold at least everything the previous run acknowledged, and the sealed
// segment must equal a batch build of the same recording: nothing lost,
// nothing duplicated.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns and kills processes")
	}
	dir := t.TempDir()
	seedRawCache(t, dir)
	raw := loadRaw(t, dir)
	total := uint64(len(Events(raw)))

	bin := filepath.Join(t.TempDir(), "replay")
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/AnanmayS/f1-telemetry-go/cmd/replay").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	var lastAck uint64
	kills, tornTails := 0, 0
	walPath := WALPath(filepath.Join(dir, "live"), testSession)
	for run := 1; ; run++ {
		if run > 60 {
			t.Fatal("replay did not finish in 60 runs")
		}
		sizeBefore := int64(-1)
		if st, err := os.Stat(walPath); err == nil {
			sizeBefore = st.Size()
		}
		// 10x real time, as the PRD specifies; fsync every 20 ms.
		cmd := exec.Command(bin, "-data", dir, "-year", strconv.Itoa(testYear), "-session", strconv.Itoa(testSession), "-speed", "10", "-sync", "20ms")
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		// Kill somewhere in the next 0.2-1.5 s (never, on late runs, so it terminates).
		var timer *time.Timer
		if run <= 12 {
			timer = time.AfterFunc(time.Duration(200+rng.Intn(1300))*time.Millisecond, func() { cmd.Process.Signal(syscall.SIGKILL) })
		}
		sealed := false
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			switch {
			case len(f) == 2 && f[0] == "RECOVERED":
				rec, _ := strconv.ParseUint(f[1], 10, 64)
				if rec < lastAck {
					t.Fatalf("run %d: WAL recovered up to seq %d but seq %d was acknowledged before the crash: acknowledged events were lost", run, rec, lastAck)
				}
				if st, err := os.Stat(walPath); err == nil && sizeBefore > st.Size() {
					tornTails++
				}
			case len(f) == 2 && f[0] == "ACK":
				ack, _ := strconv.ParseUint(f[1], 10, 64)
				if ack < lastAck {
					t.Fatalf("run %d: acknowledgement went backwards (%d after %d)", run, ack, lastAck)
				}
				lastAck = ack
			case len(f) == 1 && f[0] == "SEALED":
				sealed = true
			}
		}
		err = cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		if sealed {
			if err != nil {
				t.Fatalf("run %d: sealed but exited with %v", run, err)
			}
			break
		}
		ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("run %d: exited without sealing and was not killed: %v\n%s", run, err, stderr.String())
		}
		kills++
	}
	if kills < 3 {
		t.Fatalf("only %d kills happened; the test did not exercise recovery", kills)
	}
	if lastAck != total {
		t.Fatalf("last acknowledged seq %d, want %d", lastAck, total)
	}
	sameLaps(t, sealedSegment(t, dir), batchSegment(t, raw))
	if _, err := os.Stat(walPath); !os.IsNotExist(err) {
		t.Fatal("WAL not removed after seal")
	}
	t.Logf("%d events, %d SIGKILLs, %d torn WAL tails truncated on restart; sealed segment equals the batch build", total, kills, tornTails)
}
