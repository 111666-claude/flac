package flac_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// encodeFrames encodes the given audio frames to a synthetic in-memory FLAC
// stream.
func encodeFrames(t *testing.T, nchannels uint8, frames ...*frame.Frame) []byte {
	t.Helper()
	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  4096,
		SampleRate:    44100,
		NChannels:     nchannels,
		BitsPerSample: 16,
	}
	buf := new(bytes.Buffer)
	enc, err := flac.NewEncoder(buf, info)
	if err != nil {
		t.Fatalf("unable to create encoder; %v", err)
	}
	for _, f := range frames {
		if err := enc.WriteFrame(f); err != nil {
			t.Fatalf("unable to encode frame; %v", err)
		}
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("unable to close encoder; %v", err)
	}
	return buf.Bytes()
}

// makeFrame creates an audio frame with one subframe per channel of samples.
func makeFrame(samples ...[]int32) *frame.Frame {
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(len(samples[0])),
			SampleRate:        44100,
			Channels:          frame.Channels(len(samples) - 1),
			BitsPerSample:     16,
		},
	}
	for _, channelSamples := range samples {
		f.Subframes = append(f.Subframes, &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   channelSamples,
			NSamples:  len(channelSamples),
		})
	}
	return f
}

// silenceFrames returns n frames of blockSize silent samples per channel.
func silenceFrames(n, blockSize, nchannels int) []*frame.Frame {
	frames := make([]*frame.Frame, 0, n)
	for i := 0; i < n; i++ {
		samples := make([][]int32, nchannels)
		for channel := range samples {
			samples[channel] = make([]int32, blockSize)
		}
		frames = append(frames, makeFrame(samples...))
	}
	return frames
}

