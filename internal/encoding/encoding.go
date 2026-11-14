// Package encoding implements the integer codecs used by segment blocks:
// delta, delta-of-delta, run-length and boolean run-length, plus the
// validity bitmap that marks missing samples.
//
// Every decoder is safe on arbitrary input: it returns an error and never
// panics or allocates more than the caller-declared count allows.
package encoding

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Encoding identifiers stored in the low 7 bits of a block's encoding byte.
const (
	Delta   byte = 1 // zigzag varint of v[i]-v[i-1]
	DoD     byte = 2 // zigzag varint of delta-of-delta
	RLE     byte = 3 // (zigzag varint value, uvarint run length) pairs
	BoolRLE byte = 4 // first value byte, then alternating uvarint run lengths

	// FlagValidity is set on the encoding byte when the payload starts with
	// a validity bitmap of ceil(count/8) bytes (bit set = value present).
	FlagValidity byte = 0x80
)

// MaxCount bounds the number of values in one block. A lap at ~4 Hz holds a
// few hundred samples; even a red-flagged "lap" stays far below this.
const MaxCount = 1 << 20

var (
	ErrCorrupt  = errors.New("encoding: corrupt payload")
	ErrTooLarge = errors.New("encoding: count exceeds limit")
)

func zigzag(v int64) uint64   { return uint64(v<<1) ^ uint64(v>>63) }
func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// AppendDelta appends the delta encoding of v to dst.
func AppendDelta(dst []byte, v []int64) []byte {
	var prev int64
	for _, x := range v {
		dst = binary.AppendUvarint(dst, zigzag(x-prev))
		prev = x
	}
	return dst
}

// DecodeDelta decodes exactly count values and requires src to be fully consumed.
func DecodeDelta(src []byte, count int) ([]int64, error) {
	if err := checkCount(count, len(src)); err != nil {
		return nil, err
	}
	out := make([]int64, count)
	var prev int64
	for i := range out {
		u, n := binary.Uvarint(src)
		if n <= 0 {
			return nil, ErrCorrupt
		}
		src = src[n:]
		prev += unzigzag(u)
		out[i] = prev
	}
	if len(src) != 0 {
		return nil, ErrCorrupt
	}
	return out, nil
}

// AppendDoD appends the delta-of-delta encoding of v to dst.
func AppendDoD(dst []byte, v []int64) []byte {
	var prev, prevDelta int64
	for _, x := range v {
		d := x - prev
		dst = binary.AppendUvarint(dst, zigzag(d-prevDelta))
		prev, prevDelta = x, d
	}
	return dst
}

// DecodeDoD decodes exactly count values and requires src to be fully consumed.
func DecodeDoD(src []byte, count int) ([]int64, error) {
	if err := checkCount(count, len(src)); err != nil {
		return nil, err
	}
	out := make([]int64, count)
	var prev, prevDelta int64
	for i := range out {
		u, n := binary.Uvarint(src)
		if n <= 0 {
			return nil, ErrCorrupt
		}
		src = src[n:]
		prevDelta += unzigzag(u)
		prev += prevDelta
		out[i] = prev
	}
	if len(src) != 0 {
		return nil, ErrCorrupt
	}
	return out, nil
}

// AppendRLE appends (value, run length) pairs for v to dst.
func AppendRLE(dst []byte, v []int64) []byte {
	for i := 0; i < len(v); {
		j := i + 1
		for j < len(v) && v[j] == v[i] {
			j++
		}
		dst = binary.AppendUvarint(dst, zigzag(v[i]))
		dst = binary.AppendUvarint(dst, uint64(j-i))
		i = j
	}
	return dst
}

// DecodeRLE decodes runs that must sum to exactly count.
func DecodeRLE(src []byte, count int) ([]int64, error) {
	if count < 0 || count > MaxCount {
		return nil, ErrTooLarge
	}
	out := make([]int64, 0, count)
	for len(src) > 0 {
		u, n := binary.Uvarint(src)
		if n <= 0 {
			return nil, ErrCorrupt
		}
		src = src[n:]
		run, n := binary.Uvarint(src)
		if n <= 0 || run == 0 || run > uint64(count-len(out)) {
			return nil, ErrCorrupt
		}
		src = src[n:]
		val := unzigzag(u)
		for k := uint64(0); k < run; k++ {
			out = append(out, val)
		}
	}
	if len(out) != count {
		return nil, ErrCorrupt
	}
	return out, nil
}

