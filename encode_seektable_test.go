package flac_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// memWriteSeeker is an in-memory io.WriteSeeker used to exercise seek table
// generation. It can be configured to fail writes or seeks.
type memWriteSeeker struct {
	buf []byte
	pos int64
	// failWriteAt, when >= 0, causes a write covering that offset to fail.
	failWriteAt int64
	// failWriteRange, when non-empty, causes writes overlapping it to fail.
	failWriteRange []int64
	// failSeek causes Seek to fail.
	failSeek bool
}

func newMemWriteSeeker() *memWriteSeeker {
	return &memWriteSeeker{failWriteAt: -1}
}

func (w *memWriteSeeker) Write(p []byte) (int, error) {
	if w.failWriteAt >= 0 && w.pos <= w.failWriteAt && w.failWriteAt < w.pos+int64(len(p)) {
		return 0, errors.New("memWriteSeeker: configured write failure")
	}
	if len(w.failWriteRange) == 2 {
		end := w.pos + int64(len(p))
		if w.pos < w.failWriteRange[1] && end > w.failWriteRange[0] {
			return 0, errors.New("memWriteSeeker: configured write failure")
		}
	}
	end := w.pos + int64(len(p))
	if end > int64(len(w.buf)) {
		w.buf = append(w.buf, make([]byte, end-int64(len(w.buf)))...)
	}
	copy(w.buf[w.pos:], p)
	w.pos = end
	return len(p), nil
}

func (w *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	if w.failSeek {
		return 0, errors.New("memWriteSeeker: configured seek failure")
	}
	var pos int64
	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = w.pos + offset
	case io.SeekEnd:
		pos = int64(len(w.buf)) + offset
	default:
		return 0, errors.New("memWriteSeeker: invalid whence")
	}
	if pos < 0 {
		return 0, errors.New("memWriteSeeker: negative position")
	}
	w.pos = pos
	return pos, nil
}

// syntheticInfo returns a StreamInfo for the given block sizes.
func syntheticInfo(blockSizes []int) *meta.StreamInfo {
	total := 0
	for _, n := range blockSizes {
		total += n
	}
	return &meta.StreamInfo{
		BlockSizeMin:  uint16(blockSizes[0]),
		BlockSizeMax:  uint16(blockSizes[0]),
		SampleRate:    44100,
		NChannels:     2,
		BitsPerSample: 16,
		NSamples:      uint64(total),
	}
}

// encodeSynthetic encodes a synthetic sine wave with one frame per block size,
// optionally enabling seek table generation with maxPoints points.
func encodeSynthetic(t *testing.T, blockSizes []int, maxPoints int) []byte {
	t.Helper()
	ws := newMemWriteSeeker()
	encodeSyntheticTo(t, ws, blockSizes, maxPoints)
	return ws.buf
}

func encodeSyntheticTo(t *testing.T, w io.Writer, blockSizes []int, maxPoints int) {
	t.Helper()
	const (
		sampleRate    = 44100
		nchannels     = 2
		bitsPerSample = 16
	)
	info := syntheticInfo(blockSizes)
	enc, err := flac.NewEncoder(w, info)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if maxPoints > 0 {
		if err := enc.EnableSeekTable(maxPoints); err != nil {
			t.Fatalf("EnableSeekTable: %v", err)
		}
	}
	enc.EnablePredictionAnalysis(false)

	// Vary the blocking strategy along with the block size: fixed-block-size
	// streams require a constant block size, so use variable block sizes when
	// the block sizes differ.
	fixed := true
	for _, n := range blockSizes[1:] {
		if n != blockSizes[0] {
			fixed = false
			break
		}
	}

	offset := 0
	for _, blockSize := range blockSizes {
		f := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: fixed,
			BlockSize:         uint16(blockSize),
			SampleRate:        sampleRate,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     bitsPerSample,
		}}
		f.Subframes = make([]*frame.Subframe, nchannels)
		for channel := 0; channel < nchannels; channel++ {
			samples := make([]int32, blockSize)
			for i := range samples {
				v := math.Sin(2 * math.Pi * 440 * float64(offset+i) / sampleRate)
				samples[i] = int32(v*30000) + int32(channel)*100
			}
			f.Subframes[channel] = &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   samples,
				NSamples:  blockSize,
			}
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		offset += blockSize
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// seekTableOf returns the seek table block of the given FLAC stream, or nil.
func seekTableOf(t *testing.T, r io.Reader) *meta.SeekTable {
	t.Helper()
	stream, err := flac.Parse(r)
	if err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
	var table *meta.SeekTable
	for _, block := range stream.Blocks {
		if st, ok := block.Body.(*meta.SeekTable); ok {
			if table != nil {
				t.Fatalf("multiple seek table blocks")
			}
			table = st
		}
	}
	return table
}