// buildPeaks builds a peak pyramid from the given FLAC stream bytes.
func buildPeaks(t *testing.T, data []byte, opts *flac.PeakOptions) *flac.PeakPyramid {
	t.Helper()
	stream, err := flac.New(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	p, err := flac.BuildPeaks(stream, opts)
	if err != nil {
		t.Fatalf("unable to build peak pyramid; %v", err)
	}
	return p
}

// windowPeak returns the min and max of channel over the given windows.
func windowPeak(windows []flac.PeakWindow, channel int) (min, max int32, ok bool) {
	ok = false
	for _, w := range windows {
		if !ok || w.Min[channel] < min {
			min = w.Min[channel]
		}
		if !ok || w.Max[channel] > max {
			max = w.Max[channel]
		}
		ok = true
	}
	return min, max, ok
}

// Stereo impulse fixture: two channels, 9192 samples in total (two full
// frames of 4096 samples and a trailing partial frame of 1000 samples), all
// silent except for a +10000 impulse at sample 1000 of the left channel and
// a -8000 impulse at sample 5000 of the right channel.
const (
	impulseNSamples = 2*4096 + 1000
	impulseLPos     = 1000
	impulseLVal     = 10000
	impulseRPos     = 5000
	impulseRVal     = -8000
)

func impulseFixture(t *testing.T) []byte {
	t.Helper()
	mk := func(start, n int) ([]int32, []int32) {
		left := make([]int32, n)
		right := make([]int32, n)
		if pos := impulseLPos - start; 0 <= pos && pos < n {
			left[pos] = impulseLVal
		}
		if pos := impulseRPos - start; 0 <= pos && pos < n {
			right[pos] = impulseRVal
		}
		return left, right
	}
	var frames []*frame.Frame
	for start := 0; start < impulseNSamples; start += 4096 {
		n := 4096
		if start+n > impulseNSamples {
			n = impulseNSamples - start
		}
		left, right := mk(start, n)
		frames = append(frames, makeFrame(left, right))
	}
	return encodeFrames(t, 2, frames...)
}

func TestBuildPeaksImpulse(t *testing.T) {
	data := impulseFixture(t)
	p := buildPeaks(t, data, &flac.PeakOptions{WindowSize: 256, MergeFactor: 4})

	if p.NSamples != impulseNSamples {
		t.Fatalf("expected %d samples, got %d", impulseNSamples, p.NSamples)
	}
	if p.NChannels != 2 {
		t.Fatalf("expected 2 channels, got %d", p.NChannels)
	}

	// Level 0: 256-sample windows; ceil(9192/256) = 36 windows, the last
	// covering the partial range [8960, 9192).
	if len(p.Levels) != 4 {
		t.Fatalf("expected 4 levels, got %d", len(p.Levels))
	}
	levelSizes := []int{36, 9, 3, 1}
	for level, want := range levelSizes {
		if got := len(p.Levels[level].Windows); got != want {
			t.Fatalf("level %d: expected %d windows, got %d", level, want, got)
		}
		wantWindowSize := uint64(256) << (2 * uint(level))
		if got := p.Levels[level].WindowSize; got != wantWindowSize {
			t.Fatalf("level %d: expected window size %d, got %d", level, wantWindowSize, got)
		}
	}

	base := p.Levels[0].Windows
	// Window boundaries are contiguous from sample 0.
	for i, w := range base {
		wantStart := uint64(i) * 256
		if w.Start != wantStart {
			t.Fatalf("base window %d: expected start %d, got %d", i, wantStart, w.Start)
		}
		wantEnd := wantStart + 256
		if i == len(base)-1 {
			wantEnd = impulseNSamples // trailing partial window
		}
		if w.End != wantEnd {
			t.Fatalf("base window %d: expected end %d, got %d", i, wantEnd, w.End)
		}
	}
	// The left impulse lands in window 1000/256 = 3.
	if w := base[3]; w.Max[0] != impulseLVal || w.Min[0] != 0 {
		t.Fatalf("base window 3: expected left min/max 0/%d, got %d/%d", impulseLVal, w.Min[0], w.Max[0])
	}
	// The right impulse lands in window 5000/256 = 19.
	if w := base[19]; w.Min[1] != impulseRVal || w.Max[1] != 0 {
		t.Fatalf("base window 19: expected right min/max %d/0, got %d/%d", impulseRVal, w.Min[1], w.Max[1])
	}

	// Each coarser level preserves the impulse peaks through merging.
	for level := 1; level < len(p.Levels); level++ {
		windows := p.Levels[level].Windows
		if min, max, _ := windowPeak(windows, 0); max != impulseLVal || min != 0 {
			t.Fatalf("level %d: expected left min/max 0/%d, got %d/%d", level, impulseLVal, min, max)
		}
		if min, max, _ := windowPeak(windows, 1); min != impulseRVal || max != 0 {
			t.Fatalf("level %d: expected right min/max %d/0, got %d/%d", level, impulseRVal, min, max)
		}
	}
	// Level 1 (1024-sample windows): left impulse in window 0, right impulse
	// in window 4.
	if w := p.Levels[1].Windows[0]; w.Max[0] != impulseLVal {
		t.Fatalf("level 1 window 0: expected left max %d, got %d", impulseLVal, w.Max[0])
	}
	if w := p.Levels[1].Windows[4]; w.Min[1] != impulseRVal {
		t.Fatalf("level 1 window 4: expected right min %d, got %d", impulseRVal, w.Min[1])
	}
	// Level 2 (4096-sample windows) align with the FLAC frame boundaries.
	if w := p.Levels[2].Windows[1]; w.Min[1] != impulseRVal {
		t.Fatalf("level 2 window 1: expected right min %d, got %d", impulseRVal, w.Min[1])
	}
}

func TestPeakQueryImpulse(t *testing.T) {
	data := impulseFixture(t)
	p := buildPeaks(t, data, &flac.PeakOptions{WindowSize: 256, MergeFactor: 4})

	// Full-range queries select coarser levels as the target width shrinks:
	// samples-per-pixel = 9192/width.
	testCases := []struct {
		width         int
		wantLevel     int
		wantWindowNum uint64 // expected number of windows
	}{
		{width: 1000, wantLevel: 0, wantWindowNum: 36}, // spp=9
		{width: 30, wantLevel: 0, wantWindowNum: 36},   // spp=306
		{width: 8, wantLevel: 1, wantWindowNum: 9},     // spp=1149
		{width: 2, wantLevel: 2, wantWindowNum: 3},     // spp=4596
		{width: 1, wantLevel: 2, wantWindowNum: 3},     // spp=9192 < 16384
	}
	for _, tc := range testCases {
		q, err := p.Query(0, impulseNSamples, tc.width)
		if err != nil {
			t.Fatalf("width %d: unable to query peaks; %v", tc.width, err)
		}
		if q.Level != tc.wantLevel {
			t.Fatalf("width %d: expected level %d, got %d", tc.width, tc.wantLevel, q.Level)
		}
		if got := uint64(len(q.Windows)); got != tc.wantWindowNum {
			t.Fatalf("width %d: expected %d windows, got %d", tc.width, tc.wantWindowNum, got)
		}
		// The windows cover the queried range and report exact boundaries.
		if q.Windows[0].Start != 0 || q.Windows[len(q.Windows)-1].End != impulseNSamples {
			t.Fatalf("width %d: expected coverage [0, %d), got [%d, %d)", tc.width, impulseNSamples, q.Windows[0].Start, q.Windows[len(q.Windows)-1].End)
		}
		// Both impulses are visible at every zoom level.
		if _, max, _ := windowPeak(q.Windows, 0); max != impulseLVal {
			t.Fatalf("width %d: expected left max %d, got %d", tc.width, impulseLVal, max)
		}
		if min, _, _ := windowPeak(q.Windows, 1); min != impulseRVal {
			t.Fatalf("width %d: expected right min %d, got %d", tc.width, impulseRVal, min)
		}
	}

	// A sub-range query returns only overlapping windows, with boundaries
	// aligned to the window grid of the selected level.
	q, err := p.Query(1000, 5001, 100)
	if err != nil {
		t.Fatalf("unable to query peaks; %v", err)
	}
	if q.Level != 0 {
		t.Fatalf("expected level 0, got %d", q.Level)
	}
	// Range [1000, 5001) overlaps base windows 3 through 19.
	if got := len(q.Windows); got != 17 {
		t.Fatalf("expected 17 windows, got %d", got)
	}
	if q.Windows[0].Start != 3*256 || q.Windows[len(q.Windows)-1].End != 20*256 {
		t.Fatalf("expected window boundaries [%d, %d), got [%d, %d)", 3*256, 20*256, q.Windows[0].Start, q.Windows[len(q.Windows)-1].End)
	}
	if _, max, _ := windowPeak(q.Windows, 0); max != impulseLVal {
		t.Fatalf("expected left max %d, got %d", impulseLVal, max)
	}
	if min, _, _ := windowPeak(q.Windows, 1); min != impulseRVal {
		t.Fatalf("expected right min %d, got %d", impulseRVal, min)
	}

	// The end of the range is clamped to the stream length.
	q, err = p.Query(9000, 1<<40, 10)
	if err != nil {
		t.Fatalf("unable to query peaks; %v", err)
	}
	if last := q.Windows[len(q.Windows)-1]; last.End != impulseNSamples {
		t.Fatalf("expected last window to end at %d, got %d", impulseNSamples, last.End)
	}

	// Invalid queries.
	if _, err := p.Query(0, impulseNSamples, 0); err == nil {
		t.Fatal("expected error for zero width")
	}
	if _, err := p.Query(5000, 5000, 10); err == nil {
		t.Fatal("expected error for empty range")
	}
	if _, err := p.Query(impulseNSamples+10, impulseNSamples+20, 10); err == nil {
		t.Fatal("expected error for range past end of stream")
	}
}

func TestBuildPeaksSilence(t *testing.T) {
	// Long silence: 20 frames of 4096 silent samples, stereo.
	frames := silenceFrames(20, 4096, 2)
	data := encodeFrames(t, 2, frames...)
	p := buildPeaks(t, data, &flac.PeakOptions{WindowSize: 512, MergeFactor: 4})

	const total = 20 * 4096
	if p.NSamples != total {
		t.Fatalf("expected %d samples, got %d", total, p.NSamples)
	}
	// 160, 40, 10, 3, 1 windows per level.
	levelSizes := []int{160, 40, 10, 3, 1}
	if len(p.Levels) != len(levelSizes) {
		t.Fatalf("expected %d levels, got %d", len(levelSizes), len(p.Levels))
	}
	for level, want := range levelSizes {
		l := p.Levels[level]
		if got := len(l.Windows); got != want {
			t.Fatalf("level %d: expected %d windows, got %d", level, want, got)
		}
		wantWindowSize := uint64(512) << (2 * uint(level))
		if l.WindowSize != wantWindowSize {
			t.Fatalf("level %d: expected window size %d, got %d", level, wantWindowSize, l.WindowSize)
		}
		for i, w := range l.Windows {
			for channel := 0; channel < 2; channel++ {
				if w.Min[channel] != 0 || w.Max[channel] != 0 {
					t.Fatalf("level %d window %d channel %d: expected silent min/max 0/0, got %d/%d", level, i, channel, w.Min[channel], w.Max[channel])
				}
			}
		}
	}
	// The last window of level 3 is partial: 81920 = 2*32768 + 16384.
	last := p.Levels[3].Windows[2]
	if last.Start != 2*32768 || last.End != total {
		t.Fatalf("expected last level 3 window [%d, %d), got [%d, %d)", 2*32768, total, last.Start, last.End)
	}

	// Query the whole silent stream.
	q, err := p.Query(0, total, 10)
	if err != nil {
		t.Fatalf("unable to query peaks; %v", err)
	}
	// spp = 8192 -> level 2 (window size 8192).
	if q.Level != 2 {
		t.Fatalf("expected level 2, got %d", q.Level)
	}
	if got := len(q.Windows); got != 10 {
		t.Fatalf("expected 10 windows, got %d", got)
	}
}

func TestBuildPeaksEmptyStream(t *testing.T) {
	data := encodeFrames(t, 2)
	p := buildPeaks(t, data, nil)
	if p.NSamples != 0 {
		t.Fatalf("expected 0 samples, got %d", p.NSamples)
	}
	if len(p.Levels) != 0 {
		t.Fatalf("expected no levels, got %d", len(p.Levels))
	}
	if _, err := p.Query(0, 100, 10); !errors.Is(err, flac.ErrNoPeaks) {
		t.Fatalf("expected ErrNoPeaks, got %v", err)
	}
}

func TestBuildPeaksBadFrame(t *testing.T) {
	data := impulseFixture(t)
	// Corrupt the last byte of the stream, breaking the CRC-16 checksum of
	// the final frame. The first two frames (8192 samples) remain intact.
	corrupt := make([]byte, len(data))
	copy(corrupt, data)
	corrupt[len(corrupt)-1] ^= 0xFF

	stream, err := flac.New(bytes.NewReader(corrupt))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	p, err := flac.BuildPeaks(stream, &flac.PeakOptions{WindowSize: 256, MergeFactor: 4})
	var buildErr *flac.PeakBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("expected *flac.PeakBuildError, got %v", err)
	}
	// The partial pyramid holds the samples decoded before the corrupt
	// frame.
	if p == nil || p.NSamples != 8192 {
		t.Fatalf("expected partial pyramid with 8192 samples, got %d", p.NSamples)
	}
	if buildErr.SampleNum != p.NSamples {
		t.Fatalf("expected error sample number %d, got %d", p.NSamples, buildErr.SampleNum)
	}
	if len(p.Levels) == 0 {
		t.Fatal("expected partial pyramid to hold levels")
	}
	// The intact frames contributed both impulses.
	if _, max, _ := windowPeak(p.Levels[0].Windows, 0); max != impulseLVal {
		t.Fatalf("expected left max %d in partial pyramid, got %d", impulseLVal, max)
	}
	if min, _, _ := windowPeak(p.Levels[0].Windows, 1); min != impulseRVal {
		t.Fatalf("expected right min %d in partial pyramid, got %d", impulseRVal, min)
	}
}

