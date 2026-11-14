package encoding

import (
	"bytes"
	"math"
	"math/rand"
	"slices"
	"testing"
	"testing/quick"
)

type codec struct {
	name   string
	append func([]byte, []int64) []byte
	decode func([]byte, int) ([]int64, error)
}

var codecs = []codec{
	{"delta", AppendDelta, DecodeDelta},
	{"dod", AppendDoD, DecodeDoD},
	{"rle", AppendRLE, DecodeRLE},
}

func TestRoundTripProperty(t *testing.T) {
	for _, c := range codecs {
		t.Run(c.name, func(t *testing.T) {
			f := func(v []int64) bool {
				got, err := c.decode(c.append(nil, v), len(v))
				return err == nil && slices.Equal(got, v)
			}
			if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRoundTripExtremes(t *testing.T) {
	v := []int64{0, math.MaxInt64, math.MinInt64, -1, 1, math.MinInt64, math.MaxInt64, 0}
	for _, c := range codecs {
		got, err := c.decode(c.append(nil, v), len(v))
		if err != nil || !slices.Equal(got, v) {
			t.Errorf("%s: got %v err %v", c.name, got, err)
		}
	}
}

func TestBoolRLERoundTrip(t *testing.T) {
	f := func(bits []bool) bool {
		v := make([]int64, len(bits))
		for i, b := range bits {
			if b {
				v[i] = 1
			}
		}
		enc, ok := AppendBoolRLE(nil, v)
		if !ok {
			return false
		}
		got, err := DecodeBoolRLE(enc, len(v))
		return err == nil && slices.Equal(got, v)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := AppendBoolRLE(nil, []int64{0, 2}); ok {
		t.Fatal("non-boolean input accepted")
	}
}

func TestBlockRoundTripProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 3000; iter++ {
		kind := Kind(rng.Intn(3))
		n := rng.Intn(400)
		values := make([]int64, n)
		var valid []bool
		withMissing := rng.Intn(2) == 0
		if withMissing {
			valid = make([]bool, n)
		}
		cur := int64(rng.Intn(300))
		for i := range values {
			switch kind {
			case KindSmooth:
				cur += int64(rng.Intn(21) - 10)
			case KindStep:
				if rng.Intn(10) == 0 {
					cur = int64(rng.Intn(9))
				}
			case KindBool:
				if rng.Intn(10) == 0 {
					cur ^= 1
				}
				cur &= 1
			}
			values[i] = cur
			if withMissing {
				valid[i] = rng.Intn(5) != 0
			}
		}
		enc, payload := EncodeBlock(kind, values, valid)
		got, gotValid, err := DecodeBlock(enc, n, payload)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		for i := range values {
			present := valid == nil || valid[i]
			gotPresent := gotValid == nil || gotValid[i]
			if present != gotPresent {
				t.Fatalf("iter %d: validity mismatch at %d", iter, i)
			}
			if present && got[i] != values[i] {
				t.Fatalf("iter %d: value mismatch at %d: %d != %d", iter, i, got[i], values[i])
			}
		}
	}
}

func TestBlockPicksCompactCodec(t *testing.T) {
	// Regular 240 ms timestamps: delta-of-delta should win and cost ~1 byte/sample.
	ts := make([]int64, 400)
	for i := range ts {
		ts[i] = 1_000_000 + int64(i)*240
	}
	enc, payload := EncodeBlock(KindSmooth, ts, nil)
	if enc != DoD {
		t.Fatalf("enc = %d, want DoD", enc)
	}
	if len(payload) > len(ts)+8 {
		t.Fatalf("payload %d bytes for %d regular timestamps", len(payload), len(ts))
	}
	// A constant gear trace is a single run.
	gear := make([]int64, 400)
	for i := range gear {
		gear[i] = 8
	}
	if _, payload := EncodeBlock(KindStep, gear, nil); len(payload) > 3 {
		t.Fatalf("constant RLE payload = %d bytes", len(payload))
	}
}

func TestDecodeRejectsBadInput(t *testing.T) {
	good := AppendDelta(nil, []int64{1, 2, 3})
	cases := []struct {
		name string
		fn   func() error
	}{
		{"delta short", func() error { _, err := DecodeDelta(good[:2], 3); return err }},
		{"delta trailing", func() error { _, err := DecodeDelta(append(bytes.Clone(good), 0), 3); return err }},
		{"delta huge count", func() error { _, err := DecodeDelta(good, MaxCount+1); return err }},
		{"delta negative count", func() error { _, err := DecodeDelta(good, -1); return err }},
		{"rle zero run", func() error { _, err := DecodeRLE([]byte{2, 0}, 1); return err }},
		{"rle overrun", func() error { _, err := DecodeRLE([]byte{2, 200}, 3); return err }},
		{"rle underrun", func() error { _, err := DecodeRLE([]byte{2, 1}, 3); return err }},
		{"bool bad first", func() error { _, err := DecodeBoolRLE([]byte{7, 1}, 1); return err }},
		{"block unknown enc", func() error { _, _, err := DecodeBlock(99, 0, nil); return err }},
		{"block short bitmap", func() error { _, _, err := DecodeBlock(Delta|FlagValidity, 64, []byte{1}); return err }},
	}
	for _, c := range cases {
		if c.fn() == nil {
			t.Errorf("%s: expected error", c.name)
		}
	}
}

// Fuzz targets: arbitrary bytes must never panic, and anything that decodes
// must survive a re-encode/decode round trip.

func fuzzCodec(f *testing.F, c codec) {
	f.Add(c.append(nil, []int64{0, 1, -1, 300, 300, 300}), 6)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, 1)
	f.Add([]byte{}, 0)
	f.Fuzz(func(t *testing.T, data []byte, count int) {
		v, err := c.decode(data, count)
		if err != nil {
			return
		}
		if len(v) != count {
			t.Fatalf("decoded %d values, want %d", len(v), count)
		}
		again, err := c.decode(c.append(nil, v), count)
		if err != nil || !slices.Equal(again, v) {
			t.Fatalf("re-encode round trip failed: %v", err)
		}
	})
}

func FuzzDecodeDelta(f *testing.F) { fuzzCodec(f, codecs[0]) }
func FuzzDecodeDoD(f *testing.F)   { fuzzCodec(f, codecs[1]) }
func FuzzDecodeRLE(f *testing.F)   { fuzzCodec(f, codecs[2]) }

func FuzzDecodeBoolRLE(f *testing.F) {
	f.Add([]byte{1, 3, 2}, 5)
	f.Fuzz(func(t *testing.T, data []byte, count int) {
		v, err := DecodeBoolRLE(data, count)
		if err == nil && len(v) != count {
			t.Fatalf("decoded %d values, want %d", len(v), count)
		}
	})
}

func FuzzDecodeBlock(f *testing.F) {
	enc, payload := EncodeBlock(KindSmooth, []int64{5, 6, 7, 7}, []bool{true, false, true, true})
	f.Add(enc, 4, payload)
	f.Fuzz(func(t *testing.T, enc byte, count int, payload []byte) {
		v, valid, err := DecodeBlock(enc, count, payload)
		if err != nil {
			return
		}
		if len(v) != count || (valid != nil && len(valid) != count) {
			t.Fatalf("length mismatch")
		}
	})
}

func BenchmarkDecodeDelta(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	v := make([]int64, 400)
	cur := int64(200)
	for i := range v {
		cur += int64(rng.Intn(9) - 4)
		v[i] = cur
	}
	enc := AppendDelta(nil, v)
	b.SetBytes(int64(len(v) * 8))
	for i := 0; i < b.N; i++ {
		if _, err := DecodeDelta(enc, len(v)); err != nil {
			b.Fatal(err)
		}
	}
}
