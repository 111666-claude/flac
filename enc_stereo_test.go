package flac_test

import (
	"bytes"
	"crypto/md5"
	"io"
	"math"
	"math/rand"
	"slices"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// stereoFixtures maps fixture names to generators of left and right channel
// samples (nsamples per channel).
var stereoFixtures = map[string]func(nsamples int) (left, right []int32){
	// In-phase stereo: both channels hold the same signal.
	"in-phase": func(nsamples int) (left, right []int32) {
		left = make([]int32, nsamples)
		right = make([]int32, nsamples)
		for i := 0; i < nsamples; i++ {
			sample := int32(math.Sin(2*math.Pi*440*float64(i)/44100) * 30000)
			left[i] = sample
			right[i] = sample
		}
		return left, right
	},
	// Anti-phase stereo: the right channel holds the negated left channel.
	"anti-phase": func(nsamples int) (left, right []int32) {
		left = make([]int32, nsamples)
		right = make([]int32, nsamples)
		for i := 0; i < nsamples; i++ {
			sample := int32(math.Sin(2*math.Pi*440*float64(i)/44100) * 30000)
			left[i] = sample
			right[i] = -sample
		}
		return left, right
	},
	// Uncorrelated stereo: the channels hold independent noise.
	"uncorrelated": func(nsamples int) (left, right []int32) {
		left = make([]int32, nsamples)
		right = make([]int32, nsamples)
		lrand := rand.New(rand.NewSource(1))
		rrand := rand.New(rand.NewSource(2))
		for i := 0; i < nsamples; i++ {
			left[i] = int32(lrand.Intn(65536) - 32768)
			right[i] = int32(rrand.Intn(65536) - 32768)
		}
		return left, right
	},
}

// encodeAudio encodes the given per-channel samples into a FLAC stream, one
// frame per blockSize samples. It returns the encoded FLAC stream and the MD5
// checksum of the unencoded audio samples.
func encodeAudio(t *testing.T, bps uint8, blockSize int, chans [][]int32, stereoAnalysis bool) (data, sum []byte) {
	t.Helper()
	nchannels := len(chans)
	nsamples := len(chans[0])
	info := &meta.StreamInfo{
		BlockSizeMin:  uint16(blockSize),
		BlockSizeMax:  uint16(blockSize),
		SampleRate:    44100,
		NChannels:     uint8(nchannels),
		BitsPerSample: bps,
		NSamples:      uint64(nsamples),
	}
	buf := new(bytes.Buffer)
	enc, err := flac.NewEncoder(buf, info)
	if err != nil {
		t.Fatalf("unable to create encoder; %v", err)
	}
	enc.EnableStereoAnalysis(stereoAnalysis)
	channels := frame.ChannelsMono
	if nchannels == 2 {
		channels = frame.ChannelsLR
	}
	h := md5.New()
	for offset := 0; offset < nsamples; offset += blockSize {
		n := min(blockSize, nsamples-offset)
		f := &frame.Frame{
			Header: frame.Header{
				HasFixedBlockSize: true,
				BlockSize:         uint16(n),
				SampleRate:        44100,
				Channels:          channels,
				BitsPerSample:     bps,
			},
		}
		for c := range chans {
			// Note: clone samples, as the encoder decorrelates (and restores)
			// the samples of the frame in place.
			samples := slices.Clone(chans[c][offset : offset+n])
			f.Subframes = append(f.Subframes, &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   samples,
				NSamples:  n,
			})
		}
		// Add unencoded audio samples to running MD5 hash.
		f.Hash(h)
		if err := enc.WriteFrame(f); err != nil {
			t.Fatalf("unable to encode audio frame; %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("unable to close encoder; %v", err)
	}
	return buf.Bytes(), h.Sum(nil)
}

// decodeAudio decodes the given FLAC stream, returning the channel assignment
// of each frame, the decoded per-channel samples and the MD5 checksum of the
// decoded audio samples.
func decodeAudio(t *testing.T, data []byte) (modes []frame.Channels, chans [][]int32, sum []byte) {
	t.Helper()
	stream, err := flac.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse encoded FLAC stream; %v", err)
	}
	defer stream.Close()
	h := md5.New()
	for {
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("unable to parse audio frame; %v", err)
		}
		modes = append(modes, f.Channels)
		if chans == nil {
			chans = make([][]int32, len(f.Subframes))
		}
		for c, subframe := range f.Subframes {
			chans[c] = append(chans[c], subframe.Samples...)
		}
		// Add decoded audio samples to running MD5 hash.
		f.Hash(h)
	}
	return modes, chans, h.Sum(nil)
}

