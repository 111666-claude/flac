package flac

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/icza/bitio"
	"github.com/mewkiz/flac/meta"
	"github.com/mewkiz/pkg/errutil"
)

// Errors returned by Encoder.EnableSeekTable, Encoder.WriteFrame and
// Encoder.Close when generating a seek table.
var (
	// ErrSeekTableNotSeekable reports that seek table generation was requested
	// for an io.Writer that does not implement io.WriteSeeker. The reserved
	// SeekTable metadata block is filled in when the encoder is closed, and
	// therefore requires an output that can be rewound.
	ErrSeekTableNotSeekable = errors.New("flac: seek table generation requires an io.WriteSeeker")

	// ErrSeekTableTooLate reports that seek table generation was enabled after
	// one or more audio frames had already been written.
	ErrSeekTableTooLate = errors.New("flac: seek table generation must be enabled before writing audio frames")

	// ErrSeekTableAlreadyEnabled reports that seek table generation was enabled
	// more than once for the same encoder.
	ErrSeekTableAlreadyEnabled = errors.New("flac: seek table generation is already enabled")

	// ErrSeekTableExists reports that the metadata blocks passed to NewEncoder
	// already contain a SeekTable. The encoder never overwrites metadata
	// provided by the caller; remove the existing SeekTable block to let the
	// encoder generate one.
	ErrSeekTableExists = errors.New("flac: metadata blocks already contain a seek table")

	// ErrInvalidSeekTablePoints reports that a non-positive or too large
	// maximum number of seek points was requested.
	ErrInvalidSeekTablePoints = errors.New("flac: invalid maximum number of seek points")

	// ErrEmptyAudioStream reports that the encoder was closed with seek table
	// generation enabled, but no audio frames had been written.
	ErrEmptyAudioStream = errors.New("flac: cannot generate a seek table for an empty audio stream")

	// ErrEncoderClosed reports that an operation was attempted on a closed
	// encoder.
	ErrEncoderClosed = errors.New("flac: encoder is closed")
)

const (
	// seekPointSize is the encoded size in bytes of a single SeekPoint.
	seekPointSize = 18
	// maxSeekTablePoints is the largest number of seek points that fit in a
	// single SeekTable metadata block body; the block length field is 24 bits
	// wide.
	maxSeekTablePoints = (1<<24 - 1) / seekPointSize
)

// An Encoder represents a FLAC encoder.
type Encoder struct {
	// FLAC stream of encoder.
	*Stream
	// Underlying io.Writer or io.WriteCloser to the output stream.
	out io.Writer
	// Writer used for all encoded output. It counts the number of bytes written
	// so that the offsets of metadata blocks and audio frames are known.
	w *countWriter
	// Underlying io.WriteSeeker, if the output stream is seekable; otherwise
	// nil. Used to backfill the StreamInfo and SeekTable metadata blocks on
	// Close.
	ws io.WriteSeeker
	// startOff is the offset of the first byte written by the encoder, and
	// posKnown reports whether startOff could be determined. Seek operations
	// are relative to startOff, so the encoder may write to a writer that is
	// not positioned at offset zero of its underlying stream.
	startOff int64
	posKnown bool
	// Minimum and maximum block size (in samples) of frames written by encoder.
	blockSizeMin, blockSizeMax uint16
	// Minimum and maximum frame size (in bytes) of frames written by encoder.
	frameSizeMin, frameSizeMax uint32
	// MD5 running hash of unencoded audio samples.
	md5sum hash.Hash
	// Total number of samples (per channel) written by encoder.
	nsamples uint64
	// Current frame number if block size is fixed, and the first sample number
	// of the current frame otherwise.
	curNum uint64
	// AnalysisEnabled indicates whether analysis is enabled for the encoder.
	AnalysisEnabled bool

	// metaEnd is the offset of the first byte of the first audio frame header,
	// i.e. the offset immediately following the last metadata block. Offsets
	// are relative to the first byte written by the encoder.
	metaEnd int64
	// metaLastHdrOff is the offset of the header of the last metadata block
	// written by NewEncoder, and metaLastHdr contains its four header bytes.
	metaLastHdrOff int64
	metaLastHdr    [4]byte

	// seekTable indicates whether generation of a SeekTable metadata block is
	// enabled.
	seekTable bool
	// seekTableOffset is the offset of the body of the reserved SeekTable
	// metadata block.
	seekTableOffset int64
	// seekTableMaxPoints is the reserved number of seek points.
	seekTableMaxPoints int
	// seekPoints contains the seek points recorded for written audio frames.
	// At most seekTableMaxPoints points are retained.
	seekPoints []meta.SeekPoint
	// nframes is the number of audio frames written successfully.
	nframes uint64

	// writeErr is the first error encountered while writing. It is sticky, as
	// the output can no longer be considered valid and recorded offsets cannot
	// be trusted.
	writeErr error
	// closed reports whether Close has been called, and closeErr is the result
	// of the first call to Close.
	closed   bool
	closeErr error
}

