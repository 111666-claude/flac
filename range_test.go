package flac_test

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"testing"

	"github.com/mewkiz/flac"
)

// variableBlocksizeStereo are stereo, 16-bit, variable-blocksize FLAC files.
// Their frames use a mix of left/right and inter-channel decorrelated channel
// assignments, and file 25 contains frames as small as 64 samples, making
// frame-boundary cropping cheap to exercise.
var variableBlocksizeStereo = []string{
	"testdata/flac-test-files/subset/24 - variable blocksize file created with flake revision 264.flac",
	"testdata/flac-test-files/subset/25 - variable blocksize file created with flake revision 264, modified to create smaller blocks.flac",
}

// packSamples encodes one sample in the native FLAC little-endian PCM layout,
// using the smallest whole number of bytes that fits bitsPerSample.
func packSamples(buf []byte, samples []int32, bitsPerSample uint8) int {
	width := 0
	switch {
	case bitsPerSample <= 8:
		width = 1
	case bitsPerSample <= 16:
		width = 2
	case bitsPerSample <= 24:
		width = 3
	default:
		width = 4
	}
	off := 0
	for _, s := range samples {
		u := uint32(s)
		for b := 0; b < width; b++ {
			buf[off] = byte(u >> (8 * b))
			off++
		}
	}
	return off
}

// decodeAll decodes the full stream through the non-seekable Parse/ParseNext
// path and returns the packed, interleaved reference PCM plus stream info.
func decodeAll(t *testing.T, data []byte) ([]byte, *flac.Stream) {
	t.Helper()
	stream, err := flac.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("flac.Parse: %v", err)
	}
	var pcm []byte
	tmp := make([]byte, 32*8)
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ParseNext: %v", err)
		}
		nch := len(f.Subframes)
		n := len(f.Subframes[0].Samples)
		for i := 0; i < n; i++ {
			for ch := 0; ch < nch; ch++ {
				k := packSamples(tmp, f.Subframes[ch].Samples[i:i+1], stream.Info.BitsPerSample)
				pcm = append(pcm, tmp[:k]...)
			}
		}
	}
	return pcm, stream
}

// readRange reads a complete range through a RangeReader using the given
// scratch buffer size, and returns the packed PCM and the terminal error.
func readRange(t *testing.T, data []byte, start, nsamples uint64, bufSize int) ([]byte, *flac.RangeReader, error) {
	t.Helper()
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("flac.NewSeek: %v", err)
	}
	rr, err := stream.ReadRange(start, nsamples)
	if err != nil {
		return nil, nil, err
	}
	var got []byte
	p := make([]byte, bufSize)
	for {
		n, err := rr.Read(p)
		got = append(got, p[:n]...)
		if err != nil {
			return got, rr, err
		}
	}
}

func TestReadRange_VariableBlockSize(t *testing.T) {
	for _, path := range variableBlocksizeStereo {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		ref, stream := decodeAll(t, data)
		si := stream.Info
		stride := int(si.NChannels) * sampleByteWidth(si.BitsPerSample)

		// Ranges deliberately start and end inside variable-size frames,
		// spanning many of them, including the very last samples of the stream.
		ranges := []struct {
			start, nsamples uint64
		}{
			{0, 1},                      // first sample only
			{65, 1000},                  // crosses many small frames (file 25)
			{127, 3},                    // one sample before a frame boundary
			{si.NSamples/2 - 333, 7777}, // mid-stream, unaligned ends
			{si.NSamples - 10, 10},      // last ten samples
			{0, si.NSamples},            // the complete stream
		}
		bufSizes := []int{1, 3, stride, stride + 1, 4096}

		for ri, rng := range ranges {
			for _, bufSize := range bufSizes {
				name := fmt.Sprintf("%s/range%d/buf%d", path, ri, bufSize)
				t.Run(name, func(t *testing.T) {
					got, rr, err := readRange(t, data, rng.start, rng.nsamples, bufSize)
					if err != io.EOF {
						t.Fatalf("expected terminal io.EOF, got %v", err)
					}
					if rr.SampleRate != si.SampleRate || rr.BitsPerSample != si.BitsPerSample || rr.NChannels != si.NChannels {
						t.Fatalf("format mismatch: got %d Hz %d bps %d ch, want %d Hz %d bps %d ch",
							rr.SampleRate, rr.BitsPerSample, rr.NChannels,
							si.SampleRate, si.BitsPerSample, si.NChannels)
					}
					if rr.Start != rng.start || rr.NSamples != rng.nsamples {
						t.Fatalf("range fields mismatch: start=%d nsamples=%d", rr.Start, rr.NSamples)
					}
					want := ref[int(rng.start)*stride : int(rng.start+rng.nsamples)*stride]
					if len(got) != len(want) {
						t.Fatalf("byte length mismatch: got %d, want %d", len(got), len(want))
					}
					if !bytes.Equal(got, want) {
						// Locate first differing sample for a useful message.
						for i := range got {
							if got[i] != want[i] {
								t.Fatalf("PCM mismatch at byte %d (sample %d): got %02x, want %02x",
									i, i/stride, got[i], want[i])
							}
						}
					}
					if rem := rr.Remaining(); rem != 0 {
						t.Fatalf("Remaining() = %d after full read, want 0", rem)
					}
				})
			}
		}
	}
}

