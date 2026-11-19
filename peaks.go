package flac

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/mewkiz/flac/frame"
)

// Default parameters for BuildPeaks.
const (
	// DefaultPeakWindowSize is the default number of samples per window at
	// the base (finest) level of the peak pyramid.
	DefaultPeakWindowSize = 512
	// DefaultPeakMergeFactor is the default number of windows of one level
	// that are merged into a single window of the next coarser level.
	DefaultPeakMergeFactor = 4
	// DefaultPeakMaxWindows is the default upper bound on the total number
	// of peak windows cached across all levels of the pyramid.
	DefaultPeakMaxWindows = 1 << 20
)

// Errors returned by peak pyramid construction and queries.
var (
	// ErrNoPeaks reports that a query was issued against a peak pyramid
	// without any windows (e.g. built from an empty FLAC stream).
	ErrNoPeaks = errors.New("flac: query on empty peak pyramid")
	// ErrTooManyWindows reports that the peak pyramid would exceed the
	// configured maximum number of cached windows (see PeakOptions.MaxWindows).
	ErrTooManyWindows = errors.New("flac: peak pyramid exceeds maximum number of cached windows")
)

// A PeakWindow holds the per-channel minimum and maximum sample values of a
// half-open sample range [Start, End).
type PeakWindow struct {
	// Start is the first sample number covered by the window (inclusive).
	Start uint64
	// End is one past the last sample number covered by the window
	// (exclusive). The final window of a level may be partial, in which case
	// End-Start is smaller than the window size of the level.
	End uint64
	// Min holds the minimum sample value of each channel within the window.
	Min []int32
	// Max holds the maximum sample value of each channel within the window.
	Max []int32
}

// Contains reports whether the given sample number lies within the window.
func (w PeakWindow) Contains(sampleNum uint64) bool {
	return sampleNum >= w.Start && sampleNum < w.End
}

// A PeakLevel is one time-resolution tier of a PeakPyramid. Level 0 holds the
// base windows; each subsequent level holds windows merged from
// PeakOptions.MergeFactor consecutive windows of the previous level.
type PeakLevel struct {
	// WindowSize is the number of samples covered by each full window of
	// this level. The last window of the level may be partial.
	WindowSize uint64
	// Windows holds the peak windows of this level, ordered by sample
	// number and contiguous from sample 0.
	Windows []PeakWindow
}

// A PeakPyramid is a reusable, multi-resolution min/max index over the PCM
// samples of a FLAC stream, suitable for fast waveform rendering at any zoom
// level. It is built by BuildPeaks and queried by PeakPyramid.Query.
type PeakPyramid struct {
	// Levels holds the resolution tiers, from finest (level 0) to coarsest.
	// It is empty for streams without audio frames.
	Levels []PeakLevel
	// NSamples is the total number of samples (per channel) scanned.
	NSamples uint64
	// NChannels is the number of channels of the stream.
	NChannels int
}

// PeakOptions configures BuildPeaks. The zero value is valid and selects the
// Default* constants of this package.
type PeakOptions struct {
	// WindowSize is the number of samples per window at the base level.
	// Defaults to DefaultPeakWindowSize.
	WindowSize uint64
	// MergeFactor is the number of windows merged into one window of the
	// next coarser level. Must be at least 2. Defaults to
	// DefaultPeakMergeFactor.
	MergeFactor int
	// MaxWindows bounds the total number of peak windows cached across all
	// levels of the pyramid. Defaults to DefaultPeakMaxWindows.
	MaxWindows int
}

// A PeakBuildError reports a corrupt audio frame encountered while scanning a
// FLAC stream for peak pyramid construction. The pyramid returned alongside
// the error holds the peaks of all samples decoded before the corrupt frame.
type PeakBuildError struct {
	// SampleNum is the sample number at which scanning stopped, i.e. the
	// number of samples successfully decoded before the corrupt frame.
	SampleNum uint64
	// Err is the underlying frame parsing error.
	Err error
}

// Error implements the error interface.
func (e *PeakBuildError) Error() string {
	return fmt.Sprintf("flac: unable to build peak pyramid; corrupt frame at sample number %d: %v", e.SampleNum, e.Err)
}

// Unwrap returns the underlying frame parsing error.
func (e *PeakBuildError) Unwrap() error {
	return e.Err
}

