package flac_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/meta"
)

// twoTrackFixture returns a stereo, 16-bit fixture with two playable tracks and
// index points. The track boundaries deliberately fall in the middle of frames:
// frames are 100 samples long, track 1 covers samples [0, 250) and track 2
// covers [250, 900), so both the start of track 2 and the lead-out fall inside
// frames. The last frame is shortened (900 = 9*100), exercising a fixed
// blocksize stream whose final frame is shorter.
func twoTrackFixture() *trackFixture {
	const (
		sampleRate    = 44100
		nchannels     = 2
		bitsPerSample = 16
	)
	return &trackFixture{
		name:          "two-track",
		sampleRate:    sampleRate,
		nchannels:     nchannels,
		bitsPerSample: bitsPerSample,
		blockSizes:    []int{100, 100, 100, 100, 100, 100, 100, 100, 100},
		sample:        rampSample(sampleRate, nchannels, bitsPerSample),
		tracks: []fixtureTrack{
			{num: 1, offset: 0, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}, {Offset: 40, Num: 2}}},
			{num: 2, offset: 250, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}, {Offset: 325, Num: 2}}},
		},
		leadOut: 900,
	}
}

func TestTrackViewBasic(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)

	wantPCM, err := decodeAll(bytes.NewReader(data), 2, fixture.nchannels)
	if err != nil {
		t.Fatalf("unable to decode full stream: %v", err)
	}
	const bytesPerFrame = 2 * 2 // 16-bit stereo

	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unable to open seekable stream: %v", err)
	}
	if stream.CueSheet() == nil {
		t.Fatal("stream has no CueSheet")
	}

	testCases := []struct {
		trackNum uint8
		start    uint64
		end      uint64
	}{
		{trackNum: 1, start: 0, end: 250},
		{trackNum: 2, start: 250, end: 900},
	}

	for _, tc := range testCases {
		t.Run(string(rune('0'+tc.trackNum)), func(t *testing.T) {
			view, err := stream.OpenTrack(tc.trackNum)
			if err != nil {
				t.Fatalf("OpenTrack(%d): %v", tc.trackNum, err)
			}
			defer view.Close()

			if got := view.Start(); got != tc.start {
				t.Errorf("Start: expected %d, got %d", tc.start, got)
			}
			if got := view.End(); got != tc.end {
				t.Errorf("End: expected %d, got %d", tc.end, got)
			}
			if got, want := view.NSamples(), tc.end-tc.start; got != want {
				t.Errorf("NSamples: expected %d, got %d", want, got)
			}
			if got, want := view.Length(), int64(tc.end-tc.start)*bytesPerFrame; got != want {
				t.Errorf("Length: expected %d, got %d", want, got)
			}
			if got, want := view.Channels(), fixture.nchannels; got != want {
				t.Errorf("Channels: expected %d, got %d", want, got)
			}
			if got, want := view.BitsPerSample(), fixture.bitsPerSample; got != want {
				t.Errorf("BitsPerSample: expected %d, got %d", want, got)
			}

			// The view's bytes must be exactly the slice of the whole-stream PCM
			// that corresponds to the track.
			got, err := io.ReadAll(view)
			if err != nil {
				t.Fatalf("unable to read track view: %v", err)
			}
			want := wantPCM[tc.start*bytesPerFrame : tc.end*bytesPerFrame]
			if !bytes.Equal(got, want) {
				t.Fatalf("track %d PCM mismatch; expected %d bytes, got %d bytes", tc.trackNum, len(want), len(got))
			}
			if len(got) != int(view.Length()) {
				t.Fatalf("track %d: expected %d bytes, got %d", tc.trackNum, view.Length(), len(got))
			}

			// A read at the end of the track returns io.EOF, not data from the
			// next track.
			buf := make([]byte, 16)
			if n, err := view.Read(buf); n != 0 || err != io.EOF {
				t.Errorf("read at end of track: expected (0, io.EOF), got (%d, %v)", n, err)
			}
		})
	}

	// Adjacent views are contiguous but independent.
	v1, err := stream.OpenTrack(1)
	if err != nil {
		t.Fatal(err)
	}
	defer v1.Close()
	v2, err := stream.OpenTrack(2)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	// Interleave reads from both views to make sure each view keeps its own
	// position.
	buf1 := make([]byte, 40)
	buf2 := make([]byte, 40)
	if n, err := v1.Read(buf1); err != nil || n != len(buf1) {
		t.Fatalf("v1 read: n=%d, err=%v", n, err)
	}
	if n, err := v2.Read(buf2); err != nil || n != len(buf2) {
		t.Fatalf("v2 read: n=%d, err=%v", n, err)
	}
	if !bytes.Equal(buf1, wantPCM[0:40]) {
		t.Error("v1 returned wrong bytes after interleaved read")
	}
	if !bytes.Equal(buf2, wantPCM[250*bytesPerFrame:250*bytesPerFrame+40]) {
		t.Error("v2 returned wrong bytes after interleaved read")
	}
}

