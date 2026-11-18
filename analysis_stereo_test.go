package flac

import (
	"math"
	"slices"
	"testing"

	"github.com/mewkiz/flac/frame"
)

// makeStereoFrame returns a stereo frame holding the given left and right
// channel samples.
func makeStereoFrame(left, right []int32) *frame.Frame {
	n := len(left)
	return &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(n),
			SampleRate:        44100,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     16,
		},
		Subframes: []*frame.Subframe{
			{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: left, NSamples: n},
			{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: right, NSamples: n},
		},
	}
}

// TestAnalyzeStereoOverflow checks that extreme sample values for which the
// side channel (left - right) cannot be represented in 32 bits deterministically
// result in independent stereo.
func TestAnalyzeStereoOverflow(t *testing.T) {
	left := []int32{math.MaxInt32, math.MaxInt32, 0, math.MinInt32}
	right := []int32{math.MinInt32, math.MinInt32, 0, math.MaxInt32}
	f := makeStereoFrame(left, right)
	// Use 32 bits-per-sample; the side channel would require 33 bits.
	if got := analyzeStereo(f, 32); got != frame.ChannelsLR {
		t.Errorf("channel assignment mismatch; expected %v, got %v", frame.ChannelsLR, got)
	}
}

// TestAnalyzeStereoTie checks that equal estimates deterministically keep the
// earliest candidate (independent before left/side before right/side before
// mid/side).
func TestAnalyzeStereoTie(t *testing.T) {
	// Identical channels: side is constant zero, and left/side, right/side and
	// mid/side all estimate to the same number of bits; left/side is considered
	// first and must win the tie.
	samples := make([]int32, 512)
	for i := range samples {
		samples[i] = int32(i * 7)
	}
	f := makeStereoFrame(slices.Clone(samples), slices.Clone(samples))
	if got := analyzeStereo(f, 16); got != frame.ChannelsLeftSide {
		t.Errorf("channel assignment mismatch; expected %v, got %v", frame.ChannelsLeftSide, got)
	}

	// Silent channels: the constant side channel (6+17 bits) costs one bit more
	// than the constant right channel (6+16 bits), so independent stereo must
	// win.
	silence := make([]int32, 512)
	f = makeStereoFrame(slices.Clone(silence), slices.Clone(silence))
	if got := analyzeStereo(f, 16); got != frame.ChannelsLR {
		t.Errorf("channel assignment mismatch; expected %v, got %v", frame.ChannelsLR, got)
	}
}

// TestAnalyzeStereoDegenerate checks that degenerate frames (zero, one and an
// odd number of samples) produce a deterministic result without panicking.
func TestAnalyzeStereoDegenerate(t *testing.T) {
	for _, n := range []int{0, 1, 2, 3, 725} {
		left := make([]int32, n)
		right := make([]int32, n)
		for i := range left {
			left[i] = int32(i*13 - 100)
			right[i] = int32(100 - i*11)
		}
		f := makeStereoFrame(left, right)
		got1 := analyzeStereo(f, 16)
		got2 := analyzeStereo(f, 16)
		if got1 != got2 {
			t.Errorf("n=%d: non-deterministic result; got %v then %v", n, got1, got2)
		}
	}
}

// TestAnalyzeStereoNonDestructive checks that the analysis does not modify the
// samples of the frame.
func TestAnalyzeStereoNonDestructive(t *testing.T) {
	left := make([]int32, 4096)
	right := make([]int32, 4096)
	for i := range left {
		left[i] = int32(i*31 - 5000)
		right[i] = int32(5000 - i*17)
	}
	wantLeft := slices.Clone(left)
	wantRight := slices.Clone(right)
	f := makeStereoFrame(left, right)
	analyzeStereo(f, 16)
	if !slices.Equal(left, wantLeft) {
		t.Error("left channel samples modified by analysis")
	}
	if !slices.Equal(right, wantRight) {
		t.Error("right channel samples modified by analysis")
	}
	if f.Channels != frame.ChannelsLR {
		t.Errorf("frame channel assignment modified by analysis; expected %v, got %v", frame.ChannelsLR, f.Channels)
	}
}
