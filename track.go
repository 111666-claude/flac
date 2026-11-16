package flac

// This file implements bounded audio views of the tracks described by a
// CueSheet metadata block. A TrackView decodes the original FLAC frames of the
// stream; it does not copy or re-encode audio, and it never reads outside the
// sample range of its track.

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// Errors returned by Stream.OpenTrack, Stream.NewTrackView and TrackView
// methods. They are designed to be used with errors.Is.
var (
	// ErrNoCueSheet reports that a bounded track view was requested for a
	// stream that has no CueSheet metadata block.
	ErrNoCueSheet = errors.New("flac: stream has no CueSheet metadata block")

	// ErrTrackNotFound reports that the CueSheet does not describe a track with
	// the requested track number.
	ErrTrackNotFound = errors.New("flac: CueSheet track not found")

	// ErrLeadOutTrack reports that a lead-out track was passed where a playable
	// track was expected. The lead-out track bounds the last playable track; it
	// is not itself a track that may be read.
	ErrLeadOutTrack = errors.New("flac: lead-out track is not a playable track")

	// ErrInvalidCueSheet reports that the CueSheet is structurally unable to
	// describe a bounded track, for example because track offsets are not
	// strictly increasing.
	ErrInvalidCueSheet = errors.New("flac: invalid CueSheet")

	// ErrMissingLeadOut reports that the CueSheet has no lead-out track. Without
	// a lead-out the end of the last playable track is unknown; the library does
	// not fall back to StreamInfo.NSamples since that would guess the track
	// length.
	ErrMissingLeadOut = errors.New("flac: CueSheet has no lead-out track")

	// ErrDuplicateTrackNum reports that the CueSheet contains more than one
	// track with the same track number.
	ErrDuplicateTrackNum = errors.New("flac: duplicated CueSheet track number")

	// ErrIndexNotFound reports that the track has no index with the requested
	// index number.
	ErrIndexNotFound = errors.New("flac: CueSheet track index not found")

	// ErrIndexOutOfBounds reports that an index point lies at or beyond the end
	// of its track.
	ErrIndexOutOfBounds = errors.New("flac: CueSheet track index out of bounds")

	// ErrSeekOutOfRange reports that a seek would move a TrackView outside its
	// track.
	ErrSeekOutOfRange = errors.New("flac: track view seek out of range")

	// ErrUnalignedPosition reports that a sample-oriented read was requested at
	// a byte position that does not fall on a sample boundary.
	ErrUnalignedPosition = errors.New("flac: track view position is not sample aligned")

	// ErrTrackViewClosed reports that a method was called on a closed
	// TrackView.
	ErrTrackViewClosed = errors.New("flac: track view is closed")
)

// A TrackView is a bounded, seekable view of one track of a FLAC stream, as
// described by the stream's CueSheet metadata block.
//
// The view presents decoded audio starting at the first sample of the track. A
// position of zero refers to the first sample of the track, not to the first
// sample of the FLAC stream. Reads and seeks are clamped to the half-open
// sample range [Start, End); they never return samples from the preceding
// track, the following track, or the lead-out track.
//
// TrackView implements io.ReadSeeker. Read returns interleaved, signed,
// little-endian PCM samples; each sample occupies ceil(BitsPerSample/8) bytes
// and samples are ordered by channel within a sample frame (the same channel
// order as frame.Frame.Subframes). Seek and Read positions are byte offsets
// relative to the first byte of the track. SeekSample and ReadSamples work in
// inter-channel samples instead of bytes.
//
// Track boundaries may fall in the middle of a FLAC frame. In that case the
// containing frame is decoded and the samples that precede the track start, or
// that follow the track end, are discarded. This also works for FLAC streams
// that use variable block sizes.
//
// A TrackView reads through the Stream that created it and therefore shares the
// stream's single underlying reader and seek table. Methods of different views
// of the same stream may be called concurrently; they are serialized
// internally. The Stream's own Next, ParseNext and Seek methods use the same
// reader, so a TrackView must not be used concurrently with them (and their
// positions are not meaningful while a view is in use). To use the whole stream
// again, simply call Stream.Seek or Stream.ParseNext after the last view
// operation; every TrackView operation repositions the reader as needed.
type TrackView struct {
	stream *Stream
	track  meta.CueSheetTrack

	// start and end are absolute sample numbers (relative to the beginning of
	// the FLAC stream) of the first sample of the track and the first sample
	// after the track, respectively.
	start, end uint64

	// channels, bitsPerSample and bytesPerSample describe the PCM format of the
	// view. bytesPerSample is ceil(bitsPerSample/8).
	channels       int
	bitsPerSample  int
	bytesPerSample int
	bytesPerFrame  int

	// length is the length of the track in bytes: (end-start)*bytesPerFrame.
	length int64

	// pos is the current position in bytes, relative to the start of the track.
	pos int64

	// closed reports whether Close has been called.
	closed bool

	// Frame cache. A track boundary may split a frame, so cached is the frame
	// that contains cachedStart (an absolute sample number), and cachedPCM is
	// its interleaved PCM encoding, built on first use.
	cached      *frame.Frame
	cachedStart uint64
	cachedPCM   []byte
}