func TestTrackViewBoundaries(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	const bytesPerFrame = 4

	samples, err := decodeSamples(bytes.NewReader(data), fixture.nchannels)
	if err != nil {
		t.Fatalf("unable to decode stream: %v", err)
	}

	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// Track 2 starts in the middle of frame 2 (samples [200,300)) and ends in
	// the middle of frame 8 (samples [800,900)); track 1 ends mid-frame too.
	for _, trackNum := range []uint8{1, 2} {
		view, err := stream.OpenTrack(trackNum)
		if err != nil {
			t.Fatalf("OpenTrack(%d): %v", trackNum, err)
		}

		// Read the entire track in awkward chunk sizes that are not multiples
		// of a sample frame, exercising sub-sample positioning.
		var got []byte
		chunk := 7 // bytes; not a multiple of 4
		buf := make([]byte, chunk)
		for {
			n, err := view.Read(buf)
			got = append(got, buf[:n]...)
			if err != nil {
				if err != io.EOF {
					t.Fatalf("track %d: read error: %v", trackNum, err)
				}
				break
			}
		}
		want := flattenSamples(samples, int(view.Start()), int(view.End()), fixture.nchannels)
		if !bytes.Equal(got, want) {
			t.Fatalf("track %d: chunked read mismatch; expected %d bytes, got %d", trackNum, len(want), len(got))
		}
		if len(got) != int(view.NSamples())*bytesPerFrame {
			t.Fatalf("track %d: expected %d bytes, got %d", trackNum, view.NSamples()*bytesPerFrame, len(got))
		}

		// Verify the first and last sample values explicitly, so that an
		// off-by-one in the frame boundary logic cannot cancel out.
		if _, err := view.SeekSample(0, io.SeekStart); err != nil {
			t.Fatalf("track %d: SeekSample(0): %v", trackNum, err)
		}
		chans := make([][]int32, fixture.nchannels)
		for c := range chans {
			chans[c] = make([]int32, 1)
		}
		if n, err := view.ReadSamples(chans); err != nil || n != 1 {
			t.Fatalf("track %d: first ReadSamples: n=%d, err=%v", trackNum, n, err)
		}
		for c := 0; c < fixture.nchannels; c++ {
			if got, want := chans[c][0], samples[c][view.Start()]; got != want {
				t.Errorf("track %d: first sample channel %d: expected %d, got %d", trackNum, c, want, got)
			}
		}

		for c := range chans {
			chans[c] = make([]int32, 1)
		}
		if _, err := view.SeekSample(-1, io.SeekEnd); err != nil {
			t.Fatalf("track %d: SeekSample(-1, End): %v", trackNum, err)
		}
		if n, err := view.ReadSamples(chans); err != nil || n != 1 {
			t.Fatalf("track %d: last ReadSamples: n=%d, err=%v", trackNum, n, err)
		}
		for c := 0; c < fixture.nchannels; c++ {
			if got, want := chans[c][0], samples[c][view.End()-1]; got != want {
				t.Errorf("track %d: last sample channel %d: expected %d, got %d", trackNum, c, want, got)
			}
		}

		// The sample immediately after the track must not be reachable.
		if _, err := view.SeekSample(int64(view.NSamples()), io.SeekStart); err != nil {
			t.Fatalf("track %d: seek to end: %v", trackNum, err)
		}
		if n, err := view.ReadSamples(chans); n != 0 || err != io.EOF {
			t.Errorf("track %d: read at end of track: expected (0, io.EOF), got (%d, %v)", trackNum, n, err)
		}
		if err := view.Close(); err != nil {
			t.Fatalf("track %d: Close: %v", trackNum, err)
		}
	}
}