// countWriter counts the number of bytes written to the underlying writer.
//
// It implements io.ByteWriter so that bitio writes bytes to the underlying
// writer immediately instead of buffering them in a bufio.Writer. This makes
// the byte count, and thus metadata offsets, exact while NewEncoder is writing
// metadata. The first four bytes of the output may be recorded through a
// startRecord/stopRecord pair to capture the header of a metadata block.
type countWriter struct {
	// Underlying writer.
	w io.Writer
	// Number of bytes written to w.
	n int64
	// Recorded header bytes.
	rec [4]byte
	// Number of recorded header bytes.
	recN int
	// recording indicates whether bytes are being recorded.
	recording bool
	// one is used to write single bytes to writers that do not implement
	// io.ByteWriter.
	one [1]byte
}

// startRecord starts recording the next (at most four) bytes written.
func (c *countWriter) startRecord() {
	c.recN = 0
	c.recording = true
}

// stopRecord stops recording and returns the recorded header bytes. The
// returned bool reports whether exactly four bytes were recorded.
func (c *countWriter) stopRecord() ([4]byte, bool) {
	c.recording = false
	return c.rec, c.recN == len(c.rec)
}

// record records the given bytes while recording is enabled.
func (c *countWriter) record(p []byte) {
	if !c.recording {
		return
	}
	n := copy(c.rec[c.recN:], p)
	c.recN += n
	if c.recN == len(c.rec) {
		c.recording = false
	}
}

// Write writes p to the underlying writer.
func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.record(p[:n])
	if err == nil && n < len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// WriteByte writes b to the underlying writer.