// dataStartOf returns the offset of the first audio frame header of data.
func dataStartOf(t *testing.T, data []byte) int64 {
	t.Helper()
	if len(data) < 4 || !bytes.Equal(data[:4], []byte("fLaC")) {
		t.Fatalf("invalid FLAC signature")
	}
	off := int64(4)
	for {
		if off+4 > int64(len(data)) {
			t.Fatalf("truncated metadata block header at offset %d", off)
		}
		hdr := data[off]
		length := int64(data[off+1])<<16 | int64(data[off+2])<<8 | int64(data[off+3])
		off += 4 + length
		if off > int64(len(data)) {
			t.Fatalf("metadata block body exceeds stream length")
		}
		if hdr&0x80 != 0 {
			return off
		}
	}
}

// framePos describes the position and size of an encoded audio frame.
type framePos struct {
	SampleNum uint64
	Offset    int64
	NSamples  uint16
}

// walkFrames decodes the frame headers and bodies of data and returns the
// relative offset, first sample number and block size of every frame.
func walkFrames(t *testing.T, data []byte) (dataStart int64, frames []framePos) {
	t.Helper()
	dataStart = dataStartOf(t, data)
	var sampleNum uint64
	off := dataStart
	for off < int64(len(data)) {
		rs := bytes.NewReader(data[off:])
		f, err := frame.Parse(rs)
		if err != nil {
			t.Fatalf("frame.Parse at offset %d: %v", off, err)
		}
		consumed := int64(len(data[off:])) - int64(rs.Len())
		if consumed <= 0 {
			t.Fatalf("frame at offset %d consumed %d bytes", off, consumed)
		}
		frames = append(frames, framePos{
			SampleNum: sampleNum,
			Offset:    off - dataStart,
			NSamples:  f.BlockSize,
		})
		sampleNum += uint64(f.BlockSize)
		off += consumed
	}
	return dataStart, frames
}

// checkSeekTable verifies the structural invariants of a generated seek table
// and returns its non-placeholder points.
func checkSeekTable(t *testing.T, table *meta.SeekTable, maxPoints int) []meta.SeekPoint {
	t.Helper()
	if table == nil {
		t.Fatalf("no seek table generated")
	}
	if got, want := len(table.Points), maxPoints; got != want {
		t.Fatalf("number of seek points; expected %d, got %d", want, got)
	}
	var real []meta.SeekPoint
	var prev uint64
	seenPlaceholder := false
	for i, p := range table.Points {
		if p.SampleNum == meta.PlaceholderPoint {
			seenPlaceholder = true
			if p.Offset != 0 || p.NSamples != 0 {
				t.Errorf("point %d: non-zero placeholder fields; %+v", i, p)
			}
			continue
		}
		if seenPlaceholder {
			t.Errorf("point %d: real point follows a placeholder point", i)
		}
		if len(real) > 0 && p.SampleNum <= prev {
			t.Errorf("point %d: sample numbers not strictly ascending; %d <= %d", i, p.SampleNum, prev)
		}
		if p.NSamples == 0 {
			t.Errorf("point %d: zero samples; %+v", i, p)
		}
		prev = p.SampleNum
		real = append(real, p)
	}
	if len(real) == 0 {
		t.Fatalf("no non-placeholder seek points")
	}
	if real[0].SampleNum != 0 {
		t.Errorf("first seek point sample number; expected 0, got %d", real[0].SampleNum)
	}
	return real
}