func TestTrackViewSeek(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	const bytesPerFrame = 4

	samples, err := decodeSamples(bytes.NewReader(data), fixture.nchannels)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	view, err := stream.OpenTrack(2)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	base := int(view.Start())

	testCases := []struct {
		name    string
		seek    int64
		whence  int
		wantPos int64 // byte position after seek
		wantErr error
	}{
		// Random seeks within the track, including the first and last sample.
		{name: "start", seek: 0, whence: io.SeekStart, wantPos: 0},
		{name: "sample 1", seek: 1, whence: io.SeekStart, wantPos: 1},
		{name: "mid", seek: 123, whence: io.SeekStart, wantPos: 123},
		{name: "boundary last", seek: 649, whence: io.SeekStart, wantPos: 649},
		{name: "end", seek: 650, whence: io.SeekStart, wantPos: 650},
		{name: "relative", seek: -50, whence: io.SeekCurrent, wantPos: 600},
		{name: "from end", seek: -1, whence: io.SeekEnd, wantPos: 649},
		{name: "past end", seek: 651, whence: io.SeekStart, wantErr: flac.ErrSeekOutOfRange},
		{name: "negative", seek: -1, whence: io.SeekStart, wantErr: flac.ErrSeekOutOfRange},
		{name: "bad whence", seek: 0, whence: 99, wantErr: nil}, // checked separately below
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "bad whence" {
				if _, err := view.Seek(tc.seek, tc.whence); err == nil {
					t.Fatal("expected an error for an invalid whence")
				}
				return
			}
			pos, err := view.Seek(tc.seek*bytesPerFrame, tc.whence)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Seek: %v", err)
			}
			if pos != tc.wantPos*bytesPerFrame {
				t.Fatalf("position: expected %d, got %d", tc.wantPos*bytesPerFrame, pos)
			}
			if got := view.SamplePos(); got != tc.wantPos {
				t.Fatalf("SamplePos: expected %d, got %d", tc.wantPos, got)
			}
			// Read a single sample frame and check that the seek landed on the
			// right audio.
			if tc.wantPos >= 650 {
				return
			}
			chans := make([][]int32, fixture.nchannels)
			for c := range chans {
				chans[c] = make([]int32, 1)
			}
			if n, err := view.ReadSamples(chans); err != nil || n != 1 {
				t.Fatalf("ReadSamples after seek: n=%d, err=%v", n, err)
			}
			for c := 0; c < fixture.nchannels; c++ {
				want := samples[c][base+int(tc.wantPos)]
				if chans[c][0] != want {
					t.Errorf("sample at %d channel %d: expected %d, got %d", tc.wantPos, c, want, chans[c][0])
				}
			}
		})
	}

	// Seek(0, io.SeekEnd) must position at end-of-track without error.
	if pos, err := view.Seek(0, io.SeekEnd); err != nil || pos != view.Length() {
		t.Fatalf("Seek(0, End): pos=%d, err=%v", pos, err)
	}
	if n, err := view.Read(make([]byte, 4)); n != 0 || err != io.EOF {
		t.Fatalf("read at end: expected (0, io.EOF), got (%d, %v)", n, err)
	}
}

