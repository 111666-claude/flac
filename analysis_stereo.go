package flac

import (
	"math"

	"github.com/mewkiz/flac/frame"
)

// analyzeStereo selects the stereo channel assignment (independent, left/side,
// right/side or mid/side) that yields the fewest estimated encoded bits for
// the given frame. The samples of the frame are expected to be correlated
// (i.e. channel 0 holds left and channel 1 holds right), and are not modified
// by the analysis.
//
// The cost model mirrors the one used by analyzeSubframe: each candidate
// channel is estimated as the minimum of its constant, fixed predictor (order
// 0-4 with a single Rice partition) and verbatim encoding. The side channel is
// estimated with one extra bit per sample, matching the overhead applied
// during encoding.
//
// The selection is deterministic:
//   - candidates are considered in the order independent, left/side,
//     right/side, mid/side, and a strictly smaller estimate is required to
//     switch, so ties keep the earlier candidate;
//   - if the side channel (left - right) cannot be represented in 32 bits
//     (only possible for extreme samples at 32 bits-per-sample), all
//     decorrelated candidates are discarded and independent stereo is
//     selected.
func analyzeStereo(f *frame.Frame, bps uint) frame.Channels {
	left := f.Subframes[0].Samples
	right := f.Subframes[1].Samples
	n := len(left)
	if n != len(right) {
		// Should not happen (WriteFrame sanity-checks sample counts); fall
		// back to the channel assignment of the caller.
		return f.Channels
	}

	// Estimate the bit cost of each source channel.
	leftBits := estimateSubframeBits(left, bps)
	rightBits := estimateSubframeBits(right, bps)

	// Candidate 1: independent stereo (left, right).
	best := frame.ChannelsLR
	bestBits := leftBits + rightBits

	// Compute the mid and side channels in 64-bit arithmetic to detect
	// overflow of the side channel. The mid channel always fits in 32 bits.
	mid := make([]int32, n)
	side := make([]int32, n)
	sideOK := true
	for i := 0; i < n; i++ {
		l := int64(left[i])
		r := int64(right[i])
		s := l - r
		if s < math.MinInt32 || s > math.MaxInt32 {
			sideOK = false
		}
		side[i] = int32(s)
		mid[i] = int32((l + r) >> 1) // NOTE: `(left + right) >> 1`, matching frame.Decorrelate.
	}
	if !sideOK {
		// The side channel is not representable; all decorrelated candidates
		// are invalid, so keep independent stereo.
		return best
	}

	// The side channel requires an extra bit per sample; its estimate is
	// shared by all decorrelated candidates.
	sideBits := estimateSubframeBits(side, bps+1)

	// Candidate 2: left/side stereo.
	if bits := leftBits + sideBits; bits < bestBits {
		best, bestBits = frame.ChannelsLeftSide, bits
	}
	// Candidate 3: right/side stereo.
	if bits := rightBits + sideBits; bits < bestBits {
		best, bestBits = frame.ChannelsSideRight, bits
	}
	// Candidate 4: mid/side stereo.
	midBits := estimateSubframeBits(mid, bps)
	if bits := midBits + sideBits; bits < bestBits {
		best, bestBits = frame.ChannelsMidSide, bits
	}
	return best
}

// estimateSubframeBits estimates the encoded size in bits of a subframe
// holding the given samples, using the same cost model as analyzeSubframe:
// the minimum of constant, fixed predictor (order 0-4, single Rice partition)
// and verbatim encoding.
func estimateSubframeBits(samples []int32, bps uint) int {
	n := len(samples)
	if n == 0 {
		// Subframe header only.
		return 6
	}

	// Verbatim cost: 6-bit header + raw samples.
	best := 6 + n*int(bps)

	// Constant cost: 6-bit header + a single sample.
	allEqual := true
	for i := 1; i < n; i++ {
		if samples[i] != samples[0] {
			allEqual = false
			break
		}
	}
	if allEqual {
		best = 6 + int(bps)
	}

	// Fixed predictor cost (best of orders 0-4).
	for order := 0; order <= 4 && order < n; order++ {
		residuals := computeFixedResiduals(samples, order)
		k := chooseRice(residuals)
		if bits := costFixed(order, bps, residuals, k); bits < best {
			best = bits
		}
	}
	return best
}