// checkSeekPoints verifies that every seek point locates the frame header of
// the sample number it describes.
func checkSeekPoints(t *testing.T, data []byte, points []meta.SeekPoint, frames []framePos, dataStart int64) {
	t.Helper()
	bySample := make(map[uint64]framePos, len(frames))
	for _, f := range frames {
		if _, ok := bySample[f.SampleNum]; ok {
			t.Fatalf("duplicate frame sample number %d", f.SampleNum)
		}
		bySample[f.SampleNum] = f
	}
	var prevOffset int64 = -1
	for i, p := range points {
		f, ok := bySample[p.SampleNum]
		if !ok {
			t.Errorf("point %d: sample number %d does not match a frame start", i, p.SampleNum)
			continue
		}
		if p.Offset != uint64(f.Offset) {
			t.Errorf("point %d (sample %d): offset; expected %d, got %d", i, p.SampleNum, f.Offset, p.Offset)
		}
		if p.NSamples != f.NSamples {
			t.Errorf("point %d (sample %d): number of samples; expected %d, got %d", i, p.SampleNum, f.NSamples, p.NSamples)
		}
		if int64(p.Offset) <= prevOffset {
			t.Errorf("point %d: offsets not strictly ascending; %d <= %d", i, p.Offset, prevOffset)
		}
		prevOffset = int64(p.Offset)
		// The offset must point at the start of a frame header, i.e. the
		// 14-bit sync code 0b11111111111110 followed by a zero reserved bit.
		off := dataStart + int64(p.Offset)
		if off+1 >= int64(len(data)) {
			t.Errorf("point %d: offset %d beyond end of stream", i, p.Offset)
			continue
		}
		if data[off] != 0xFF || data[off+1]&0xFE != 0xF8 {
			t.Errorf("point %d at offset %d: no frame header sync code; got % X", i, p.Offset, data[off:off+2])
		}
	}
}

// TestEncodeSeekTable verifies that generated seek tables index the actual
// frames of streams with different block sizes and blocking strategies.
func TestEncodeSeekTable(t *testing.T) {
	tests := []struct {
		name       string
		blockSizes []int
		maxPoints  int
	}{
		{name: "fixed-4096", blockSizes: repeat(4096, 10), maxPoints: 100},
		{name: "fixed-4608", blockSizes: repeat(4608, 5), maxPoints: 50},
		{name: "blocksize-16", blockSizes: repeat(16, 40), maxPoints: 100},
		{name: "blocksize-192", blockSizes: repeat(192, 12), maxPoints: 100},
		{name: "exact-points", blockSizes: repeat(4096, 4), maxPoints: 4},
		{name: "single-point", blockSizes: repeat(4096, 6), maxPoints: 1},
		{name: "short-final-frame", blockSizes: []int{4096, 4096, 1000}, maxPoints: 10},
		{name: "variable", blockSizes: []int{192, 576, 725, 1000, 4096, 32, 2304, 16}, maxPoints: 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := encodeSynthetic(t, tt.blockSizes, tt.maxPoints)
			table := seekTableOf(t, bytes.NewReader(data))
			points := checkSeekTable(t, table, tt.maxPoints)
			dataStart, frames := walkFrames(t, data)
			checkSeekPoints(t, data, points, frames, dataStart)

			wantReal := len(tt.blockSizes)
			if wantReal > tt.maxPoints {
				wantReal = tt.maxPoints
			}
			if len(points) > wantReal {
				t.Errorf("number of non-placeholder points; expected <= %d, got %d", wantReal, len(points))
			}
		})
	}
}