func TestTrackViewReadSamples(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)

	samples, err := decodeSamples(bytes.NewReader(data), fixture.nchannels)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	view, err := stream.OpenTrack(1)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	// Read in blocks that straddle frame boundaries.
	chans := make([][]int32, fixture.nchannels)
	for c := range chans {
		chans[c] = make([]int32, 37)
	}
	nsamples := int(view.NSamples())
	read := 0
	for read < nsamples {
		n, err := view.ReadSamples(chans)
		if n > 0 {
			for c := 0; c < fixture.nchannels; c++ {
				for i := 0; i < n; i++ {
					want := samples[c][int(view.Start())+read+i]
					if chans[c][i] != want {
						t.Fatalf("sample %d channel %d: expected %d, got %d", read+i, c, want, chans[c][i])
					}
				}
			}
			read += n
		}
		if err != nil {
			if err != io.EOF {
				t.Fatalf("ReadSamples: %v", err)
			}
			break
		}
	}
	if read != nsamples {
		t.Fatalf("expected to read %d samples, got %d", nsamples, read)
	}

	// A truncated destination slice limits the read.
	if _, err := view.SeekSample(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	short := [][]int32{make([]int32, 3), make([]int32, 3)}
	if n, err := view.ReadSamples(short); err != nil || n != 3 {
		t.Fatalf("ReadSamples with 3-element slices: n=%d, err=%v", n, err)
	}
	// Different lengths request the shortest.
	uneven := [][]int32{make([]int32, 5), make([]int32, 2)}
	if n, err := view.ReadSamples(uneven); err != nil || n != 2 {
		t.Fatalf("ReadSamples with uneven slices: n=%d, err=%v", n, err)
	}
	// Wrong channel count is an error.
	if _, err := view.ReadSamples([][]int32{make([]int32, 1)}); err == nil {
		t.Fatal("expected an error for a wrong channel count")
	}
	// Reading from an unaligned byte position is an error.
	if _, err := view.Seek(1, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := view.ReadSamples(short); !errors.Is(err, flac.ErrUnalignedPosition) {
		t.Fatalf("expected ErrUnalignedPosition, got %v", err)
	}
}

func TestTrackViewIndices(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	samples, err := decodeSamples(bytes.NewReader(data), fixture.nchannels)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	view, err := stream.OpenTrack(2)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()

	indices := view.Indices()
	if len(indices) != 2 {
		t.Fatalf("expected 2 indices, got %d", len(indices))
	}
	// Index 2 of track 2 is 325 samples into the track.
	offset, ok := view.Index(2)
	if !ok || offset != 325 {
		t.Fatalf("Index(2): expected (325, true), got (%d, %v)", offset, ok)
	}
	if _, ok := view.Index(9); ok {
		t.Fatal("Index(9) unexpectedly found")
	}
	if err := view.SeekIndex(2); err != nil {
		t.Fatalf("SeekIndex(2): %v", err)
	}
	if got := view.SamplePos(); got != 325 {
		t.Fatalf("SamplePos after SeekIndex(2): expected 325, got %d", got)
	}
	chans := make([][]int32, fixture.nchannels)
	for c := range chans {
		chans[c] = make([]int32, 1)
	}
	if n, err := view.ReadSamples(chans); err != nil || n != 1 {
		t.Fatalf("ReadSamples: n=%d, err=%v", n, err)
	}
	if got, want := chans[0][0], samples[0][int(view.Start())+325]; got != want {
		t.Errorf("sample at index 2: expected %d, got %d", want, got)
	}
	if _, err := view.IndexOffset(9); !errors.Is(err, flac.ErrIndexNotFound) {
		t.Fatalf("expected ErrIndexNotFound, got %v", err)
	}
	if err := view.SeekIndex(9); !errors.Is(err, flac.ErrIndexNotFound) {
		t.Fatalf("expected ErrIndexNotFound from SeekIndex, got %v", err)
	}
}

func TestTrackViewErrors(t *testing.T) {
	// A stream without a CueSheet.
	noCue := &trackFixture{
		name:          "no-cue",
		sampleRate:    44100,
		nchannels:     2,
		bitsPerSample: 16,
		blockSizes:    []int{40, 40},
		sample:        rampSample(44100, 2, 16),
		noCueSheet:    true,
	}
	noCueData := mustEncode(noCue)
	stream, err := flac.NewSeek(bytes.NewReader(noCueData))
	if err != nil {
		t.Fatal(err)
	}
	if stream.CueSheet() != nil {
		t.Fatal("expected no CueSheet")
	}
	if _, err := stream.OpenTrack(1); !errors.Is(err, flac.ErrNoCueSheet) {
		t.Fatalf("expected ErrNoCueSheet, got %v", err)
	}
	if _, err := stream.NewTrackView(nil, 1); !errors.Is(err, flac.ErrNoCueSheet) {
		t.Fatalf("expected ErrNoCueSheet from NewTrackView(nil), got %v", err)
	}

	// A stream created with New (not NewSeek) cannot open views.
	plain, err := flac.New(bytes.NewReader(noCueData))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.OpenTrack(1); !errors.Is(err, flac.ErrNoSeeker) {
		t.Fatalf("expected ErrNoSeeker, got %v", err)
	}

	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	stream, err = flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// Unknown track number.
	if _, err := stream.OpenTrack(9); !errors.Is(err, flac.ErrTrackNotFound) {
		t.Fatalf("expected ErrTrackNotFound, got %v", err)
	}
	// The lead-out track is not playable.
	if _, err := stream.OpenTrack(255); !errors.Is(err, flac.ErrLeadOutTrack) {
		t.Fatalf("expected ErrLeadOutTrack, got %v", err)
	}
}

func TestNewTrackViewValidation(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	track := func(num uint8, offset uint64, indices ...meta.CueSheetTrackIndex) meta.CueSheetTrack {
		return meta.CueSheetTrack{Num: num, Offset: offset, IsAudio: true, Indicies: indices}
	}

	testCases := []struct {
		name string
		cs   *meta.CueSheet
		num  uint8
		want error
	}{
		{
			name: "duplicate track number",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(1, 0), track(1, 100), track(255, 200),
			}},
			num:  1,
			want: flac.ErrDuplicateTrackNum,
		},
		{
			name: "missing lead-out",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(1, 0), track(2, 100),
			}},
			num:  1,
			want: flac.ErrMissingLeadOut,
		},
		{
			name: "only lead-out",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(255, 0),
			}},
			num:  1,
			want: flac.ErrMissingLeadOut,
		},
		{
			name: "non-increasing offsets",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(1, 100), track(2, 100), track(255, 200),
			}},
			num:  1,
			want: flac.ErrInvalidCueSheet,
		},
		{
			name: "index out of bounds",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(1, 0, meta.CueSheetTrackIndex{Offset: 100, Num: 1}),
				track(2, 100), track(255, 200),
			}},
			num:  1,
			want: flac.ErrIndexOutOfBounds,
		},
		{
			name: "lead-out beyond stream",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(1, 0), track(2, 100), track(255, 100000),
			}},
			num:  1,
			want: flac.ErrInvalidCueSheet,
		},
		{
			name: "track number zero",
			cs: &meta.CueSheet{Tracks: []meta.CueSheetTrack{
				track(0, 0), track(1, 100), track(255, 200),
			}},
			num:  1,
			want: flac.ErrInvalidCueSheet,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := stream.NewTrackView(tc.cs, tc.num); !errors.Is(err, tc.want) {
				t.Fatalf("expected %v, got %v", tc.want, err)
			}
		})
	}

	// A valid external cue sheet with the same layout opens fine.
	valid := &meta.CueSheet{Tracks: []meta.CueSheetTrack{
		track(1, 0), track(2, 250), track(255, 900),
	}}
	view, err := stream.NewTrackView(valid, 1)
	if err != nil {
		t.Fatalf("NewTrackView with a valid cue sheet: %v", err)
	}
	if got, want := view.End(), uint64(250); got != want {
		t.Errorf("End: expected %d, got %d", want, got)
	}
	view.Close()
}