// TestEncodeStereoAnalysisModes checks that the adaptive stereo channel
// assignment selects the expected mode for in-phase, anti-phase and
// uncorrelated stereo fixtures, and that the decoded per-channel PCM samples
// are bit-identical to the input.
func TestEncodeStereoAnalysisModes(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 3
		bps       = 16
	)
	nsamples := blockSize * nblocks
	wants := map[string]frame.Channels{
		// side is constant zero; left/side, right/side and mid/side tie and
		// left/side is considered first.
		"in-phase": frame.ChannelsLeftSide,
		// mid is constant zero.
		"anti-phase": frame.ChannelsMidSide,
		// decorrelation only inflates the sample range of noise.
		"uncorrelated": frame.ChannelsLR,
	}
	for name, gen := range stereoFixtures {
		t.Run(name, func(t *testing.T) {
			left, right := gen(nsamples)
			data, wantSum := encodeAudio(t, bps, blockSize, [][]int32{left, right}, true)
			modes, chans, gotSum := decodeAudio(t, data)
			// Check channel assignment of each frame.
			wantMode := wants[name]
			if len(modes) != nblocks {
				t.Fatalf("frame count mismatch; expected %d, got %d", nblocks, len(modes))
			}
			for i, mode := range modes {
				if mode != wantMode {
					t.Errorf("frame %d: channel assignment mismatch; expected %v, got %v", i, wantMode, mode)
				}
			}
			// Check that decoded samples are bit-identical to the input.
			if !slices.Equal(chans[0], left) {
				t.Error("left channel samples mismatch")
			}
			if !slices.Equal(chans[1], right) {
				t.Error("right channel samples mismatch")
			}
			// Check MD5 checksum of decoded audio samples.
			if !bytes.Equal(gotSum, wantSum) {
				t.Errorf("MD5 checksum mismatch; expected %x, got %x", wantSum, gotSum)
			}
		})
	}
}

// TestEncodeStereoAnalysisSize checks the size trend of adaptive stereo
// channel assignment: correlated fixtures shrink, and no fixture grows.
func TestEncodeStereoAnalysisSize(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 3
		bps       = 16
	)
	nsamples := blockSize * nblocks
	for name, gen := range stereoFixtures {
		t.Run(name, func(t *testing.T) {
			left, right := gen(nsamples)
			dataOff, _ := encodeAudio(t, bps, blockSize, [][]int32{left, right}, false)
			dataOn, _ := encodeAudio(t, bps, blockSize, [][]int32{left, right}, true)
			sizeOff, sizeOn := len(dataOff), len(dataOn)
			t.Logf("stereo analysis disabled: %d bytes, enabled: %d bytes", sizeOff, sizeOn)
			if sizeOn > sizeOff {
				t.Errorf("encoded size grew with stereo analysis; expected <= %d, got %d", sizeOff, sizeOn)
			}
			if name != "uncorrelated" && sizeOn >= sizeOff {
				t.Errorf("correlated fixture did not shrink with stereo analysis; expected < %d, got %d", sizeOff, sizeOn)
			}
		})
	}
}

// TestEncodeStereoAnalysisOddSamples checks that frames with an odd number of
// samples and odd (left + right) sums round-trip bit-identically when a
// decorrelated channel assignment is selected.
func TestEncodeStereoAnalysisOddSamples(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 3
		odd       = 725 // odd number of samples in the last block
		bps       = 16
	)
	nsamples := blockSize*nblocks + odd
	left := make([]int32, nsamples)
	right := make([]int32, nsamples)
	for i := 0; i < nsamples; i++ {
		sample := int32(math.Sin(2*math.Pi*440*float64(i)/44100) * 30000)
		left[i] = sample
		right[i] = sample - 1 // side is constant 1; mid sums are odd
	}
	data, wantSum := encodeAudio(t, bps, blockSize, [][]int32{left, right}, true)
	modes, chans, gotSum := decodeAudio(t, data)
	for i, mode := range modes {
		if mode != frame.ChannelsLeftSide {
			t.Errorf("frame %d: channel assignment mismatch; expected %v, got %v", i, frame.ChannelsLeftSide, mode)
		}
	}
	if !slices.Equal(chans[0], left) {
		t.Error("left channel samples mismatch")
	}
	if !slices.Equal(chans[1], right) {
		t.Error("right channel samples mismatch")
	}
	if !bytes.Equal(gotSum, wantSum) {
		t.Errorf("MD5 checksum mismatch; expected %x, got %x", wantSum, gotSum)
	}
}