// normalize returns o with unset fields replaced by their defaults, or an
// error for invalid field values.
func (o *PeakOptions) normalize() (PeakOptions, error) {
	n := PeakOptions{
		WindowSize:  DefaultPeakWindowSize,
		MergeFactor: DefaultPeakMergeFactor,
		MaxWindows:  DefaultPeakMaxWindows,
	}
	if o != nil {
		if o.WindowSize != 0 {
			n.WindowSize = o.WindowSize
		}
		if o.MergeFactor != 0 {
			n.MergeFactor = o.MergeFactor
		}
		if o.MaxWindows != 0 {
			n.MaxWindows = o.MaxWindows
		}
	}
	if n.MergeFactor < 2 {
		return n, fmt.Errorf("flac: invalid peak merge factor %d; expected >= 2", n.MergeFactor)
	}
	if n.MaxWindows < 1 {
		return n, fmt.Errorf("flac: invalid peak window limit %d; expected >= 1", n.MaxWindows)
	}
	return n, nil
}

// BuildPeaks scans the audio frames of stream from its current position to
// the end of the stream and builds a reusable peak pyramid over the decoded
// PCM samples. The stream is consumed by the scan; its decoded samples are
// only read, never modified.
//
// Each window of the base level covers opts.WindowSize consecutive samples
// and records the per-channel minimum and maximum sample values. Coarser
// levels are derived by merging opts.MergeFactor consecutive windows of the
// previous level. The final window of each level may be partial if the
// sample count is not a multiple of the window size.
//
// A stream without audio frames yields a pyramid with no levels and
// NSamples == 0; queries against it return ErrNoPeaks.
//
// If a corrupt audio frame is encountered, the pyramid built from the samples
// decoded so far is returned along with a *PeakBuildError. If the pyramid
// would exceed opts.MaxWindows windows, nil is returned along with an error
// matching ErrTooManyWindows; increase PeakOptions.WindowSize or
// PeakOptions.MaxWindows to accommodate longer streams.
func BuildPeaks(stream *Stream, opts *PeakOptions) (*PeakPyramid, error) {
	o, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	b := &peakBuilder{opts: o}
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Corrupt frame; return the peaks of the samples decoded so
			// far along with the error. The window limit is not enforced
			// for the partial pyramid.
			levels, _ := b.finalize()
			return &PeakPyramid{
				Levels:    levels,
				NSamples:  b.nsamples,
				NChannels: b.nchannels,
			}, &PeakBuildError{SampleNum: b.nsamples, Err: err}
		}
		if err := b.addFrame(f); err != nil {
			return nil, err
		}
	}
	levels, err := b.finalize()
	if err != nil {
		return nil, err
	}
	return &PeakPyramid{
		Levels:    levels,
		NSamples:  b.nsamples,
		NChannels: b.nchannels,
	}, nil
}

// A peakBuilder accumulates base-level peak windows frame by frame and merges
// them into coarser levels.
type peakBuilder struct {
	opts      PeakOptions
	nsamples  uint64 // total number of samples scanned
	nchannels int    // number of channels of the stream
	nwindows  int    // total number of windows cached across all levels

	base []PeakWindow // base level windows

	// current (incomplete) window accumulator.
	curMin   []int32
	curMax   []int32
	curStart uint64
	curCount uint64
}

// addFrame folds the samples of one decoded audio frame into the base-level
// peak windows.
func (b *peakBuilder) addFrame(f *frame.Frame) error {
	nchannels := len(f.Subframes)
	if nchannels == 0 {
		return nil
	}
	if b.nchannels == 0 {
		b.nchannels = nchannels
	} else if nchannels != b.nchannels {
		return fmt.Errorf("flac: unable to build peak pyramid; channel count changed from %d to %d at sample number %d", b.nchannels, nchannels, b.nsamples)
	}
	nsamples := len(f.Subframes[0].Samples)
	for channel, sub := range f.Subframes {
		if len(sub.Samples) != nsamples {
			return fmt.Errorf("flac: unable to build peak pyramid; channel %d has %d samples, expected %d", channel, len(sub.Samples), nsamples)
		}
	}
	for i := 0; i < nsamples; i++ {
		if b.curCount == 0 {
			b.curStart = b.nsamples
			b.curMin = make([]int32, nchannels)
			b.curMax = make([]int32, nchannels)
			for channel := range b.curMin {
				sample := f.Subframes[channel].Samples[i]
				b.curMin[channel] = sample
				b.curMax[channel] = sample
			}
		} else {
			for channel := 0; channel < nchannels; channel++ {
				sample := f.Subframes[channel].Samples[i]
				if sample < b.curMin[channel] {
					b.curMin[channel] = sample
				}
				if sample > b.curMax[channel] {
					b.curMax[channel] = sample
				}
			}
		}
		b.curCount++
		if b.nsamples == math.MaxUint64 {
			return fmt.Errorf("flac: unable to build peak pyramid; sample count overflow")
		}
		b.nsamples++
		if b.curCount == b.opts.WindowSize {
			if err := b.flushWindow(); err != nil {
				return err
			}
		}
	}
	return nil
}