// TestEncodeSeekTableThinning verifies that more frames than points yields a
// bounded table that still starts at sample zero and stays in ascending order.
func TestEncodeSeekTableThinning(t *testing.T) {
	for _, maxPoints := range []int{2, 3, 5, 7} {
		t.Run("", func(t *testing.T) {
			const nframes = 40
			blockSizes := repeat(256, nframes)
			data := encodeSynthetic(t, blockSizes, maxPoints)
			table := seekTableOf(t, bytes.NewReader(data))
			points := checkSeekTable(t, table, maxPoints)
			dataStart, frames := walkFrames(t, data)
			checkSeekPoints(t, data, points, frames, dataStart)
			if len(points) > maxPoints {
				t.Errorf("number of non-placeholder points; expected <= %d, got %d", maxPoints, len(points))
			}
		})
	}
}

// TestEncodeSeekTableSeek verifies that the generated table allows the
// streaming seek API to locate the frame containing arbitrary sample numbers.
func TestEncodeSeekTableSeek(t *testing.T) {
	blockSizes := []int{4096, 2048, 1000, 4096, 17, 4096, 512}
	data := encodeSynthetic(t, blockSizes, 100)
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("flac.NewSeek: %v", err)
	}
	nsamples := stream.Info.NSamples
	if nsamples == 0 {
		t.Fatalf("NSamples was not backfilled")
	}
	for _, target := range []uint64{0, 17, 4095, 4096, 12000, nsamples / 2, nsamples - 1} {
		got, err := stream.Seek(target)
		if err != nil {
			t.Fatalf("Seek(%d): %v", target, err)
		}
		f, err := stream.ParseNext()
		if err != nil {
			t.Fatalf("ParseNext after Seek(%d): %v", target, err)
		}
		if f.SampleNumber() != got {
			t.Errorf("Seek(%d): frame sample number %d does not match returned value %d", target, f.SampleNumber(), got)
		}
		if got > target || target >= got+uint64(f.BlockSize) {
			t.Errorf("Seek(%d): returned frame [%d, %d) does not contain the target", target, got, got+uint64(f.BlockSize))
		}
	}
}

// TestEncodeSeekTableStreamInfo verifies that the StreamInfo backfill is
// unaffected and that caller-provided metadata blocks are preserved.
func TestEncodeSeekTableStreamInfo(t *testing.T) {
	ws := newMemWriteSeeker()
	blockSizes := []int{4096, 4096, 1000}
	comment := &meta.Block{
		Header: meta.Header{Type: meta.TypeVorbisComment, Length: 1},
		Body:   &meta.VorbisComment{Vendor: "seek table test", Tags: [][2]string{{"TITLE", "demo"}}},
	}
	padding := &meta.Block{
		Header: meta.Header{Type: meta.TypePadding, Length: 8},
	}
	// Determine the encoded lengths by encoding the blocks on their own.
	info := syntheticInfo(blockSizes)
	enc, err := flac.NewEncoder(ws, info, comment, padding)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	enc.EnablePredictionAnalysis(false)
	total := 0
	for _, blockSize := range blockSizes {
		f := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: false,
			BlockSize:         uint16(blockSize),
			SampleRate:        44100,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     16,
		}}
		f.Subframes = make([]*frame.Subframe, 2)
		for ch := 0; ch < 2; ch++ {
			f.Subframes[ch] = &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredConstant},
				Samples:   make([]int32, blockSize),
				NSamples:  blockSize,
			}
		}
		if err := enc.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		total += blockSize
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stream, err := flac.Parse(bytes.NewReader(ws.buf))
	if err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
	if got, want := stream.Info.NSamples, uint64(total); got != want {
		t.Errorf("NSamples; expected %d, got %d", want, got)
	}
	if got, want := stream.Info.BlockSizeMin, uint16(1000); got != want {
		t.Errorf("BlockSizeMin; expected %d, got %d", want, got)
	}
	if got, want := stream.Info.BlockSizeMax, uint16(4096); got != want {
		t.Errorf("BlockSizeMax; expected %d, got %d", want, got)
	}
	if stream.Info.MD5sum == ([16]byte{}) {
		t.Errorf("MD5 checksum was not backfilled")
	}
	var gotComment *meta.VorbisComment
	var gotPadding bool
	var table *meta.SeekTable
	var tableIndex, nblocks int
	for i, block := range stream.Blocks {
		nblocks++
		switch body := block.Body.(type) {
		case *meta.VorbisComment:
			gotComment = body
		case *meta.SeekTable:
			table = body
			tableIndex = i
		case nil:
			// Padding blocks have no parsed body; identify them by type.
			if block.Type == meta.TypePadding {
				gotPadding = true
			}
		}
	}
	if gotComment == nil {
		t.Fatalf("Vorbis comment block was dropped")
	}
	if gotComment.Vendor != "seek table test" || len(gotComment.Tags) != 1 || gotComment.Tags[0] != [2]string{"TITLE", "demo"} {
		t.Errorf("Vorbis comment was modified; got %+v", gotComment)
	}
	if !gotPadding {
		t.Errorf("Padding block was dropped")
	}
	if table == nil {
		t.Fatalf("no seek table generated")
	}
	if tableIndex != nblocks-1 {
		t.Errorf("seek table is block %d of %d; expected it to be last", tableIndex, nblocks)
	}
}