// TestEncodeStereoAnalysisExtremeValues checks that extreme sample values
// round-trip bit-identically with stereo analysis enabled.
func TestEncodeStereoAnalysisExtremeValues(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 2
	)
	nsamples := blockSize * nblocks
	for _, bps := range []uint8{8, 16, 24} {
		t.Run(string(rune('0'+bps/10))+string(rune('0'+bps%10))+"bps", func(t *testing.T) {
			maxVal := int32(1<<(bps-1)) - 1
			minVal := -int32(1 << (bps - 1))
			left := make([]int32, nsamples)
			right := make([]int32, nsamples)
			for i := 0; i < nsamples; i++ {
				// Alternate between extreme values.
				if i%2 == 0 {
					left[i] = maxVal
					right[i] = minVal
				} else {
					left[i] = minVal
					right[i] = maxVal
				}
			}
			data, wantSum := encodeAudio(t, bps, blockSize, [][]int32{left, right}, true)
			_, chans, gotSum := decodeAudio(t, data)
			if !slices.Equal(chans[0], left) {
				t.Error("left channel samples mismatch")
			}
			if !slices.Equal(chans[1], right) {
				t.Error("right channel samples mismatch")
			}
			if !bytes.Equal(gotSum, wantSum) {
				t.Errorf("MD5 checksum mismatch; expected %x, got %x", wantSum, gotSum)
			}
		})
	}
}

// TestEncodeStereoAnalysisMono checks that mono frames are unaffected by
// stereo analysis.
func TestEncodeStereoAnalysisMono(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 2
		bps       = 16
	)
	nsamples := blockSize * nblocks
	mono := make([]int32, nsamples)
	for i := 0; i < nsamples; i++ {
		mono[i] = int32(math.Sin(2*math.Pi*440*float64(i)/44100) * 30000)
	}
	data, wantSum := encodeAudio(t, bps, blockSize, [][]int32{mono}, true)
	modes, chans, gotSum := decodeAudio(t, data)
	for i, mode := range modes {
		if mode != frame.ChannelsMono {
			t.Errorf("frame %d: channel assignment mismatch; expected %v, got %v", i, frame.ChannelsMono, mode)
		}
	}
	if !slices.Equal(chans[0], mono) {
		t.Error("mono channel samples mismatch")
	}
	if !bytes.Equal(gotSum, wantSum) {
		t.Errorf("MD5 checksum mismatch; expected %x, got %x", wantSum, gotSum)
	}
}

// TestEncodeStereoAnalysisDisabled checks that the original behavior is
// preserved when stereo analysis is disabled: the channel assignment of the
// caller is used as-is.
func TestEncodeStereoAnalysisDisabled(t *testing.T) {
	const (
		blockSize = 4096
		nblocks   = 2
		bps       = 16
	)
	nsamples := blockSize * nblocks
	left, right := stereoFixtures["in-phase"](nsamples)
	data, wantSum := encodeAudio(t, bps, blockSize, [][]int32{left, right}, false)
	modes, chans, gotSum := decodeAudio(t, data)
	for i, mode := range modes {
		if mode != frame.ChannelsLR {
			t.Errorf("frame %d: channel assignment mismatch; expected %v, got %v", i, frame.ChannelsLR, mode)
		}
	}
	if !slices.Equal(chans[0], left) {
		t.Error("left channel samples mismatch")
	}
	if !slices.Equal(chans[1], right) {
		t.Error("right channel samples mismatch")
	}
	if !bytes.Equal(gotSum, wantSum) {
		t.Errorf("MD5 checksum mismatch; expected %x, got %x", wantSum, gotSum)
	}
}

// TestEncodeStereoAnalysisNonDestructive checks that WriteFrame with stereo
// analysis enabled leaves the caller's frame (channel assignment and samples)
// unmodified.
func TestEncodeStereoAnalysisNonDestructive(t *testing.T) {
	const (
		blockSize = 4096
		bps       = 16
	)
	left, right := stereoFixtures["in-phase"](blockSize)
	info := &meta.StreamInfo{
		BlockSizeMin:  blockSize,
		BlockSizeMax:  blockSize,
		SampleRate:    44100,
		NChannels:     2,
		BitsPerSample: bps,
		NSamples:      blockSize,
	}
	buf := new(bytes.Buffer)
	enc, err := flac.NewEncoder(buf, info)
	if err != nil {
		t.Fatalf("unable to create encoder; %v", err)
	}
	enc.EnableStereoAnalysis(true)
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         blockSize,
			SampleRate:        44100,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     bps,
		},
		Subframes: []*frame.Subframe{
			{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: left, NSamples: blockSize},
			{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, Samples: right, NSamples: blockSize},
		},
	}
	wantLeft := slices.Clone(left)
	wantRight := slices.Clone(right)
	if err := enc.WriteFrame(f); err != nil {
		t.Fatalf("unable to encode audio frame; %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("unable to close encoder; %v", err)
	}
	if f.Channels != frame.ChannelsLR {
		t.Errorf("frame channel assignment modified; expected %v, got %v", frame.ChannelsLR, f.Channels)
	}
	if !slices.Equal(f.Subframes[0].Samples, wantLeft) {
		t.Error("left channel samples modified by WriteFrame")
	}
	if !slices.Equal(f.Subframes[1].Samples, wantRight) {
		t.Error("right channel samples modified by WriteFrame")
	}
}
