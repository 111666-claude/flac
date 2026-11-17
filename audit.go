package flac

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// AuditReport summarizes the result of a streaming frame audit of a FLAC
// stream, as performed by Audit.
type AuditReport struct {
	// Frames is the number of audio frames that were decoded in full before
	// the audit terminated.
	Frames uint64
	// Samples is the total number of inter-channel samples decoded from the
	// frames counted in Frames.
	Samples uint64
	// ExpectedSamples is the total number of inter-channel samples declared
	// by the STREAMINFO metadata block; a 0 value implies unknown.
	ExpectedSamples uint64
	// ErrorOffset is the byte offset, relative to the start of the FLAC
	// stream, of the frame in which the first anomaly was detected; -1 if no
	// anomaly was found.
	ErrorOffset int64
}

// An AuditError describes the first anomaly encountered while auditing the
// frames of a FLAC stream. The statistics accumulated up to the point of
// failure are retained in the AuditReport returned alongside the error.
type AuditError struct {
	// Offset is the byte offset, relative to the start of the FLAC stream, of
	// the frame in which the anomaly was detected.
	Offset int64
	// Frame is the index of the frame in which the anomaly was detected.
	Frame uint64
	// Err is the underlying error describing the anomaly.
	Err error
}

// Error returns a string representation of the audit error.
func (e *AuditError) Error() string {
	return fmt.Sprintf("flac.Audit: anomaly at offset %d (frame %d): %v", e.Offset, e.Frame, e.Err)
}

// Unwrap returns the underlying error describing the anomaly.
func (e *AuditError) Unwrap() error {
	return e.Err
}

// countingReader keeps track of the number of bytes read from r.
type countingReader struct {
	r io.Reader
	n int64
}

// Read reads from the underlying reader and records the number of bytes read.
func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n += int64(n)
	return n, err
}

// Audit scans the audio frames of the FLAC stream of r sequentially, decoding
// each frame in turn to verify that every frame of the stream decodes
// correctly. It reports the number of frames and inter-channel samples
// decoded, and verifies frame number continuity and, when declared by
// STREAMINFO, the total number of samples and the maximum block and frame
// sizes of the stream.
//
// Frames are decoded and discarded one at a time; the PCM samples of the
// stream are never held in memory in their entirety.
//
// If an anomaly is encountered (e.g. a CRC checksum mismatch, a truncated
// frame, a discontinuity in frame numbering, or a frame exceeding the limits
// declared by STREAMINFO), the audit terminates and returns an *AuditError
// locating the frame at fault by byte offset. The returned report retains the
// statistics of all frames decoded prior to the anomaly. A stream free of
// anomalies yields a report with ErrorOffset set to -1 and a nil error.
//
// Audit does not modify the state of any existing Stream and does not alter
// the behavior of Stream.ParseNext; it opens its own Stream on r.
func Audit(r io.Reader) (report AuditReport, err error) {
	report.ErrorOffset = -1

	// Wrap r in a counting reader so that byte offsets can be reported. The
	// stream buffers reads internally, so the offset of the current frame is
	// derived by subtracting the buffered (read-ahead but not yet consumed)
	// bytes from the total number of bytes read.
	cr := &countingReader{r: r}
	stream, err := New(cr)
	if err != nil {
		report.ErrorOffset = 0
		return report, &AuditError{Offset: 0, Frame: 0, Err: err}
	}
	br := stream.r.(*bufio.Reader)
	offset := func() int64 {
		return cr.n - int64(br.Buffered())
	}
	report.ExpectedSamples = stream.Info.NSamples

	// base is the frame or sample number of the first frame of the stream;
	// subsequent frames are expected to follow contiguously from it. For
	// streams with a fixed block size, frame numbers are expected to be
	// consecutive; for streams with a variable block size, sample numbers are
	// expected to accumulate by block size.
	var base uint64
	var first = true
	for {
		off := offset()
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				// Graceful end of FLAC stream.
				break
			}
			report.ErrorOffset = off
			return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
		}

		// Verify continuity of frame (or sample) numbering.
		if first {
			base = f.Num
			first = false
		} else if f.HasFixedBlockSize {
			if want, got := base+report.Frames, f.Num; want != got {
				err := fmt.Errorf("non-contiguous frame numbering; expected frame number %d, got %d", want, got)
				report.ErrorOffset = off
				return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
			}
		} else {
			if want, got := base+report.Samples, f.Num; want != got {
				err := fmt.Errorf("non-contiguous sample numbering; expected sample number %d, got %d", want, got)
				report.ErrorOffset = off
				return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
			}
		}

		// Verify the frame against the limits declared by STREAMINFO.
		if max := stream.Info.BlockSizeMax; max > 0 && f.BlockSize > max {
			err := fmt.Errorf("frame block size %d exceeds STREAMINFO maximum block size %d", f.BlockSize, max)
			report.ErrorOffset = off
			return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
		}
		if max := stream.Info.FrameSizeMax; max > 0 {
			if size := uint64(offset() - off); size > uint64(max) {
				err := fmt.Errorf("frame size %d exceeds STREAMINFO maximum frame size %d", size, max)
				report.ErrorOffset = off
				return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
			}
		}

		report.Frames++
		report.Samples += uint64(f.BlockSize)
	}

	// Verify the total number of samples against STREAMINFO, if known.
	if want := stream.Info.NSamples; want != 0 && report.Samples != want {
		off := offset()
		err := fmt.Errorf("decoded %d samples, STREAMINFO declares %d", report.Samples, want)
		report.ErrorOffset = off
		return report, &AuditError{Offset: off, Frame: report.Frames, Err: err}
	}

	return report, nil
}

// IsAuditError reports whether err is an *AuditError, and if so returns it.
func IsAuditError(err error) (*AuditError, bool) {
	var aerr *AuditError
	if errors.As(err, &aerr) {
		return aerr, true
	}
	return nil, false
}