// flushWindow appends the current window accumulator to the base level, if
// non-empty. The final window of a stream may be partial.
func (b *peakBuilder) flushWindow() error {
	if b.curCount == 0 {
		return nil
	}
	if b.nwindows >= b.opts.MaxWindows {
		return fmt.Errorf("flac: unable to cache base window %d: %w", b.nwindows+1, ErrTooManyWindows)
	}
	b.base = append(b.base, PeakWindow{
		Start: b.curStart,
		End:   b.curStart + b.curCount,
		Min:   b.curMin,
		Max:   b.curMax,
	})
	b.nwindows++
	b.curCount = 0
	b.curMin = nil
	b.curMax = nil
	return nil
}

// finalize flushes the trailing partial window and merges the base level into
// successively coarser levels.
func (b *peakBuilder) finalize() ([]PeakLevel, error) {
	if err := b.flushWindow(); err != nil {
		return nil, err
	}
	if len(b.base) == 0 {
		return nil, nil
	}
	levels := []PeakLevel{{WindowSize: b.opts.WindowSize, Windows: b.base}}
	cur := b.base
	windowSize := b.opts.WindowSize
	factor := uint64(b.opts.MergeFactor)
	// Merge until a single window remains. The number of levels is bounded
	// by the word size, as each level covers at least twice as many samples
	// as the previous one.
	for len(cur) > 1 {
		if windowSize > math.MaxUint64/factor {
			// The window size of the next coarser level would overflow;
			// no stream can be long enough to require it.
			break
		}
		next := make([]PeakWindow, 0, (len(cur)+b.opts.MergeFactor-1)/b.opts.MergeFactor)
		for i := 0; i < len(cur); i += b.opts.MergeFactor {
			end := i + b.opts.MergeFactor
			if end > len(cur) {
				end = len(cur)
			}
			group := cur[i:end]
			merged := PeakWindow{
				Start: group[0].Start,
				End:   group[len(group)-1].End,
				Min:   append([]int32(nil), group[0].Min...),
				Max:   append([]int32(nil), group[0].Max...),
			}
			for _, w := range group[1:] {
				for channel := range merged.Min {
					if w.Min[channel] < merged.Min[channel] {
						merged.Min[channel] = w.Min[channel]
					}
					if w.Max[channel] > merged.Max[channel] {
						merged.Max[channel] = w.Max[channel]
					}
				}
			}
			next = append(next, merged)
		}
		if b.nwindows+len(next) > b.opts.MaxWindows {
			return nil, fmt.Errorf("flac: unable to cache level %d windows: %w", len(levels), ErrTooManyWindows)
		}
		b.nwindows += len(next)
		windowSize *= factor
		levels = append(levels, PeakLevel{WindowSize: windowSize, Windows: next})
		cur = next
	}
	return levels, nil
}

// A PeakQuery holds the result of a PeakPyramid.Query call: the peak windows
// covering the queried sample range at the resolution best matching the
// requested target width.
type PeakQuery struct {
	// Level is the index of the selected pyramid level.
	Level int
	// WindowSize is the number of samples covered by each full window of
	// the selected level.
	WindowSize uint64
	// Windows holds the peak windows overlapping the queried sample range,
	// ordered by sample number. The Start and End fields of each window
	// report its exact sample boundaries; the last window may be partial.
	Windows []PeakWindow
}

// Query returns the peak windows overlapping the half-open sample range
// [start, end), selecting the coarsest pyramid level whose window size still
// provides at least one window per unit of the given target width (e.g. per
// output pixel). If end exceeds the number of scanned samples it is clamped.
//
// It returns ErrNoPeaks if the pyramid holds no windows (e.g. built from an
// empty FLAC stream), and an error for a non-positive width or an empty
// range.
func (p *PeakPyramid) Query(start, end uint64, width int) (*PeakQuery, error) {
	if p == nil || len(p.Levels) == 0 {
		return nil, ErrNoPeaks
	}
	if width <= 0 {
		return nil, fmt.Errorf("flac: invalid peak query width %d; expected > 0", width)
	}
	if end > p.NSamples {
		end = p.NSamples
	}
	if start >= end {
		return nil, fmt.Errorf("flac: empty peak query range [%d, %d)", start, end)
	}
	// Select the coarsest level with at least one window per unit of target
	// width. Levels are ordered by increasing window size.
	samplesPerPixel := (end - start) / uint64(width)
	level := 0
	for i, l := range p.Levels {
		if l.WindowSize > samplesPerPixel {
			break
		}
		level = i
	}
	l := p.Levels[level]
	lo := start / l.WindowSize
	hi := (end - 1) / l.WindowSize
	if max := uint64(len(l.Windows)) - 1; hi > max {
		hi = max
	}
	return &PeakQuery{
		Level:      level,
		WindowSize: l.WindowSize,
		Windows:    l.Windows[lo : hi+1],
	}, nil
}