func sampleByteWidth(bps uint8) int {
	switch {
	case bps <= 8:
		return 1
	case bps <= 16:
		return 2
	case bps <= 24:
		return 3
	default:
		return 4
	}
}

// TestReadRange_MD5 verifies full-range output is byte-identical to the
// original decoder's PCM by checking StreamInfo's MD5 signature, the same
// integrity check used by ExampleOpen.
func TestReadRange_MD5(t *testing.T) {
	paths := append(append([]string{}, variableBlocksizeStereo...),
		"testdata/flac-test-files/subset/22 - 12 bit per sample.flac")
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := flac.NewSeek(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			rr, err := stream.ReadRange(0, stream.Info.NSamples)
			if err != nil {
				t.Fatal(err)
			}
			h := md5.New()
			// io.Copy turns the terminal io.EOF into a nil error.
			if _, err := io.Copy(h, rr); err != nil {
				t.Fatalf("io.Copy: %v", err)
			}
			if got, want := h.Sum(nil), stream.Info.MD5sum[:]; !bytes.Equal(got, want) {
				t.Fatalf("MD5 mismatch: got %x, want %x", got, want)
			}
		})
	}
}

// oneShotReader hides ReadSeeker methods so flac.New produces a non-seekable
// stream.
type oneShotReader struct {
	r io.Reader
}

func (r oneShotReader) Read(p []byte) (int, error) { return r.r.Read(p) }