func (c *countWriter) WriteByte(b byte) error {
	c.one[0] = b
	var err error
	if bw, ok := c.w.(io.ByteWriter); ok {
		err = bw.WriteByte(b)
	} else {
		var n int
		n, err = c.w.Write(c.one[:])
		if err == nil && n != 1 {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return err
	}
	c.n++
	c.record(c.one[:])
	return nil
}

// NewEncoder returns a new FLAC encoder for the given metadata StreamInfo block
// and optional metadata blocks.
//
// By default prediction analysis is enabled. For more information, see
// Encoder.EnablePredictionAnalysis.
//
// To generate a SeekTable metadata block while encoding, call
// Encoder.EnableSeekTable before writing any audio frames. This requires that w
// implements io.WriteSeeker.
func NewEncoder(w io.Writer, info *meta.StreamInfo, blocks ...*meta.Block) (*Encoder, error) {
	// Store FLAC signature.
	enc := &Encoder{
		Stream: &Stream{
			Info:   info,
			Blocks: blocks,
		},
		out:             w,
		w:               &countWriter{w: w},
		md5sum:          md5.New(),
		AnalysisEnabled: true, // enable prediction analysis by default.
	}
	// Determine if the output can be rewound. Seek operations for seek table
	// generation are performed relative to the position at which the FLAC
	// stream starts, so that the encoder may write to a writer that is not
	// positioned at offset zero.
	if ws, ok := w.(io.WriteSeeker); ok {
		enc.ws = ws
		if off, err := ws.Seek(0, io.SeekCurrent); err == nil {
			enc.startOff = off
			enc.posKnown = true
		}
	}
	bw := bitio.NewWriter(enc.w)
	if _, err := bw.Write(flacSignature); err != nil {
		return nil, errutil.Err(err)
	}
	// Encode metadata blocks. Record the header and offset of each block so
	// that the header of the last block may be updated if a SeekTable is
	// appended by EnableSeekTable.
	enc.metaLastHdrOff = enc.w.n
	enc.w.startRecord()
	if err := encodeStreamInfo(bw, info, len(blocks) == 0); err != nil {
		return nil, errutil.Err(err)
	}
	if hdr, ok := enc.w.stopRecord(); ok {
		enc.metaLastHdr = hdr
	} else {
		return nil, errors.New("flac: unable to determine StreamInfo metadata block header")
	}
	for i, block := range blocks {
		enc.metaLastHdrOff = enc.w.n
		enc.w.startRecord()
		if err := encodeBlock(bw, block, i == len(blocks)-1); err != nil {
			return nil, errutil.Err(err)
		}
		hdr, ok := enc.w.stopRecord()
		if !ok {
			return nil, errors.New("flac: unable to determine metadata block header")
		}
		enc.metaLastHdr = hdr
	}
	// Flush pending writes of metadata blocks.
	if _, err := bw.Align(); err != nil {
		return nil, errutil.Err(err)
	}
	enc.metaEnd = enc.w.n
	// Return encoder to be used for encoding audio samples.
	return enc, nil
}

// EnableSeekTable enables generation of a SeekTable metadata block with at
// most maxPoints seek points.
//
// EnableSeekTable must be called after NewEncoder and before the first call to
// WriteFrame. The underlying io.Writer must implement io.WriteSeeker, as a
// SeekTable metadata block with maxPoints placeholder points is reserved at the
// end of the metadata section and filled in when Close is called.
//
// While encoding, the encoder records the first sample number, the byte offset
// relative to the first frame header, and the number of samples of each written
// audio frame. If more frames than maxPoints are written, the recorded points
// are thinned by dropping every second point (always retaining the point of the
// first frame), keeping memory use bounded while preserving coverage of the
// stream. At Close, the reserved block is filled with the recorded points in
// strictly ascending sample number order, and unused points are written as
// placeholder points, as defined by the FLAC format.
//
// The StreamInfo metadata block is still updated on Close with the minimum and
// maximum block sizes, the total number of samples and the MD5 checksum of the
// unencoded audio samples, as it is without seek table generation. All other
// metadata blocks are written unchanged.
//
// If the metadata blocks passed to NewEncoder already contain a SeekTable, or
// the underlying writer cannot be rewound, EnableSeekTable returns an error and
// leaves the encoder usable without seek table generation.
func (enc *Encoder) EnableSeekTable(maxPoints int) error {
	if enc.closed {
		return ErrEncoderClosed
	}
	if enc.seekTable {
		return ErrSeekTableAlreadyEnabled
	}
	if enc.writeErr != nil {
		return enc.writeErr
	}
	if maxPoints < 1 || maxPoints > maxSeekTablePoints {
		return fmt.Errorf("%w: got %d, expected a value in the range [1, %d]", ErrInvalidSeekTablePoints, maxPoints, maxSeekTablePoints)
	}
	if enc.ws == nil || !enc.posKnown {
		return ErrSeekTableNotSeekable
	}
	if enc.nframes > 0 {
		return ErrSeekTableTooLate
	}
	for _, block := range enc.Blocks {
		if block.Type == meta.TypeSeekTable {
			return ErrSeekTableExists
		}
		if _, ok := block.Body.(*meta.SeekTable); ok {
			return ErrSeekTableExists
		}
	}

	// Mark the last metadata block written by NewEncoder as no longer being
	// the last block. The header is rewritten in place, without updating the
	// byte count of the encoder.
	hdr := enc.metaLastHdr
	hdr[0] &^= 0x80 // clear the IsLast bit.
	if err := enc.seekAbs(enc.metaLastHdrOff); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to seek to metadata block header: %w", err))
	}
	if _, err := enc.ws.Write(hdr[:]); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to update metadata block header: %w", err))
	}

	// Append a SeekTable metadata block, filled with placeholder points, at
	// the end of the metadata section. The block is written through enc.w so
	// that the byte count, and thus the offsets of audio frames, stays exact.
	if err := enc.seekAbs(enc.metaEnd); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to seek to end of metadata section: %w", err))
	}
	bw := bitio.NewWriter(enc.w)
	tableHdr := &meta.Header{
		IsLast: true,
		Type:   meta.TypeSeekTable,
		Length: int64(seekPointSize * maxPoints),
	}
	if err := encodeBlockHeader(bw, tableHdr); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to write seek table header: %w", err))
	}
	var placeholder [seekPointSize]byte
	binary.BigEndian.PutUint64(placeholder[0:8], meta.PlaceholderPoint)
	for i := 0; i < maxPoints; i++ {
		if _, err := bw.Write(placeholder[:]); err != nil {
			return enc.failSeekTable(fmt.Errorf("flac: unable to reserve seek table: %w", err))
		}
	}
	if _, err := bw.Align(); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to flush seek table: %w", err))
	}

	enc.seekTableOffset = enc.metaEnd + int64(len(hdr))
	enc.metaEnd = enc.w.n
	if err := enc.seekAbs(enc.metaEnd); err != nil {
		return enc.failSeekTable(fmt.Errorf("flac: unable to seek past seek table: %w", err))
	}
	enc.seekTable = true
	enc.seekTableMaxPoints = maxPoints
	return nil
}