func TestBuildPeaksWindowLimit(t *testing.T) {
	data := impulseFixture(t)
	stream, err := flac.New(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	// The base level alone requires 36 windows of 256 samples.
	_, err = flac.BuildPeaks(stream, &flac.PeakOptions{WindowSize: 256, MaxWindows: 10})
	if !errors.Is(err, flac.ErrTooManyWindows) {
		t.Fatalf("expected ErrTooManyWindows, got %v", err)
	}
}

func TestBuildPeaksInvalidOptions(t *testing.T) {
	data := impulseFixture(t)
	stream, err := flac.New(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	if _, err := flac.BuildPeaks(stream, &flac.PeakOptions{MergeFactor: 1}); err == nil {
		t.Fatal("expected error for merge factor < 2")
	}
}

func TestBuildPeaksMultiChannel(t *testing.T) {
	// Three channels with distinct constant values per channel.
	const n = 5000
	left := make([]int32, n)
	center := make([]int32, n)
	right := make([]int32, n)
	for i := range left {
		left[i] = -100
		center[i] = 42
		right[i] = 300
	}
	right[n-1] = -7 // trailing partial window of the base level
	data := encodeFrames(t, 3, makeFrame(left, center, right))
	p := buildPeaks(t, data, &flac.PeakOptions{WindowSize: 4096, MergeFactor: 2})

	if p.NChannels != 3 {
		t.Fatalf("expected 3 channels, got %d", p.NChannels)
	}
	// Base level: a full window and a trailing partial window.
	base := p.Levels[0].Windows
	if len(base) != 2 {
		t.Fatalf("expected 2 base windows, got %d", len(base))
	}
	if base[1].Start != 4096 || base[1].End != n {
		t.Fatalf("expected partial window [4096, %d), got [%d, %d)", n, base[1].Start, base[1].End)
	}
	// Merged level: a single window spanning the whole stream.
	merged := p.Levels[1].Windows
	if len(merged) != 1 || merged[0].Start != 0 || merged[0].End != n {
		t.Fatalf("expected single merged window [0, %d), got %+v", n, merged)
	}
	w := merged[0]
	wantMin := []int32{-100, 42, -7}
	wantMax := []int32{-100, 42, 300}
	for channel := 0; channel < 3; channel++ {
		if w.Min[channel] != wantMin[channel] || w.Max[channel] != wantMax[channel] {
			t.Fatalf("channel %d: expected min/max %d/%d, got %d/%d", channel, wantMin[channel], wantMax[channel], w.Min[channel], w.Max[channel])
		}
	}
}

func TestBuildPeaksChannelCountChange(t *testing.T) {
	// Concatenate the frames of a stereo stream and a mono stream. The
	// metadata of the mono stream is stripped (4-byte signature plus 38-byte
	// StreamInfo block), so its frames follow the stereo frames directly.
	stereo := encodeFrames(t, 2, silenceFrames(1, 1024, 2)...)
	mono := encodeFrames(t, 1, silenceFrames(1, 1024, 1)...)
	const streamInfoLen = 4 + 4 + 34 // signature + block header + StreamInfo body
	data := append(stereo, mono[streamInfoLen:]...)

	stream, err := flac.New(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	_, err = flac.BuildPeaks(stream, nil)
	if err == nil {
		t.Fatal("expected error for channel count change")
	}
}

// TestBuildPeaksMatchesDecode verifies that the peaks of the pyramid match an
// independent decode pass over the same FLAC stream, i.e. that building the
// pyramid observes the unmodified PCM samples of the stream.
func TestBuildPeaksMatchesDecode(t *testing.T) {
	data := impulseFixture(t)
	p := buildPeaks(t, data, &flac.PeakOptions{WindowSize: 256, MergeFactor: 4})

	// Independent decode pass.
	stream, err := flac.New(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to parse FLAC stream; %v", err)
	}
	var samples [2][]int32
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unable to parse frame; %v", err)
		}
		for channel, sub := range f.Subframes {
			samples[channel] = append(samples[channel], sub.Samples...)
		}
	}
	if got := len(samples[0]); got != impulseNSamples {
		t.Fatalf("expected %d decoded samples, got %d", impulseNSamples, got)
	}
	// Every base window reports the exact min/max of the decoded samples.
	for i, w := range p.Levels[0].Windows {
		for channel := 0; channel < 2; channel++ {
			wantMin, wantMax := int32(0), int32(0)
			for _, sample := range samples[channel][w.Start:w.End] {
				if sample < wantMin {
					wantMin = sample
				}
				if sample > wantMax {
					wantMax = sample
				}
			}
			if w.Min[channel] != wantMin || w.Max[channel] != wantMax {
				t.Fatalf("window %d channel %d: expected min/max %d/%d, got %d/%d", i, channel, wantMin, wantMax, w.Min[channel], w.Max[channel])
			}
		}
	}
}