// CueSheet returns the CueSheet metadata block of the stream, or nil if the
// stream contains no CueSheet block. NewSeek and Parse populate this field;
// New skips metadata blocks and therefore always leaves it nil.
func (stream *Stream) CueSheet() *meta.CueSheet {
	return stream.cueSheet
}

// OpenTrack opens a bounded view of the CueSheet track with the given track
// number. Track numbers are matched exactly against CueSheetTrack.Num; the
// lead-out track number is not accepted.
//
// OpenTrack uses the CueSheet metadata block of the stream and therefore
// requires a stream created with NewSeek; for any other stream it returns an
// error wrapping ErrNoSeeker. If the stream has no CueSheet block it returns
// ErrNoCueSheet.
//
// The returned view ends at the offset of the next track in the CueSheet (or at
// the lead-out track, when opening the last playable track). Track offsets are
// relative to the beginning of the FLAC audio stream, as specified by the FLAC
// format. The length of a track is never derived from StreamInfo.NSamples; if
// the CueSheet has no lead-out the view cannot be opened and ErrMissingLeadOut
// is returned.
//
// The CueSheet is validated before a view is returned. Duplicate track numbers
// yield ErrDuplicateTrackNum, offsets that are not strictly increasing yield
// ErrInvalidCueSheet, and an index point at or beyond the end of the requested
// track yields ErrIndexOutOfBounds.
func (stream *Stream) OpenTrack(trackNum uint8) (*TrackView, error) {
	return stream.NewTrackView(stream.cueSheet, trackNum)
}

// Tracks returns the tracks of the stream's CueSheet, including the lead-out
// track. It returns nil if the stream has no CueSheet. The returned slice is a
// copy and may be modified without affecting the stream.
func (stream *Stream) Tracks() []meta.CueSheetTrack {
	if stream.cueSheet == nil {
		return nil
	}
	tracks := make([]meta.CueSheetTrack, len(stream.cueSheet.Tracks))
	for i, track := range stream.cueSheet.Tracks {
		if len(track.Indicies) != 0 {
			track.Indicies = make([]meta.CueSheetTrackIndex, len(track.Indicies))
			copy(track.Indicies, stream.cueSheet.Tracks[i].Indicies)
		}
		tracks[i] = track
	}
	return tracks
}

