package flac

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// Sentinel errors reported by ReadRange and RangeReader.Read. The errors
// returned by ReadRange wrap these sentinels, so callers may use errors.Is to
// check for specific failure conditions.
var (
	// ErrZeroLength reports that a sample range of zero length was requested.
	ErrZeroLength = errors.New("flac.ReadRange: zero-length sample range")

	// ErrRangeOverflow reports that the requested sample range overflows the
	// uint64 sample-number space, or that its decoded PCM would exceed the
	// addressable buffer size.
	ErrRangeOverflow = errors.New("flac.ReadRange: integer overflow in requested sample range")

	// ErrRangeExceedsStream reports that the requested sample range starts at,
	// or extends past, the end of the audio stream.
	ErrRangeExceedsStream = errors.New("flac.ReadRange: requested sample range exceeds stream length")
)

// RangeReader provides streaming access to an exact, sample-accurate range of
// the decoded, interleaved PCM samples of a FLAC stream.
//
// Audio samples are decoded one FLAC frame at a time, cropped to the requested
// [Start, Start+NSamples) sample interval and packed into the native FLAC
// PCM layout: channels interleaved per sample, each sample stored as a
// little-endian signed integer of BitsPerSample width rounded up to the next
// whole byte (8-bit samples use 1 byte, 12/16-bit samples use 2 bytes and
// 20/24-bit samples use 3 bytes). The full requested range is never buffered;
// at most one frame is held in memory at any time.
//
// RangeReader implements io.Reader. Read returns io.EOF once the complete
// requested range has been delivered. A corrupt frame, or a stream that ends
// before the requested range is complete, reports the underlying decode error
// (often io.ErrUnexpectedEOF), never a clean io.EOF.
type RangeReader struct {
	// Start is the absolute sample number of the first sample in the range
	// (counted from the start of the stream, regardless of channel count).
	Start uint64
	// NSamples is the number of inter-channel samples in the requested range.
	NSamples uint64
	// SampleRate is the sample rate in Hz, taken from StreamInfo.
	SampleRate uint32
	// BitsPerSample is the sample size in bits-per-sample, taken from StreamInfo.
	BitsPerSample uint8
	// NChannels is the number of channels, taken from StreamInfo.
	NChannels uint8

	// stream is the seekable FLAC stream the range is read from.
	stream *Stream
	// end is the absolute sample number immediately following the range.
	end uint64
	// delivered counts the inter-channel samples already packed into buf. It
	// drives completion independently of the physical order of frames, which
	// the FLAC specification does not require to match sample-number order.
	delivered uint64
	// stride is the number of bytes occupied by one inter-channel sample
	// (i.e. one sample from every channel) in the packed PCM output.
	stride int
	// buf holds packed PCM bytes of the current frame, cropped to the
	// intersection of the frame and the requested range.
	buf []byte
	// readBytes counts the number of PCM bytes already returned by Read.
	readBytes uint64
}