func TestReadRange_Errors(t *testing.T) {
	data, err := os.ReadFile(variableBlocksizeStereo[1])
	if err != nil {
		t.Fatal(err)
	}
	ref, stream0 := decodeAll(t, data)
	total := stream0.Info.NSamples
	stride := int(stream0.Info.NChannels) * sampleByteWidth(stream0.Info.BitsPerSample)

	t.Run("non-seekable input", func(t *testing.T) {
		s, err := flac.New(oneShotReader{r: bytes.NewReader(data)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.ReadRange(0, 10)
		if !errors.Is(err, flac.ErrNoSeeker) {
			t.Fatalf("expected ErrNoSeeker, got %v", err)
		}
	})

	t.Run("zero length", func(t *testing.T) {
		s, _ := flac.NewSeek(bytes.NewReader(data))
		_, err := s.ReadRange(0, 0)
		if !errors.Is(err, flac.ErrZeroLength) {
			t.Fatalf("expected ErrZeroLength, got %v", err)
		}
	})

	t.Run("start at end of stream", func(t *testing.T) {
		s, _ := flac.NewSeek(bytes.NewReader(data))
		_, err := s.ReadRange(total, 1)
		if !errors.Is(err, flac.ErrRangeExceedsStream) {
			t.Fatalf("expected ErrRangeExceedsStream, got %v", err)
		}
	})

	t.Run("start beyond stream", func(t *testing.T) {
		s, _ := flac.NewSeek(bytes.NewReader(data))
		_, err := s.ReadRange(total+100, 1)
		if !errors.Is(err, flac.ErrRangeExceedsStream) {
			t.Fatalf("expected ErrRangeExceedsStream, got %v", err)
		}
	})

	t.Run("tail shorter than requested", func(t *testing.T) {
		s, _ := flac.NewSeek(bytes.NewReader(data))
		_, err := s.ReadRange(total-5, 6)
		if !errors.Is(err, flac.ErrRangeExceedsStream) {
			t.Fatalf("expected ErrRangeExceedsStream, got %v", err)
		}
		// A range that fits exactly must still work.
		got, _, err := readRange(t, data, total-5, 5, 128)
		if err != io.EOF {
			t.Fatalf("exact-tail range: %v", err)
		}
		want := ref[(total-5)*uint64(stride):]
		if !bytes.Equal(got, want) {
			t.Fatal("exact-tail range PCM mismatch")
		}
	})

	t.Run("integer overflow", func(t *testing.T) {
		s, _ := flac.NewSeek(bytes.NewReader(data))
		_, err := s.ReadRange(math.MaxUint64-1, 2)
		if !errors.Is(err, flac.ErrRangeOverflow) {
			t.Fatalf("expected ErrRangeOverflow, got %v", err)
		}
		_, err = s.ReadRange(math.MaxUint64, math.MaxUint64)
		if !errors.Is(err, flac.ErrRangeOverflow) {
			t.Fatalf("expected ErrRangeOverflow, got %v", err)
		}
	})

	t.Run("truncated stream", func(t *testing.T) {
		// Drop the final frame's bytes. For a stream whose total length is
		// known, Seek builds the frame table while locating the start frame and
		// surfaces the truncation up front, so ReadRange itself must fail.
		truncated := data[:len(data)-256]
		s, err := flac.NewSeek(bytes.NewReader(truncated))
		if err != nil {
			t.Fatal(err)
		}
		rr, err := s.ReadRange(0, total)
		if err == nil {
			// If the truncation falls outside the frames touched by Seek, it
			// must instead surface while delivering the range.
			out, rerr := io.ReadAll(rr)
			if rerr == nil || errors.Is(rerr, io.EOF) {
				t.Fatalf("expected non-EOF decode error, got %v", rerr)
			}
			if len(out) >= len(ref) {
				t.Fatalf("truncated stream delivered %d bytes, want fewer than %d", len(out), len(ref))
			}
			if !bytes.Equal(out, ref[:len(out)]) {
				t.Fatal("PCM delivered before truncation does not match reference")
			}
			return
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("expected wrapped/unexpected error, got bare io.EOF")
		}
	})

	t.Run("truncated stream unknown length", func(t *testing.T) {
		// With an unknown sample count the length cannot be validated ahead of
		// time. Pre-scan the intact stream once so its generated seek table
		// exists, then truncate the underlying data: range delivery must
		// emit every decodable sample and report the shortfall as
		// io.ErrUnexpectedEOF instead of a clean io.EOF.
		full, err := os.ReadFile("testdata/flac-test-files/subset/45 - no total number of samples set.flac")
		if err != nil {
			t.Fatal(err)
		}
		fullRef, fullSI := decodeAll(t, full)
		if fullSI.Info.NSamples != 0 {
			t.Fatalf("fixture changed: NSamples=%d, want 0", fullSI.Info.NSamples)
		}
		rs := bytes.NewReader(full)
		s, err := flac.NewSeek(rs)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Seek(0); err != nil { // build the generated seek table
			t.Fatalf("pre-scan Seek: %v", err)
		}
		rs.Reset(full[:len(full)-256])
		rr, err := s.ReadRange(0, uint64(len(fullRef)/stride))
		if err != nil {
			t.Fatalf("ReadRange: %v", err)
		}
		out, err := io.ReadAll(rr)
		// A mid-frame cut surfaces as a concrete frame-decode error (CRC
		// mismatch, padding/residual error or io.ErrUnexpectedEOF); every one
		// of these is an explicit failure rather than a clean io.EOF.
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("expected a decode/EOF error for truncated stream, got %v", err)
		}
		if !bytes.Equal(out, fullRef[:len(out)]) {
			t.Fatal("PCM delivered before truncation does not match reference")
		}
		if len(out) == 0 {
			t.Fatal("expected partial PCM before the truncation error")
		}
	})

	t.Run("corrupt frame", func(t *testing.T) {
		corrupt := bytes.Clone(data)
		// Metadata is at the start of the file; mutating mid-file hits audio
		// frame data and must break CRC-16 or residual decoding.
		idx := len(corrupt) * 2 / 3
		corrupt[idx] ^= 0xFF
		s, err := flac.NewSeek(bytes.NewReader(corrupt))
		if err != nil {
			t.Fatal(err)
		}
		rr, err := s.ReadRange(0, total)
		if err == nil {
			// Seek may not have touched the damaged frame; the error must then
			// surface during range delivery.
			if _, rerr := io.ReadAll(rr); rerr == nil || errors.Is(rerr, io.EOF) {
				t.Fatalf("expected decode error for corrupt frame, got %v", rerr)
			}
			return
		}
		if errors.Is(err, io.EOF) {
			t.Fatalf("corrupt frame produced bare io.EOF, want a decode error")
		}
	})
}

// TestReadRange_UnknownLength covers streams whose StreamInfo declares no
// total sample count: valid ranges work, over-long ranges deliver every
// available sample and report io.ErrUnexpectedEOF rather than io.EOF.
func TestReadRange_UnknownLength(t *testing.T) {
	path := "testdata/flac-test-files/subset/45 - no total number of samples set.flac"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ref, si := decodeAll(t, data)
	if si.Info.NSamples != 0 {
		t.Fatalf("test fixture changed: NSamples=%d, want 0", si.Info.NSamples)
	}
	stride := int(si.Info.NChannels) * sampleByteWidth(si.Info.BitsPerSample)
	actualSamples := uint64(len(ref) / stride)

	// A bounded, in-range read still terminates cleanly.
	got, _, err := readRange(t, data, 100, 200, 64)
	if err != io.EOF {
		t.Fatalf("bounded range: %v", err)
	}
	want := ref[100*uint64(stride) : 300*uint64(stride)]
	if !bytes.Equal(got, want) {
		t.Fatal("bounded range PCM mismatch")
	}

	// An over-long range cannot be rejected up front (length unknown); all
	// existing samples are delivered and the shortfall is reported.
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	rr, err := stream.ReadRange(0, actualSamples+1_000_000)
	if err != nil {
		t.Fatalf("ReadRange: %v", err)
	}
	out, err := io.ReadAll(rr)
	// A clean end-of-stream is impossible here: samples were requested beyond
	// what exists. The exact error (unexpected EOF vs. frame decode error)
	// depends on how the final frame terminates, but it must never be io.EOF.
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("expected a non-EOF error, got %v", err)
	}
	if !bytes.Equal(out, ref) {
		t.Fatal("over-long range did not deliver the full available PCM")
	}
	if rem := rr.Remaining(); rem != 1_000_000 {
		t.Fatalf("Remaining() = %d, want 1000000 unfulfilled samples", rem)
	}
}

