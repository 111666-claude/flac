package flac

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/mewkiz/flac/meta"
)

func TestSampleWidth(t *testing.T) {
	cases := []struct {
		bps  uint8
		want int
	}{
		{1, 1}, {4, 1}, {8, 1},
		{9, 2}, {12, 2}, {16, 2},
		{17, 3}, {20, 3}, {24, 3},
		{25, 4}, {32, 4},
		{0, 0}, {33, 0},
	}
	for _, tc := range cases {
		if got := sampleWidth(tc.bps); got != tc.want {
			t.Errorf("sampleWidth(%d) = %d, want %d", tc.bps, got, tc.want)
		}
	}
}

func TestReadRange_UnsupportedBitsPerSample(t *testing.T) {
	// A 33-bit StreamInfo is accepted by the metadata layer but cannot be
	// packed into the supported whole-byte PCM widths; ReadRange must reject
	// it with an overflow-class error rather than panic.
	s := &Stream{
		r: bytes.NewReader(nil), // implements io.ReadSeeker
		Info: &meta.StreamInfo{
			SampleRate:    44100,
			NChannels:     2,
			BitsPerSample: 33,
			NSamples:      100,
		},
	}
	_, err := s.ReadRange(0, 10)
	if !errors.Is(err, ErrRangeOverflow) {
		t.Fatalf("expected ErrRangeOverflow for unsupported bps, got %v", err)
	}
}

func TestRangeReader_ReadContracts(t *testing.T) {
	t.Run("empty buffer", func(t *testing.T) {
		rr := &RangeReader{NSamples: 1, stride: 4, delivered: 1}
		n, err := rr.Read(make([]byte, 0))
		if n != 0 || err != nil {
			t.Fatalf("Read(empty) = (%d, %v), want (0, nil)", n, err)
		}
	})

	t.Run("immediate EOF when range already delivered", func(t *testing.T) {
		rr := &RangeReader{NSamples: 4, stride: 2, delivered: 4}
		n, err := rr.Read(make([]byte, 8))
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("Read = (%d, %v), want (0, io.EOF)", n, err)
		}
	})

	t.Run("Remaining counts undelivered samples", func(t *testing.T) {
		// 3 samples of 2 bytes each already returned out of 10 requested.
		rr := &RangeReader{NSamples: 10, stride: 2, readBytes: 6}
		if rem := rr.Remaining(); rem != 7 {
			t.Fatalf("Remaining = %d, want 7", rem)
		}
	})
}

// TestReadRange_OverflowGuards keeps the integer-overflow validation explicit
// and independent of any concrete audio fixture.
func TestReadRange_OverflowGuards(t *testing.T) {
	s := &Stream{
		r: bytes.NewReader(nil),
		Info: &meta.StreamInfo{
			SampleRate:    44100,
			NChannels:     2,
			BitsPerSample: 16,
			NSamples:      math.MaxUint64, // length unknown-ish but huge
		},
	}
	if _, err := s.ReadRange(math.MaxUint64-1, 2); !errors.Is(err, ErrRangeOverflow) {
		t.Fatalf("wrap-around range: got %v, want ErrRangeOverflow", err)
	}
	if _, err := s.ReadRange(math.MaxUint64, math.MaxUint64); !errors.Is(err, ErrRangeOverflow) {
		t.Fatalf("max range: got %v, want ErrRangeOverflow", err)
	}
}