// testInfo returns a minimal StreamInfo with valid block sizes.
func testInfo() *meta.StreamInfo {
	return &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  16,
		SampleRate:    44100,
		NChannels:     1,
		BitsPerSample: 16,
	}
}

// TestEnableSeekTableNotSeekable verifies that seek table generation is
// refused for a writer that cannot be rewound, and that encoding continues
// normally without a seek table.
func TestEnableSeekTableNotSeekable(t *testing.T) {
	out := new(bytes.Buffer)
	info := testInfo()
	info.NChannels = 1
	enc, err := flac.NewEncoder(out, info)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); !errors.Is(err, flac.ErrSeekTableNotSeekable) {
		t.Fatalf("EnableSeekTable error; expected %v, got %v", flac.ErrSeekTableNotSeekable, err)
	}
	enc.EnablePredictionAnalysis(false)
	if err := enc.WriteFrame(monoFrame(16)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if table := seekTableOf(t, bytes.NewReader(out.Bytes())); table != nil {
		t.Errorf("seek table was generated for a non-seekable writer")
	}
}

// TestEncodeSeekTableNonZeroStart verifies that seek points are relative to
// the first frame of the FLAC stream, even when the writer is positioned at a
// non-zero offset of its underlying stream.
func TestEncodeSeekTableNonZeroStart(t *testing.T) {
	const prefix = "ID3-prefix-of-sorts"
	ws := newMemWriteSeeker()
	ws.buf = append(ws.buf, make([]byte, len(prefix))...)
	copy(ws.buf, prefix)
	ws.pos = int64(len(prefix))

	encodeSyntheticTo(t, ws, []int{1000, 1000, 300}, 10)
	data := ws.buf[len(prefix):]
	if string(ws.buf[:len(prefix)]) != prefix {
		t.Errorf("bytes preceding the FLAC stream were overwritten")
	}
	stream, err := flac.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
	if got, want := stream.Info.NSamples, uint64(2300); got != want {
		t.Errorf("NSamples; expected %d, got %d", want, got)
	}
	table := seekTableOf(t, bytes.NewReader(data))
	points := checkSeekTable(t, table, 10)
	dataStart, frames := walkFrames(t, data)
	checkSeekPoints(t, data, points, frames, dataStart)
}

// TestEnableSeekTableInvalidPoints verifies that non-positive and excessive
// numbers of seek points are rejected, and that the encoder stays usable.
func TestEnableSeekTableInvalidPoints(t *testing.T) {
	for _, n := range []int{0, -1, math.MaxInt32, 1 << 24, 932068} {
		ws := newMemWriteSeeker()
		enc, err := flac.NewEncoder(ws, testInfo())
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		if err := enc.EnableSeekTable(n); !errors.Is(err, flac.ErrInvalidSeekTablePoints) {
			t.Errorf("EnableSeekTable(%d) error; expected %v, got %v", n, flac.ErrInvalidSeekTablePoints, err)
		}
		if err := enc.Close(); err != nil {
			t.Errorf("Close after rejected EnableSeekTable(%d): %v", n, err)
		}
	}

	// The largest number of points that fits in a 24-bit block length is
	// accepted.
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(932067); err != nil {
		t.Errorf("EnableSeekTable(932067): %v", err)
	}
	if err := enc.Close(); !errors.Is(err, flac.ErrEmptyAudioStream) {
		t.Errorf("Close: expected %v, got %v", flac.ErrEmptyAudioStream, err)
	}
}