// NewTrackView opens a bounded view of the track with the given track number in
// cs. It is like OpenTrack, but uses the provided CueSheet instead of the one
// embedded in the stream. This is useful when the cue sheet comes from an
// external .cue file. Passing a nil CueSheet returns ErrNoCueSheet.
//
// The stream must have been created with NewSeek; otherwise an error wrapping
// ErrNoSeeker is returned.
func (stream *Stream) NewTrackView(cs *meta.CueSheet, trackNum uint8) (*TrackView, error) {
	if !stream.seekable {
		return nil, fmt.Errorf("flac.Stream.NewTrackView: %w", ErrNoSeeker)
	}
	if cs == nil {
		return nil, ErrNoCueSheet
	}
	if len(cs.Tracks) == 0 {
		return nil, fmt.Errorf("%w: no tracks", ErrMissingLeadOut)
	}

	// Validate track numbers and offsets before looking for the requested
	// track, so that ambiguous cue sheets fail with a clear result instead of
	// silently selecting one of several candidates.
	seen := make(map[uint8]struct{}, len(cs.Tracks))
	for i, track := range cs.Tracks {
		if track.Num == 0 {
			return nil, fmt.Errorf("%w: invalid track number 0", ErrInvalidCueSheet)
		}
		if _, ok := seen[track.Num]; ok {
			return nil, fmt.Errorf("%w: track number %d", ErrDuplicateTrackNum, track.Num)
		}
		seen[track.Num] = struct{}{}
		if i > 0 && track.Offset <= cs.Tracks[i-1].Offset {
			return nil, fmt.Errorf("%w: track offsets are not strictly increasing (track %d offset %d follows offset %d)", ErrInvalidCueSheet, track.Num, track.Offset, cs.Tracks[i-1].Offset)
		}
	}

	// The last track of a cue sheet is always the lead-out track. Require it to
	// be present and to carry the track number mandated by the FLAC format for
	// the kind of cue sheet in use. If it is missing we must not guess the
	// length of the last track.
	leadOut := cs.Tracks[len(cs.Tracks)-1]
	wantLeadOutNum := uint8(255)
	if cs.IsCompactDisc {
		wantLeadOutNum = 170
	}
	if leadOut.Num != wantLeadOutNum {
		return nil, fmt.Errorf("%w: expected lead-out track number %d as the last track, got %d", ErrMissingLeadOut, wantLeadOutNum, leadOut.Num)
	}
	if len(cs.Tracks) < 2 {
		return nil, fmt.Errorf("%w: the cue sheet contains only the lead-out track", ErrMissingLeadOut)
	}

	// Locate the requested track.
	index := -1
	for i, track := range cs.Tracks {
		if track.Num == trackNum {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, fmt.Errorf("%w: track number %d", ErrTrackNotFound, trackNum)
	}
	if index == len(cs.Tracks)-1 {
		return nil, fmt.Errorf("%w: track number %d", ErrLeadOutTrack, trackNum)
	}

	track := cs.Tracks[index]
	end := cs.Tracks[index+1].Offset
	if track.Offset >= end {
		return nil, fmt.Errorf("%w: track %d has no samples (offset %d, next offset %d)", ErrInvalidCueSheet, track.Num, track.Offset, end)
	}

	// StreamInfo.NSamples is used only to reject a cue sheet that reaches
	// beyond the audio data; it is never used to derive a track length.
	if stream.Info != nil && stream.Info.NSamples != 0 && leadOut.Offset > stream.Info.NSamples {
		return nil, fmt.Errorf("%w: lead-out offset %d exceeds the stream sample count %d", ErrInvalidCueSheet, leadOut.Offset, stream.Info.NSamples)
	}

	// Validate the index points of the requested track.
	nsamples := end - track.Offset
	for _, ix := range track.Indicies {
		if ix.Offset >= nsamples {
			return nil, fmt.Errorf("%w: index %d offset %d is outside track %d of length %d", ErrIndexOutOfBounds, ix.Num, ix.Offset, track.Num, nsamples)
		}
	}

	if stream.Info == nil || stream.Info.BitsPerSample == 0 || stream.Info.NChannels == 0 {
		return nil, fmt.Errorf("%w: missing audio format information", ErrInvalidCueSheet)
	}
	bps := int(stream.Info.BitsPerSample)
	bytesPerSample := (bps + 7) / 8
	channels := int(stream.Info.NChannels)
	bytesPerFrame := bytesPerSample * channels
	if bytesPerFrame <= 0 {
		return nil, fmt.Errorf("%w: invalid audio format (%d bits per sample, %d channels)", ErrInvalidCueSheet, bps, channels)
	}
	if nsamples > uint64(math.MaxInt64)/uint64(bytesPerFrame) {
		return nil, fmt.Errorf("%w: track %d is too large to address as a byte stream", ErrInvalidCueSheet, track.Num)
	}

	return &TrackView{
		stream:         stream,
		track:          track,
		start:          track.Offset,
		end:            end,
		channels:       channels,
		bitsPerSample:  bps,
		bytesPerSample: bytesPerSample,
		bytesPerFrame:  bytesPerFrame,
		length:         int64(nsamples) * int64(bytesPerFrame),
	}, nil
}

// Track returns a copy of the CueSheet track described by the view.
func (view *TrackView) Track() meta.CueSheetTrack {
	track := view.track
	if len(track.Indicies) != 0 {
		track.Indicies = make([]meta.CueSheetTrackIndex, len(view.track.Indicies))
		copy(track.Indicies, view.track.Indicies)
	}
	return track
}

// Ensure that TrackView satisfies io.ReadSeeker.
var _ io.ReadSeeker = (*TrackView)(nil)

// Number returns the CueSheet track number of the view.
func (view *TrackView) Number() uint8 {
	return view.track.Num
}

// Start returns the absolute sample number of the first sample of the track,
// relative to the beginning of the FLAC stream.
func (view *TrackView) Start() uint64 {
	return view.start
}

// End returns the absolute sample number one past the last sample of the
// track, relative to the beginning of the FLAC stream. End is the offset of the
// next track (or of the lead-out track for the last playable track).
func (view *TrackView) End() uint64 {
	return view.end
}

// NSamples returns the number of inter-channel samples in the track.
func (view *TrackView) NSamples() uint64 {
	return view.end - view.start
}

// Length returns the length of the track in bytes, using the interleaved PCM
// encoding of Read.
func (view *TrackView) Length() int64 {
	return view.length
}

// Channels returns the number of audio channels of the track.
func (view *TrackView) Channels() int {
	return view.channels
}

// BitsPerSample returns the number of bits per sample of the track. Each sample
// occupies ceil(BitsPerSample/8) bytes in the output of Read.
func (view *TrackView) BitsPerSample() int {
	return view.bitsPerSample
}

// Indices returns a copy of the index points of the track. Index offsets are
// relative to the beginning of the track.
func (view *TrackView) Indices() []meta.CueSheetTrackIndex {
	if len(view.track.Indicies) == 0 {
		return nil
	}
	indices := make([]meta.CueSheetTrackIndex, len(view.track.Indicies))
	copy(indices, view.track.Indicies)
	return indices
}

// Index returns the sample offset of the index point with the given index
// number, relative to the beginning of the track. The second return value is
// false if the track has no such index point or if the index point lies outside
// the track.
func (view *TrackView) Index(num uint8) (uint64, bool) {
	for _, ix := range view.track.Indicies {
		if ix.Num == num {
			if ix.Offset >= view.NSamples() {
				return 0, false
			}
			return ix.Offset, true
		}
	}
	return 0, false
}

// IndexOffset returns the sample offset of the index point with the given index
// number, relative to the beginning of the track. It returns ErrIndexNotFound
// if the track has no such index point and ErrIndexOutOfBounds if the index
// point lies outside the track.
func (view *TrackView) IndexOffset(num uint8) (int64, error) {
	for _, ix := range view.track.Indicies {
		if ix.Num != num {
			continue
		}
		if ix.Offset >= view.NSamples() {
			return 0, fmt.Errorf("%w: index %d offset %d is outside track %d of length %d", ErrIndexOutOfBounds, ix.Num, ix.Offset, view.track.Num, view.NSamples())
		}
		if ix.Offset > uint64(math.MaxInt64) {
			return 0, fmt.Errorf("%w: index %d offset %d is too large", ErrIndexOutOfBounds, ix.Num, ix.Offset)
		}
		return int64(ix.Offset), nil
	}
	return 0, fmt.Errorf("%w: track %d has no index %d", ErrIndexNotFound, view.track.Num, num)
}

// SeekIndex seeks to the index point with the given index number. The index
// offset is relative to the beginning of the track. It returns ErrIndexNotFound
// if the track has no such index point and ErrIndexOutOfBounds if the index
// point lies outside the track.
func (view *TrackView) SeekIndex(num uint8) error {
	offset, err := view.IndexOffset(num)
	if err != nil {
		return err
	}
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	if view.closed {
		return ErrTrackViewClosed
	}
	view.seekSampleLocked(offset)
	return nil
}

// Pos returns the current position in bytes, relative to the beginning of the
// track.
func (view *TrackView) Pos() int64 {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	return view.pos
}

// SamplePos returns the current position in inter-channel samples, relative to
// the beginning of the track. If the position is not sample aligned (because
// Read was used with a byte count that is not a multiple of the sample frame
// size), SamplePos returns the sample containing the current byte.
func (view *TrackView) SamplePos() int64 {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	return view.pos / int64(view.bytesPerFrame)
}

// Read reads interleaved, signed, little-endian PCM bytes from the track into
// p. It returns io.EOF when the end of the track has been reached. If a read
// would cross the track boundary, only the bytes up to the boundary are
// returned.
func (view *TrackView) Read(p []byte) (int, error) {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	if view.closed {
		return 0, ErrTrackViewClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	if view.pos >= view.length {
		return 0, io.EOF
	}

	total := 0
	for total < len(p) && view.pos < view.length {
		abs := view.start + uint64(view.pos)/uint64(view.bytesPerFrame)
		if err := view.ensureFrameLocked(abs); err != nil {
			return total, err
		}
		pcm, err := view.pcmLocked()
		if err != nil {
			return total, err
		}
		frameOffset := int(abs-view.cachedStart)*view.bytesPerFrame + int(view.pos%int64(view.bytesPerFrame))
		available := int64(len(pcm) - frameOffset)
		if remaining := view.length - view.pos; available > remaining {
			available = remaining
		}
		if available <= 0 {
			return total, io.ErrUnexpectedEOF
		}
		n := int64(len(p) - total)
		if n > available {
			n = available
		}
		copy(p[total:], pcm[frameOffset:frameOffset+int(n)])
		total += int(n)
		view.pos += n
	}
	return total, nil
}

// ReadSamples reads inter-channel samples from the track into dst, one slice
// per channel. It reads at most the length of the shortest destination slice
// and returns the number of samples written to each slice. It returns io.EOF
// when the end of the track has been reached. If a read would cross the track
// boundary, only the samples up to the boundary are returned.
//
// dst must contain exactly Channels slices. ReadSamples returns
// ErrUnalignedPosition if the current position (set by Read or Seek) is not on
// a sample boundary.
func (view *TrackView) ReadSamples(dst [][]int32) (int, error) {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	if view.closed {
		return 0, ErrTrackViewClosed
	}
	if len(dst) != view.channels {
		return 0, fmt.Errorf("flac.TrackView.ReadSamples: channel count mismatch; expected %d, got %d", view.channels, len(dst))
	}
	if view.pos%int64(view.bytesPerFrame) != 0 {
		return 0, fmt.Errorf("%w: current position %d is not a multiple of the sample frame size %d", ErrUnalignedPosition, view.pos, view.bytesPerFrame)
	}
	want := -1
	for _, samples := range dst {
		if want < 0 || len(samples) < want {
			want = len(samples)
		}
	}
	if want <= 0 {
		return 0, nil
	}
	if view.pos >= view.length {
		return 0, io.EOF
	}

	total := 0
	for total < want && view.pos < view.length {
		abs := view.start + uint64(view.pos)/uint64(view.bytesPerFrame)
		if err := view.ensureFrameLocked(abs); err != nil {
			return total, err
		}
		offset := int(abs - view.cachedStart)
		available := len(view.cached.Subframes[0].Samples) - offset
		if available < 0 {
			return total, io.ErrUnexpectedEOF
		}
		remaining := (view.length - view.pos) / int64(view.bytesPerFrame)
		if int64(available) > remaining {
			available = int(remaining)
		}
		if available <= 0 {
			return total, io.ErrUnexpectedEOF
		}
		n := want - total
		if n > available {
			n = available
		}
		for channel := 0; channel < view.channels; channel++ {
			copy(dst[channel][total:total+n], view.cached.Subframes[channel].Samples[offset:offset+n])
		}
		total += n
		view.pos += int64(n) * int64(view.bytesPerFrame)
	}
	return total, nil
}

// Seek sets the position of the view, relative to the beginning of the track.
// Byte offsets are used, matching Read. Seek returns the new position in bytes.
//
// Seek accepts any position in [0, Length()], including Length() itself, which
// places the view at end-of-track so that the next Read returns io.EOF. A seek
// outside that range returns an error wrapping ErrSeekOutOfRange; the position
// is left unchanged.
func (view *TrackView) Seek(offset int64, whence int) (int64, error) {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	if view.closed {
		return 0, ErrTrackViewClosed
	}
	base, err := view.baseLocked(whence)
	if err != nil {
		return 0, err
	}
	target, ok := addInt64(base, offset)
	if !ok || target < 0 || target > view.length {
		return view.pos, fmt.Errorf("%w: byte offset %d from %d is outside [0, %d]", ErrSeekOutOfRange, offset, base, view.length)
	}
	view.pos = target
	return view.pos, nil
}

// SeekSample sets the position of the view in inter-channel samples, relative
// to the beginning of the track. SeekSample returns the new position in
// samples. The position is stored as a byte position internally, so a sample
// position always corresponds to the beginning of a sample frame.
//
// SeekSample accepts any position in [0, NSamples()]. A seek outside that range
// returns an error wrapping ErrSeekOutOfRange; the position is left unchanged.
func (view *TrackView) SeekSample(offset int64, whence int) (int64, error) {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	if view.closed {
		return 0, ErrTrackViewClosed
	}
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = view.pos / int64(view.bytesPerFrame)
	case io.SeekEnd:
		base = int64(view.NSamples())
	default:
		return view.pos / int64(view.bytesPerFrame), fmt.Errorf("flac.TrackView.SeekSample: invalid whence %d", whence)
	}
	target, ok := addInt64(base, offset)
	if !ok {
		return view.pos / int64(view.bytesPerFrame), fmt.Errorf("%w: sample offset %d from %d overflows", ErrSeekOutOfRange, offset, base)
	}
	return view.seekSampleLocked(target)
}

// seekSampleLocked sets the byte position for the provided sample offset,
// which is expected to be absolute within the track. The caller must hold
// stream.viewMu.
func (view *TrackView) seekSampleLocked(sample int64) (int64, error) {
	nsamples := int64(view.NSamples())
	if sample < 0 || sample > nsamples {
		return view.pos / int64(view.bytesPerFrame), fmt.Errorf("%w: sample offset %d is outside [0, %d]", ErrSeekOutOfRange, sample, nsamples)
	}
	view.pos = sample * int64(view.bytesPerFrame)
	return sample, nil
}

// baseLocked returns the base offset for a byte-oriented Seek. The caller must
// hold stream.viewMu.
func (view *TrackView) baseLocked(whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		return 0, nil
	case io.SeekCurrent:
		return view.pos, nil
	case io.SeekEnd:
		return view.length, nil
	default:
		return 0, fmt.Errorf("flac.TrackView.Seek: invalid whence %d", whence)
	}
}