func TestTrackViewUnsupportedBlocking(t *testing.T) {
	// A variable-blocksize stream whose track boundaries fall in the middle of
	// frames of different sizes.
	fixture := &trackFixture{
		name:          "variable-blocksize",
		sampleRate:    48000,
		nchannels:     1,
		bitsPerSample: 16,
		blockSizes:    []int{64, 32, 48, 64, 16, 64, 32},
		sample:        rampSample(48000, 1, 16),
		tracks: []fixtureTrack{
			{num: 3, offset: 0, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}, {Offset: 70, Num: 2}}},
			{num: 7, offset: 130, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}}},
		},
		leadOut: 300,
	}
	data := mustEncode(fixture)
	wantSamples, err := decodeSamples(bytes.NewReader(data), fixture.nchannels)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		num   uint8
		start uint64
		end   uint64
	}{
		{num: 3, start: 0, end: 130},
		{num: 7, start: 130, end: 300},
	} {
		view, err := stream.OpenTrack(tc.num)
		if err != nil {
			t.Fatalf("OpenTrack(%d): %v", tc.num, err)
		}
		got, err := io.ReadAll(view)
		if err != nil {
			t.Fatalf("track %d: read: %v", tc.num, err)
		}
		want := flattenSamples(wantSamples, int(tc.start), int(tc.end), fixture.nchannels)
		if !bytes.Equal(got, want) {
			t.Fatalf("track %d: variable-blocksize PCM mismatch", tc.num)
		}
		// Seek to a sample that lies in the middle of a frame and verify.
		if _, err := view.SeekSample(int64(tc.end-tc.start-1), io.SeekStart); err != nil {
			t.Fatalf("track %d: seek: %v", tc.num, err)
		}
		chans := [][]int32{make([]int32, 1)}
		if n, err := view.ReadSamples(chans); err != nil || n != 1 {
			t.Fatalf("track %d: read: n=%d, err=%v", tc.num, n, err)
		}
		if got, want := chans[0][0], wantSamples[0][tc.end-1]; got != want {
			t.Errorf("track %d: last sample: expected %d, got %d", tc.num, want, got)
		}
		view.Close()
	}
}