// TestEnableSeekTableTwice verifies that seek table generation can only be
// enabled once.
func TestEnableSeekTableTwice(t *testing.T) {
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	if err := enc.EnableSeekTable(20); !errors.Is(err, flac.ErrSeekTableAlreadyEnabled) {
		t.Fatalf("EnableSeekTable error; expected %v, got %v", flac.ErrSeekTableAlreadyEnabled, err)
	}
}

// TestEnableSeekTableTooLate verifies that seek table generation cannot be
// enabled after audio frames have been written.
func TestEnableSeekTableTooLate(t *testing.T) {
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	enc.EnablePredictionAnalysis(false)
	if err := enc.WriteFrame(monoFrame(16)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := enc.EnableSeekTable(10); !errors.Is(err, flac.ErrSeekTableTooLate) {
		t.Fatalf("EnableSeekTable error; expected %v, got %v", flac.ErrSeekTableTooLate, err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestEnableSeekTableExisting verifies that a caller-provided SeekTable block
// is neither overwritten nor duplicated.
func TestEnableSeekTableExisting(t *testing.T) {
	ws := newMemWriteSeeker()
	existing := &meta.Block{
		Header: meta.Header{Type: meta.TypeSeekTable, Length: 2 * 18},
		Body: &meta.SeekTable{Points: []meta.SeekPoint{
			{SampleNum: 0, Offset: 0, NSamples: 16},
			{SampleNum: meta.PlaceholderPoint},
		}},
	}
	enc, err := flac.NewEncoder(ws, testInfo(), existing)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); !errors.Is(err, flac.ErrSeekTableExists) {
		t.Fatalf("EnableSeekTable error; expected %v, got %v", flac.ErrSeekTableExists, err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	table := seekTableOf(t, bytes.NewReader(ws.buf))
	if table == nil {
		t.Fatalf("caller-provided seek table was dropped")
	}
	if got, want := len(table.Points), 2; got != want {
		t.Fatalf("number of seek points; expected %d, got %d", want, got)
	}
	if table.Points[0].NSamples != 16 || table.Points[1].SampleNum != meta.PlaceholderPoint {
		t.Fatalf("caller-provided seek table was overwritten; got %+v", table.Points)
	}
}

// TestEncodeSeekTableEmptyStream verifies that an empty audio stream reports a
// clear error and never produces a table with bogus index points.
func TestEncodeSeekTableEmptyStream(t *testing.T) {
	ws := newMemWriteSeeker()
	info := &meta.StreamInfo{
		BlockSizeMin:  16,
		BlockSizeMax:  16,
		SampleRate:    44100,
		NChannels:     1,
		BitsPerSample: 16,
	}
	enc, err := flac.NewEncoder(ws, info)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	if err := enc.Close(); !errors.Is(err, flac.ErrEmptyAudioStream) {
		t.Fatalf("Close error; expected %v, got %v", flac.ErrEmptyAudioStream, err)
	}
	// The metadata section must still be parseable, and the reserved points
	// must all be placeholders.
	stream, err := flac.Parse(bytes.NewReader(ws.buf))
	if err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
	if stream.Info.BlockSizeMin < 16 || stream.Info.BlockSizeMax < 16 {
		t.Errorf("invalid StreamInfo block sizes; got min %d, max %d", stream.Info.BlockSizeMin, stream.Info.BlockSizeMax)
	}
	for _, block := range stream.Blocks {
		if st, ok := block.Body.(*meta.SeekTable); ok {
			for i, p := range st.Points {
				if p.SampleNum != meta.PlaceholderPoint {
					t.Errorf("point %d: expected a placeholder point, got %+v", i, p)
				}
			}
		}
	}
}

// TestEncodeSeekTableWriteFailure verifies that a write failure during frame
// encoding is reported by WriteFrame and Close, and that no seek table with
// bogus offsets is produced.
func TestEncodeSeekTableWriteFailure(t *testing.T) {
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	enc.EnablePredictionAnalysis(false)
	// Fail while writing the first frame, after the metadata section.
	ws.failWriteAt = int64(len(ws.buf)) + 8
	if err := enc.WriteFrame(monoFrame(32)); err == nil {
		t.Fatalf("WriteFrame: expected write error, got nil")
	}
	// The error is sticky.
	if err := enc.WriteFrame(monoFrame(32)); err == nil {
		t.Fatalf("WriteFrame after failure: expected write error, got nil")
	}
	if err := enc.Close(); err == nil {
		t.Fatalf("Close after write failure: expected error, got nil")
	}
	// The reserved seek table must not claim to index any frames.
	table := seekTableOf(t, bytes.NewReader(ws.buf))
	if table == nil {
		t.Fatalf("reserved seek table block is missing")
	}
	for i, p := range table.Points {
		if p.SampleNum != meta.PlaceholderPoint {
			t.Errorf("point %d: bogus index point after write failure; %+v", i, p)
		}
	}
}

// TestEncodeSeekTableBackfillFailure verifies that a seek failure while
// filling in the seek table is reported by Close.
func TestEncodeSeekTableBackfillFailure(t *testing.T) {
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.EnableSeekTable(10); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	enc.EnablePredictionAnalysis(false)
	if err := enc.WriteFrame(monoFrame(32)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	ws.failSeek = true
	if err := enc.Close(); err == nil {
		t.Fatalf("Close after seek failure: expected error, got nil")
	}
	// Close is idempotent and keeps reporting the failure.
	ws.failSeek = false
	if err := enc.Close(); err == nil {
		t.Fatalf("second Close: expected the first error, got nil")
	}
}

// TestEncodeSeekTableWriteBackfillFailure verifies that a write failure while
// filling in the seek table is reported by Close.
func TestEncodeSeekTableWriteBackfillFailure(t *testing.T) {
	ws := newMemWriteSeeker()
	enc, err := flac.NewEncoder(ws, testInfo())
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	const maxPoints = 4
	if err := enc.EnableSeekTable(maxPoints); err != nil {
		t.Fatalf("EnableSeekTable: %v", err)
	}
	enc.EnablePredictionAnalysis(false)
	if err := enc.WriteFrame(monoFrame(32)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	// Seek table body: signature (4) + StreamInfo block (4+34) + block header
	// (4). Only fail writes inside the seek table body, not the StreamInfo
	// backfill.
	tableStart := int64(4 + 4 + 34 + 4)
	ws.failWriteRange = []int64{tableStart, tableStart + maxPoints*18}
	if err := enc.Close(); err == nil {
		t.Fatalf("Close after seek table write failure: expected error, got nil")
	}
}

// TestEncodeWithoutSeekTable verifies that the encoder does not add a seek
// table unless requested, and that ordinary encoding still round-trips.
func TestEncodeWithoutSeekTable(t *testing.T) {
	data := encodeSynthetic(t, []int{1024, 1024, 1024}, 0)
	if table := seekTableOf(t, bytes.NewReader(data)); table != nil {
		t.Fatalf("unexpected seek table in stream encoded without seek table generation")
	}
	if _, err := flac.Parse(bytes.NewReader(data)); err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
}

// monoFrame returns a mono verbatim frame with n zero samples.
func monoFrame(n int) *frame.Frame {
	f := &frame.Frame{Header: frame.Header{
		HasFixedBlockSize: true,
		BlockSize:         uint16(n),
		SampleRate:        44100,
		Channels:          frame.ChannelsMono,
		BitsPerSample:     16,
	}}
	f.Subframes = []*frame.Subframe{{
		SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
		Samples:   make([]int32, n),
		NSamples:  n,
	}}
	return f
}

// repeat returns a slice containing n copies of v.
func repeat(v, n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = v
	}
	return s
}