// addInt64 returns a+b and reports whether the addition did not overflow.
func addInt64(a, b int64) (int64, bool) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, false
	}
	return sum, true
}

// Close releases the cached frame of the view. It does not close the
// underlying stream. Subsequent method calls on the view return
// ErrTrackViewClosed.
func (view *TrackView) Close() error {
	view.stream.viewMu.Lock()
	defer view.stream.viewMu.Unlock()
	view.closed = true
	view.cached = nil
	view.cachedPCM = nil
	return nil
}

// ensureFrameLocked makes sure that the frame containing the absolute sample
// number abs is cached. The caller must hold stream.viewMu.
func (view *TrackView) ensureFrameLocked(abs uint64) error {
	if view.cached != nil && abs >= view.cachedStart && abs < view.cachedStart+uint64(view.cached.BlockSize) {
		return nil
	}
	f, frameStart, err := view.stream.frameAt(abs)
	if err != nil {
		return fmt.Errorf("flac.TrackView: unable to locate sample %d of track %d: %w", abs, view.track.Num, err)
	}
	if len(f.Subframes) != view.channels {
		return fmt.Errorf("flac.TrackView: frame has %d channels, expected %d", len(f.Subframes), view.channels)
	}
	for _, subframe := range f.Subframes {
		if len(subframe.Samples) != int(f.BlockSize) {
			return fmt.Errorf("flac.TrackView: frame has %d samples in a subframe, expected %d", len(subframe.Samples), f.BlockSize)
		}
	}
	view.cached = f
	view.cachedStart = frameStart
	view.cachedPCM = nil
	return nil
}