// failSeekTable marks the encoder as failed and returns err. A partially
// reserved seek table leaves the metadata section in an inconsistent state, so
// no further audio frames are accepted.
func (enc *Encoder) failSeekTable(err error) error {
	if enc.writeErr == nil {
		enc.writeErr = err
	}
	return err
}

// seekAbs seeks the underlying io.WriteSeeker to the given offset relative to
// the start of the FLAC stream.
func (enc *Encoder) seekAbs(off int64) error {
	if enc.ws == nil || !enc.posKnown {
		return ErrSeekTableNotSeekable
	}
	_, err := enc.ws.Seek(enc.startOff+off, io.SeekStart)
	return err
}

// Close closes the underlying io.Writer of the encoder and flushes any pending
// writes. If the io.Writer implements io.WriteSeeker, the encoder will update
// the StreamInfo metadata block with the MD5 checksum of the unencoded audio
// samples, the number of samples, and the minimum and maximum frame size and
// block size. If seek table generation is enabled, the reserved SeekTable
// metadata block is filled in as well.
//
// Close reports the first error encountered while writing audio frames, and any
// error encountered while updating or filling in metadata. In such cases the
// output must not be treated as a valid FLAC stream, as recorded byte offsets
// may be wrong. Calling Close more than once returns the result of the first
// call.
func (enc *Encoder) Close() error {
	if enc.closed {
		return enc.closeErr
	}
	enc.closed = true
	var err error
	if enc.writeErr != nil {
		// Offsets are not trustworthy after a failed write; do not backfill
		// metadata that would make the output look valid.
		err = enc.writeErr
	} else {
		err = enc.finalize()
	}
	if closer, ok := enc.out.(io.Closer); ok {
		if cerr := closer.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	enc.closeErr = err
	return err
}

// finalize updates the StreamInfo metadata block and fills in the reserved seek
// table before the underlying writer is closed.
func (enc *Encoder) finalize() error {
	// Update the StreamInfo metadata block even when no audio frames were
	// written, so that the metadata section is self-consistent.
	if enc.ws != nil {
		if err := enc.updateStreamInfo(); err != nil {
			return err
		}
	}
	if enc.seekTable && enc.nframes == 0 {
		// The reserved SeekTable block only contains placeholder points and
		// indexes no frames. Report the empty stream rather than presenting
		// the output as a successfully indexed stream.
		return ErrEmptyAudioStream
	}
	if enc.seekTable {
		if err := enc.writeSeekTable(); err != nil {
			return err
		}
		// Leave the underlying writer positioned at the end of the stream.
		if err := enc.seekAbs(enc.w.n); err != nil {
			return fmt.Errorf("flac: unable to seek to end of stream: %w", err)
		}
	}
	return nil
}

// updateStreamInfo writes the accumulated stream properties to the StreamInfo
// metadata block.
func (enc *Encoder) updateStreamInfo() error {
	off := int64(len(flacSignature))
	if enc.posKnown {
		off += enc.startOff
	}
	if _, err := enc.ws.Seek(off, io.SeekStart); err != nil {
		return errutil.Err(err)
	}
	// Update minimum and maximum block size (in samples) of FLAC stream. If no
	// audio frames were written, keep the block sizes supplied by the caller,
	// as a zero block size would render the stream invalid.
	if enc.nframes > 0 {
		enc.Info.BlockSizeMin = enc.blockSizeMin
		enc.Info.BlockSizeMax = enc.blockSizeMax
	}
	// Update minimum and maximum frame size (in bytes) of FLAC stream.
	enc.Info.FrameSizeMin = enc.frameSizeMin
	enc.Info.FrameSizeMax = enc.frameSizeMax
	// Update total number of samples (per channel) of FLAC stream.
	enc.Info.NSamples = enc.nsamples
	// Update MD5 checksum of the unencoded audio samples.
	sum := enc.md5sum.Sum(nil)
	for i := range sum {
		enc.Info.MD5sum[i] = sum[i]
	}
	bw := bitio.NewWriter(enc.ws)
	// Write updated StreamInfo metadata block to output stream. The StreamInfo
	// block is no longer the last metadata block if a seek table follows it.
	if err := encodeStreamInfo(bw, enc.Info, len(enc.Blocks) == 0 && !enc.seekTable); err != nil {
		return errutil.Err(err)
	}
	if _, err := bw.Align(); err != nil {
		return errutil.Err(err)
	}
	return nil
}

// writeSeekTable fills the reserved SeekTable metadata block with the recorded
// seek points, padding unused points with placeholder points.
func (enc *Encoder) writeSeekTable() error {
	if err := enc.seekAbs(enc.seekTableOffset); err != nil {
		return fmt.Errorf("flac: unable to seek to seek table: %w", err)
	}
	var buf [seekPointSize]byte
	for i := 0; i < enc.seekTableMaxPoints; i++ {
		var point meta.SeekPoint
		if i < len(enc.seekPoints) {
			point = enc.seekPoints[i]
		} else {
			point.SampleNum = meta.PlaceholderPoint
		}
		binary.BigEndian.PutUint64(buf[0:8], point.SampleNum)
		binary.BigEndian.PutUint64(buf[8:16], point.Offset)
		binary.BigEndian.PutUint16(buf[16:18], point.NSamples)
		if _, err := enc.ws.Write(buf[:]); err != nil {
			return fmt.Errorf("flac: unable to write seek table: %w", err)
		}
	}
	return nil
}

// addSeekPoint records a seek point for an audio frame. To keep the number of
// retained points bounded by seekTableMaxPoints, every second point is dropped
// when the table is full. This keeps the first point (the first audio frame)
// and retains points in strictly ascending sample number order.
func (enc *Encoder) addSeekPoint(point meta.SeekPoint) {
	if len(enc.seekPoints) >= enc.seekTableMaxPoints {
		if enc.seekTableMaxPoints == 1 {
			// Keep the point of the first audio frame.
			return
		}
		n := 0
		for i := 0; i < len(enc.seekPoints); i += 2 {
			enc.seekPoints[n] = enc.seekPoints[i]
			n++
		}
		enc.seekPoints = enc.seekPoints[:n]
	}
	enc.seekPoints = append(enc.seekPoints, point)
}

// EnablePredictionAnalysis specifies whether to enable analysis for the
// encoder. When analysis is enabled, subframes that are currently marked as
// PredVerbatim will be analyzed to use the best prediction method
// (constant, fixed or verbatim) based on size.
func (enc *Encoder) EnablePredictionAnalysis(enable bool) {
	enc.AnalysisEnabled = enable
}