// AppendBoolRLE encodes v (each value 0 or 1) as the first value followed by
// alternating run lengths. It reports false if v holds a non-boolean value.
func AppendBoolRLE(dst []byte, v []int64) ([]byte, bool) {
	for _, x := range v {
		if x != 0 && x != 1 {
			return dst, false
		}
	}
	if len(v) == 0 {
		return dst, true
	}
	dst = append(dst, byte(v[0]))
	for i := 0; i < len(v); {
		j := i + 1
		for j < len(v) && v[j] == v[i] {
			j++
		}
		dst = binary.AppendUvarint(dst, uint64(j-i))
		i = j
	}
	return dst, true
}

// DecodeBoolRLE decodes alternating runs that must sum to exactly count.
func DecodeBoolRLE(src []byte, count int) ([]int64, error) {
	if count < 0 || count > MaxCount {
		return nil, ErrTooLarge
	}
	if count == 0 {
		if len(src) != 0 {
			return nil, ErrCorrupt
		}
		return []int64{}, nil
	}
	if len(src) == 0 || src[0] > 1 {
		return nil, ErrCorrupt
	}
	val := int64(src[0])
	src = src[1:]
	out := make([]int64, 0, count)
	for len(src) > 0 {
		run, n := binary.Uvarint(src)
		if n <= 0 || run == 0 || run > uint64(count-len(out)) {
			return nil, ErrCorrupt
		}
		src = src[n:]
		for k := uint64(0); k < run; k++ {
			out = append(out, val)
		}
		val ^= 1
	}
	if len(out) != count {
		return nil, ErrCorrupt
	}
	return out, nil
}

// checkCount rejects counts that a varint stream of srcLen bytes cannot hold
// (every value takes at least one byte), which bounds allocation by input size.
func checkCount(count, srcLen int) error {
	if count < 0 || count > MaxCount {
		return ErrTooLarge
	}
	if count > srcLen {
		return ErrCorrupt
	}
	return nil
}

// Kind tells EncodeBlock which codecs suit a channel.
type Kind uint8

const (
	KindSmooth Kind = iota // speed, rpm, throttle, x, y, z, distance, t
	KindStep               // gear, drs
	KindBool               // brake
)

// EncodeBlock encodes values, choosing the smallest codec suited to kind.
// valid may be nil (all present); otherwise missing positions are recorded in
// a validity bitmap and their value replaced by the previous present value so
// they cost nothing in the codec stream.
func EncodeBlock(kind Kind, values []int64, valid []bool) (enc byte, payload []byte) {
	anyMissing := false
	for _, ok := range valid {
		if !ok {
			anyMissing = true
			break
		}
	}
	if anyMissing {
		filled := make([]int64, len(values))
		var last int64
		for i, x := range values {
			if valid[i] {
				last = x
			}
			filled[i] = last
		}
		values = filled
		payload = make([]byte, (len(values)+7)/8)
		for i, ok := range valid {
			if ok {
				payload[i/8] |= 1 << (i % 8)
			}
		}
	}
	prefix := len(payload)
	var best []byte
	try := func(e byte, b []byte) {
		if best == nil || len(b) < len(best) {
			enc, best = e, b
		}
	}
	switch kind {
	case KindSmooth:
		try(Delta, AppendDelta(nil, values))
		try(DoD, AppendDoD(nil, values))
	case KindStep:
		try(RLE, AppendRLE(nil, values))
	case KindBool:
		if b, ok := AppendBoolRLE(nil, values); ok {
			try(BoolRLE, b)
		} else {
			try(RLE, AppendRLE(nil, values))
		}
	}
	if best == nil { // zero values: any codec encodes to nothing
		enc = Delta
	}
	payload = append(payload[:prefix], best...)
	if anyMissing {
		enc |= FlagValidity
	}
	return enc, payload
}

// DecodeBlock reverses EncodeBlock. valid is nil when every value is present.
func DecodeBlock(enc byte, count int, payload []byte) (values []int64, valid []bool, err error) {
	if count < 0 || count > MaxCount {
		return nil, nil, ErrTooLarge
	}
	if enc&FlagValidity != 0 {
		n := (count + 7) / 8
		if len(payload) < n {
			return nil, nil, ErrCorrupt
		}
		valid = make([]bool, count)
		for i := range valid {
			valid[i] = payload[i/8]&(1<<(i%8)) != 0
		}
		payload = payload[n:]
	}
	switch enc &^ FlagValidity {
	case Delta:
		values, err = DecodeDelta(payload, count)
	case DoD:
		values, err = DecodeDoD(payload, count)
	case RLE:
		values, err = DecodeRLE(payload, count)
	case BoolRLE:
		values, err = DecodeBoolRLE(payload, count)
	default:
		err = fmt.Errorf("%w: unknown encoding %d", ErrCorrupt, enc)
	}
	if err != nil {
		return nil, nil, err
	}
	return values, valid, nil
}