func TestTrackViewClosed(t *testing.T) {
	data := mustEncode(twoTrackFixture())
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	view, err := stream.OpenTrack(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.Read(make([]byte, 1)); !errors.Is(err, flac.ErrTrackViewClosed) {
		t.Fatalf("expected ErrTrackViewClosed from Read, got %v", err)
	}
	if _, err := view.Seek(0, io.SeekStart); !errors.Is(err, flac.ErrTrackViewClosed) {
		t.Fatalf("expected ErrTrackViewClosed from Seek, got %v", err)
	}
	if err := view.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestTrackViewConcurrent verifies the documented constraint that multiple
// views of the same stream may be used concurrently: their operations are
// serialized on the shared reader.
func TestTrackViewConcurrent(t *testing.T) {
	fixture := twoTrackFixture()
	data := mustEncode(fixture)
	wantPCM, err := decodeAll(bytes.NewReader(data), 2, fixture.nchannels)
	if err != nil {
		t.Fatal(err)
	}
	const bytesPerFrame = 4

	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	type tc struct {
		num   uint8
		start uint64
		end   uint64
	}
	cases := []tc{{1, 0, 250}, {2, 250, 900}}

	errs := make(chan error, len(cases))
	for _, c := range cases {
		go func(c tc) {
			view, err := stream.OpenTrack(c.num)
			if err != nil {
				errs <- err
				return
			}
			defer view.Close()
			for iter := 0; iter < 20; iter++ {
				if _, err := view.Seek(0, io.SeekStart); err != nil {
					errs <- err
					return
				}
				got, err := io.ReadAll(view)
				if err != nil {
					errs <- err
					return
				}
				want := wantPCM[c.start*bytesPerFrame : c.end*bytesPerFrame]
				if !bytes.Equal(got, want) {
					errs <- errors.New("concurrent read mismatch")
					return
				}
			}
			errs <- nil
		}(c)
	}
	for range cases {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestTrackViewShortenedLastFrame(t *testing.T) {
	// A fixed-blocksize stream whose final frame is shorter than the rest. The
	// track boundary and the lead-out both fall inside the shortened frame.
	// Frame.SampleNumber cannot be used for this frame, so frameAt must derive
	// sample numbers by accumulating block sizes.
	const n = 40
	fixture := &trackFixture{
		name:           "shortened-last-frame",
		sampleRate:     44100,
		nchannels:      1,
		bitsPerSample:  16,
		blockSizes:     []int{n, n, n, n, 25},
		sample:         rampSample(44100, 1, 16),
		fixedBlockSize: true,
		tracks: []fixtureTrack{
			{num: 1, offset: 0, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}}},
			{num: 2, offset: 100, indices: []meta.CueSheetTrackIndex{{Offset: 0, Num: 1}}},
		},
		leadOut: 170,
	}
	data := mustEncode(fixture)
	samples, err := decodeSamples(bytes.NewReader(data), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(samples[0]), 185; got != want {
		t.Fatalf("fixture has %d samples, expected %d", got, want)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	view, err := stream.OpenTrack(2)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	if got, want := view.End(), uint64(170); got != want {
		t.Fatalf("End: expected %d, got %d", want, got)
	}
	// The track ends before the shortened frame, but the lead-out is inside it;
	// read the whole track and compare against the full decode.
	got, err := io.ReadAll(view)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, flattenSamples(samples, 100, 170, 1)) {
		t.Fatal("shortened-last-frame PCM mismatch")
	}
	// Seek to the last sample of the track, which lives in the shortened frame.
	if _, err := view.SeekSample(-1, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	chans := [][]int32{make([]int32, 1)}
	if n, err := view.ReadSamples(chans); err != nil || n != 1 {
		t.Fatalf("ReadSamples: n=%d, err=%v", n, err)
	}
	if got, want := chans[0][0], samples[0][169]; got != want {
		t.Fatalf("last sample: expected %d, got %d", want, got)
	}
}

// flattenSamples returns the interleaved, little-endian PCM for samples in
// [start, end) for 16-bit audio.
func flattenSamples(samples [][]int32, start, end, channels int) []byte {
	out := make([]byte, 0, (end-start)*channels*2)
	for i := start; i < end; i++ {
		for c := 0; c < channels; c++ {
			v := samples[c][i]
			out = append(out, byte(v), byte(v>>8))
		}
	}
	return out
}

// TestTrackViewRealFile exercises track views on a checked-in FLAC file that
// carries a CueSheet with two playable tracks and an index point, rather than
// only on synthetic fixtures.
//
// meta/testdata/input-SCPAP.flac: 5880 samples, 2 channels, 16 bits per sample;
// frames are 0x1200 (4608) samples, so the track boundary at sample 2940 falls
// in the middle of the second frame.
func TestTrackViewRealFile(t *testing.T) {
	const path = "meta/testdata/input-SCPAP.flac"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	cs := stream.CueSheet()
	if cs == nil {
		t.Fatal("expected a CueSheet in", path)
	}

	wantPCM, err := decodeAll(bytes.NewReader(data), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	const bytesPerFrame = 4

	testCases := []struct {
		num   uint8
		start uint64
		end   uint64
		// idxNum and idxOffset describe the second index point of track 1.
		idxNum    uint8
		idxOffset uint64
	}{
		{num: 1, start: 0, end: 2940, idxNum: 2, idxOffset: 588},
		{num: 2, start: 2940, end: 5880},
	}
	for _, tc := range testCases {
		t.Run(string(rune('0'+tc.num)), func(t *testing.T) {
			view, err := stream.OpenTrack(tc.num)
			if err != nil {
				t.Fatalf("OpenTrack(%d): %v", tc.num, err)
			}
			defer view.Close()

			if got := view.Start(); got != tc.start {
				t.Errorf("Start: expected %d, got %d", tc.start, got)
			}
			if got := view.End(); got != tc.end {
				t.Errorf("End: expected %d, got %d", tc.end, got)
			}
			got, err := io.ReadAll(view)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			want := wantPCM[tc.start*bytesPerFrame : tc.end*bytesPerFrame]
			if !bytes.Equal(got, want) {
				t.Fatalf("track %d PCM mismatch", tc.num)
			}

			if tc.idxNum != 0 {
				offset, ok := view.Index(tc.idxNum)
				if !ok || offset != tc.idxOffset {
					t.Fatalf("Index(%d): expected (%d, true), got (%d, %v)", tc.idxNum, tc.idxOffset, offset, ok)
				}
				if err := view.SeekIndex(tc.idxNum); err != nil {
					t.Fatalf("SeekIndex(%d): %v", tc.idxNum, err)
				}
				// Cross-check against a direct byte seek.
				if pos, err := view.Seek(int64(tc.idxOffset)*bytesPerFrame, io.SeekStart); err != nil || pos != int64(tc.idxOffset)*bytesPerFrame {
					t.Fatalf("Seek to index offset: pos=%d, err=%v", pos, err)
				}
				if err := view.SeekIndex(tc.idxNum); err != nil {
					t.Fatal(err)
				}
				chunk := make([]byte, 8)
				if n, err := io.ReadFull(view, chunk); err != nil || n != len(chunk) {
					t.Fatalf("read at index: n=%d, err=%v", n, err)
				}
				wantChunk := wantPCM[(tc.start+tc.idxOffset)*bytesPerFrame : (tc.start+tc.idxOffset)*bytesPerFrame+8]
				if !bytes.Equal(chunk, wantChunk) {
					t.Fatalf("track %d: audio at index %d mismatch", tc.num, tc.idxNum)
				}
			}

			// Randomly seek within the track and compare with the full decode.
			for _, sample := range []int64{0, 1, int64(tc.end-tc.start) / 3, int64(tc.end-tc.start) - 1} {
				if _, err := view.SeekSample(sample, io.SeekStart); err != nil {
					t.Fatalf("SeekSample(%d): %v", sample, err)
				}
				chans := [][]int32{make([]int32, 1), make([]int32, 1)}
				if n, err := view.ReadSamples(chans); err != nil || n != 1 {
					t.Fatalf("ReadSamples at %d: n=%d, err=%v", sample, n, err)
				}
				wantStart := wantPCM[(tc.start+uint64(sample))*bytesPerFrame : (tc.start+uint64(sample))*bytesPerFrame+4]
				gotStart := []byte{byte(chans[0][0]), byte(chans[0][0] >> 8), byte(chans[1][0]), byte(chans[1][0] >> 8)}
				if !bytes.Equal(gotStart, wantStart) {
					t.Fatalf("track %d sample %d mismatch: expected % X, got % X", tc.num, sample, wantStart, gotStart)
				}
			}
		})
	}

	// The lead-out track of this file uses CD-DA number 170 and is not
	// playable.
	if _, err := stream.OpenTrack(170); !errors.Is(err, flac.ErrLeadOutTrack) {
		t.Fatalf("expected ErrLeadOutTrack for 170, got %v", err)
	}
}