// TestReadRange_SampleWidths checks byte widths for 8-, 12- and 24-bit files.
func TestReadRange_SampleWidths(t *testing.T) {
	cases := []struct {
		path string
		bps  uint8
	}{
		{"testdata/flac-test-files/subset/23 - 8 bit per sample.flac", 8},
		{"testdata/flac-test-files/subset/22 - 12 bit per sample.flac", 12},
		{"testdata/flac-test-files/subset/28 - high resolution audio, default settings.flac", 24},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			data, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			ref, si := decodeAll(t, data)
			if si.Info.BitsPerSample != tc.bps {
				t.Fatalf("fixture bps = %d, want %d", si.Info.BitsPerSample, tc.bps)
			}
			stride := int(si.Info.NChannels) * sampleByteWidth(si.Info.BitsPerSample)
			start := uint64(4097) // inside the second fixed-size frame
			nsamples := uint64(5000)
			got, rr, err := readRange(t, data, start, nsamples, 1000)
			if err != io.EOF {
				t.Fatalf("Read: %v", err)
			}
			if rr.BitsPerSample != tc.bps {
				t.Fatalf("BitsPerSample = %d, want %d", rr.BitsPerSample, tc.bps)
			}
			want := ref[start*uint64(stride) : (start+nsamples)*uint64(stride)]
			if !bytes.Equal(got, want) {
				t.Fatal("PCM mismatch across sample widths")
			}
		})
	}
}

// TestReadRange_PreservesSeekNext ensures the new API does not disturb the
// existing Stream.Seek/ParseNext behaviour.
func TestReadRange_PreservesSeekNext(t *testing.T) {
	data, err := os.ReadFile(variableBlocksizeStereo[0])
	if err != nil {
		t.Fatal(err)
	}
	stream, err := flac.NewSeek(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}

	// Drive a range read to completion.
	rr, err := stream.ReadRange(12345, 6789)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rr); err != nil {
		t.Fatalf("range copy: %v", err)
	}

	// Seek and frame parsing must still work afterwards.
	frameStart, err := stream.Seek(0)
	if err != nil {
		t.Fatalf("Seek after range read: %v", err)
	}
	if frameStart != 0 {
		t.Fatalf("Seek(0) = %d, want 0", frameStart)
	}
	f, err := stream.ParseNext()
	if err != nil {
		t.Fatalf("ParseNext after range read: %v", err)
	}
	if f.SampleNumber() != 0 {
		t.Fatalf("first frame sample number = %d, want 0", f.SampleNumber())
	}

	// Seek to a mid-stream frame after the range read, as in TestSeek.
	mid := stream.Info.NSamples / 2
	pos, err := stream.Seek(mid)
	if err != nil {
		t.Fatalf("Seek(mid): %v", err)
	}
	if pos > mid {
		t.Fatalf("Seek(%d) landed on frame starting at %d", mid, pos)
	}
	f, err = stream.ParseNext()
	if err != nil {
		t.Fatalf("ParseNext after Seek(mid): %v", err)
	}
	if f.SampleNumber() != pos {
		t.Fatalf("frame sample number %d, want seek position %d", f.SampleNumber(), pos)
	}
}