// ReadRange prepares a sample-accurate read of the NSamples inter-channel
// samples starting at absolute sample number start from a seekable FLAC
// stream. Unlike Seek, which only positions the stream on the frame containing
// a sample, the returned RangeReader crops frames so that the first and last
// samples delivered are exactly the ones requested, even when the range
// starts or ends in the middle of a variable-size frame.
//
// The stream must have been created with NewSeek; otherwise ErrNoSeeker is
// returned. The seek tables used by Stream.Seek are reused, so no decoding
// state of the stream beyond its read position is modified.
//
// The following error conditions are reported up front and wrap the matching
// sentinel error:
//
//   - ErrNoSeeker: the stream does not support seeking.
//   - ErrZeroLength: nsamples is 0.
//   - ErrRangeOverflow: start + nsamples wraps around the uint64 sample space.
//   - ErrRangeExceedsStream: start is at or past the last sample, or the range
//     extends past the end of a stream whose total sample count is known.
//
// When StreamInfo.NSamples is 0 (unknown total length) the range cannot be
// validated ahead of time; an over-long range then delivers all available
// samples and Read returns a decode error (typically io.ErrUnexpectedEOF)
// instead of io.EOF.
func (stream *Stream) ReadRange(start, nsamples uint64) (*RangeReader, error) {
	if _, ok := stream.r.(io.ReadSeeker); !ok {
		return nil, ErrNoSeeker
	}
	if nsamples == 0 {
		return nil, fmt.Errorf("%w: start=%d nsamples=0", ErrZeroLength, start)
	}
	if nsamples > math.MaxUint64-start {
		return nil, fmt.Errorf("%w: start=%d nsamples=%d", ErrRangeOverflow, start, nsamples)
	}
	end := start + nsamples
	if total := stream.Info.NSamples; total != 0 && end > total {
		return nil, fmt.Errorf("%w: start=%d nsamples=%d (stream has %d samples)", ErrRangeExceedsStream, start, nsamples, total)
	}

	width := sampleWidth(stream.Info.BitsPerSample)
	if width <= 0 {
		return nil, fmt.Errorf("%w: unsupported bits-per-sample %d", ErrRangeOverflow, stream.Info.BitsPerSample)
	}
	stride := int(stream.Info.NChannels) * width
	// Block sizes are at most 65535 samples, so the per-frame buffer always
	// fits in an int; guard the multiplication anyway against future changes.
	if int64(stride)*65535 > int64(math.MaxInt) {
		return nil, fmt.Errorf("%w: decoded frame too large; channels=%d bits-per-sample=%d", ErrRangeOverflow, stream.Info.NChannels, stream.Info.BitsPerSample)
	}

	// Locate the frame containing the first sample. Re-anchor the read position
	// at the start of the audio data first so the result does not depend on the
	// stream's current position: a cursor already at or past the last frame
	// would otherwise make Stream.Seek walk off the end of the stream. Seek
	// restores the position to the start of the containing frame; RangeReader
	// then fills lazily from there.
	rs := stream.r.(io.ReadSeeker)
	if _, err := rs.Seek(stream.dataStart, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := stream.Seek(start); err != nil {
		return nil, err
	}

	return &RangeReader{
		Start:         start,
		NSamples:      nsamples,
		SampleRate:    stream.Info.SampleRate,
		BitsPerSample: stream.Info.BitsPerSample,
		NChannels:     stream.Info.NChannels,
		stream:        stream,
		end:           end,
		stride:        stride,
	}, nil
}

// sampleWidth returns the number of bytes used to store one sample of the
// given bits-per-sample size in the packed little-endian PCM output. It
// returns 0 for unsupported sample sizes.
func sampleWidth(bitsPerSample uint8) int {
	switch {
	case 1 <= bitsPerSample && bitsPerSample <= 8:
		return 1
	case 9 <= bitsPerSample && bitsPerSample <= 16:
		return 2
	case 17 <= bitsPerSample && bitsPerSample <= 24:
		return 3
	case 25 <= bitsPerSample && bitsPerSample <= 32:
		return 4
	default:
		return 0
	}
}

// Read reads up to len(p) bytes of the requested, cropped PCM range into p. It
// returns the number of bytes read and an error; n may be greater than 0 even
// when an error is returned. io.EOF is returned only after the complete
// requested range has been delivered.
func (rr *RangeReader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	for n == 0 {
		if len(rr.buf) == 0 {
			if err = rr.fill(); err != nil {
				return n, err
			}
		}
		k := copy(p[n:], rr.buf)
		rr.buf = rr.buf[k:]
		n += k
	}
	rr.readBytes += uint64(n)
	return n, nil
}

// Remaining returns the number of inter-channel samples of the requested
// range that have not yet been delivered by Read.
func (rr *RangeReader) Remaining() uint64 {
	delivered := rr.readBytes / uint64(rr.stride)
	if delivered >= rr.NSamples {
		return 0
	}
	return rr.NSamples - delivered
}

// fill decodes the next FLAC frame, crops it to the requested sample interval
// and packs the cropped, interleaved PCM into rr.buf. It returns io.EOF once
// the whole requested range has been delivered, and io.ErrUnexpectedEOF (or
// the decode error) if the stream ends or a frame is corrupt before then.
//
// Frames are selected purely by intersecting their own [SampleNumber,
// SampleNumber+BlockSize) interval with the requested absolute interval, so
// correctness does not depend on the physical ordering of frames.
func (rr *RangeReader) fill() error {
	if rr.delivered >= rr.NSamples {
		return io.EOF
	}

	// Decode frames until one intersects the requested interval. Frames outside
	// the interval (e.g. an out-of-order frame already passed, or one preceding
	// the seek point) contribute nothing and are skipped without recursing.
	for {
		f, err := rr.stream.ParseNext()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// The stream ended. A final frame may extend past the sample
				// count declared by StreamInfo (padding samples), so reaching
				// EOF after the whole requested range was packed is a graceful
				// completion; requested samples still missing means truncation.
				if rr.delivered >= rr.NSamples {
					return io.EOF
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}

		frameStart := f.SampleNumber()
		frameEnd := frameStart + uint64(f.BlockSize)
		nch := int(rr.NChannels)
		if len(f.Subframes) != nch {
			return fmt.Errorf("flac.RangeReader: frame at sample %d has %d subframes, expected %d", frameStart, len(f.Subframes), nch)
		}

		// Intersect the frame with the requested interval.
		lo := rr.Start
		if frameStart > lo {
			lo = frameStart
		}
		hi := rr.end
		if frameEnd < hi {
			hi = frameEnd
		}
		if hi <= lo {
			continue // frame lies entirely outside the requested interval
		}

		first := int(lo - frameStart)
		last := int(hi - frameStart) // exclusive
		width := rr.stride / nch

		// Grow the reusable buffer only when a larger crop is encountered.
		needed := (last - first) * rr.stride
		if cap(rr.buf) < needed {
			rr.buf = make([]byte, needed)
		} else {
			rr.buf = rr.buf[:needed]
		}

		// Pack interleaved samples in native FLAC little-endian PCM layout.
		off := 0
		for i := first; i < last; i++ {
			for ch := 0; ch < nch; ch++ {
				if i >= len(f.Subframes[ch].Samples) {
					return io.ErrUnexpectedEOF
				}
				s := uint32(f.Subframes[ch].Samples[i])
				for b := 0; b < width; b++ {
					rr.buf[off] = byte(s >> (8 * b))
					off++
				}
			}
		}

		rr.delivered += uint64(last - first)
		return nil
	}
}

// Compile-time assertion that RangeReader is an io.Reader.
var _ io.Reader = (*RangeReader)(nil)