// frameAt decodes and returns the frame that contains the absolute sample
// number abs, together with the absolute sample number of its first sample.
//
// The frame is located through the stream's seek table, and its sample number
// is derived by accumulating block sizes from the seek point. This is correct
// for variable-blocksize streams and for the shortened last frame of a
// fixed-blocksize stream, for which Frame.SampleNumber is not reliable.
func (stream *Stream) frameAt(abs uint64) (*frame.Frame, uint64, error) {
	if stream.seekTable == nil {
		if stream.seekTableSize <= 0 {
			return nil, 0, ErrNoSeektable
		}
		if err := stream.makeSeekTable(); err != nil {
			return nil, 0, err
		}
	}
	if len(stream.seekTable.Points) == 0 {
		return nil, 0, ErrNoSeektable
	}
	rs, ok := stream.r.(io.ReadSeeker)
	if !ok {
		return nil, 0, ErrNoSeeker
	}

	// Find the last seek point at or before abs. Placeholder points carry no
	// sample number and are ignored.
	var point meta.SeekPoint
	found := false
	for _, p := range stream.seekTable.Points {
		if p.SampleNum == meta.PlaceholderPoint {
			continue
		}
		if p.SampleNum > abs {
			break
		}
		point = p
		found = true
	}
	if !found {
		// No seek point precedes abs (for example, a table whose first point is
		// after the target sample). Start decoding from the first frame rather
		// than jumping forward past the target.
		point = meta.SeekPoint{SampleNum: 0, Offset: 0}
	}

	if _, err := rs.Seek(stream.dataStart+int64(point.Offset), io.SeekStart); err != nil {
		return nil, 0, err
	}
	sampleNum := point.SampleNum
	for {
		// Record the offset to the start of the frame so that we can rewind to
		// it once the frame containing abs has been found.
		offset, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				return nil, 0, io.ErrUnexpectedEOF
			}
			return nil, 0, err
		}
		if abs < sampleNum+uint64(f.BlockSize) {
			if _, err := rs.Seek(offset, io.SeekStart); err != nil {
				return nil, 0, err
			}
			return f, sampleNum, nil
		}
		sampleNum += uint64(f.BlockSize)
	}
}

// pcmLocked returns the cached frame encoded as interleaved, signed,
// little-endian PCM. The caller must hold stream.viewMu.
func (view *TrackView) pcmLocked() ([]byte, error) {
	if view.cachedPCM != nil {
		return view.cachedPCM, nil
	}
	if view.cached == nil {
		return nil, io.ErrUnexpectedEOF
	}
	nsamples := len(view.cached.Subframes[0].Samples)
	buf := make([]byte, nsamples*view.bytesPerFrame)
	offset := 0
	for i := 0; i < nsamples; i++ {
		for channel := 0; channel < view.channels; channel++ {
			sample := view.cached.Subframes[channel].Samples[i]
			for b := 0; b < view.bytesPerSample; b++ {
				buf[offset] = byte(sample >> (8 * uint(b)))
				offset++
			}
		}
	}
	view.cachedPCM = buf
	return buf, nil
}
