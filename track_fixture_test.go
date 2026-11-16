package flac_test

// This file builds deterministic FLAC files with an embedded CueSheet, used by
// the track view tests. The files are generated in memory from synthetic PCM
// so that the exact sample values at track boundaries are known.

import (
	"bytes"
	"fmt"
	"io"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// trackFixture describes a synthetic FLAC stream with a CueSheet.
type trackFixture struct {
	// name is used in test sub-test names.
	name string
	// sampleRate, nchannels and bitsPerSample describe the stream format.
	sampleRate, nchannels, bitsPerSample int
	// blockSizes lists the block size of each encoded frame. A variable
	// blocking strategy is used when the sizes differ.
	blockSizes []int
	// sample is the deterministic audio function; it must return values that
	// fit in the stream's bit depth.
	sample func(sampleNum, channel int) int32
	// tracks are the playable tracks of the cue sheet, in order.
	tracks []fixtureTrack
	// leadOut is the offset of the lead-out track.
	leadOut uint64
	// leadOutNum overrides the lead-out track number when non-zero.
	leadOutNum uint8
	// extraBlocks are additional metadata blocks to append.
	extraBlocks []*meta.Block
	// noCueSheet omits the CueSheet block.
	noCueSheet bool
	// fixedBlockSize forces a fixed-blocksize blocking strategy even when the
	// block sizes differ. The FLAC format permits only the last frame of such a
	// stream to be shorter than the others, so this is only valid when all but
	// the last block size are equal. It is useful for exercising the shortened
	// last frame, for which Frame.SampleNumber is not reliable.
	fixedBlockSize bool
}

// fixtureTrack describes a playable track in a trackFixture.
type fixtureTrack struct {
	num     uint8
	offset  uint64
	indices []meta.CueSheetTrackIndex
}

// nsamples returns the total number of samples in the fixture.
func (f *trackFixture) nsamples() int {
	total := 0
	for _, n := range f.blockSizes {
		total += n
	}
	return total
}

// encode renders the fixture as a complete FLAC stream.
func (f *trackFixture) encode() ([]byte, error) {
	info := &meta.StreamInfo{
		BlockSizeMin:  uint16(f.blockSizes[0]),
		BlockSizeMax:  uint16(f.blockSizes[0]),
		SampleRate:    uint32(f.sampleRate),
		NChannels:     uint8(f.nchannels),
		BitsPerSample: uint8(f.bitsPerSample),
		NSamples:      uint64(f.nsamples()),
	}
	for _, n := range f.blockSizes {
		if uint16(n) < info.BlockSizeMin {
			info.BlockSizeMin = uint16(n)
		}
		if uint16(n) > info.BlockSizeMax {
			info.BlockSizeMax = uint16(n)
		}
	}

	var blocks []*meta.Block
	blocks = append(blocks, f.extraBlocks...)
	if !f.noCueSheet {
		var csTracks []meta.CueSheetTrack
		for _, t := range f.tracks {
			csTracks = append(csTracks, meta.CueSheetTrack{
				Offset:   t.offset,
				Num:      t.num,
				IsAudio:  true,
				Indicies: t.indices,
			})
		}
		leadOutNum := f.leadOutNum
		if leadOutNum == 0 {
			leadOutNum = 255
		}
		csTracks = append(csTracks, meta.CueSheetTrack{
			Offset:  f.leadOut,
			Num:     leadOutNum,
			IsAudio: true,
		})
		blocks = append(blocks, &meta.Block{
			Header: meta.Header{Type: meta.TypeCueSheet, Length: 1},
			Body:   &meta.CueSheet{Tracks: csTracks},
		})
	}

	out := new(bytes.Buffer)
	enc, err := flac.NewEncoder(out, info, blocks...)
	if err != nil {
		return nil, err
	}
	enc.EnablePredictionAnalysis(false)

	sampleNum := 0
	variable := false
	for _, n := range f.blockSizes {
		if n != f.blockSizes[0] {
			variable = true
		}
	}
	if f.fixedBlockSize {
		variable = false
	}
	for _, n := range f.blockSizes {
		hdr := frame.Header{
			HasFixedBlockSize: !variable,
			BlockSize:         uint16(n),
			SampleRate:        uint32(f.sampleRate),
			Channels:          frame.Channels(f.nchannels - 1),
			BitsPerSample:     uint8(f.bitsPerSample),
		}
		fr := &frame.Frame{Header: hdr}
		fr.Subframes = make([]*frame.Subframe, f.nchannels)
		for channel := 0; channel < f.nchannels; channel++ {
			samples := make([]int32, n)
			for i := range samples {
				samples[i] = f.sample(sampleNum+i, channel)
			}
			fr.Subframes[channel] = &frame.Subframe{
				SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
				Samples:   samples,
				NSamples:  n,
			}
		}
		if err := enc.WriteFrame(fr); err != nil {
			return nil, err
		}
		sampleNum += n
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// rampSample returns a deterministic sample that is unique per (sampleNum,
// channel) pair within the range used by the fixtures.
func rampSample(sampleRate, nchannels, bitsPerSample int) func(sampleNum, channel int) int32 {
	max := int32(1)<<(bitsPerSample-1) - 1
	min := -max - 1
	return func(sampleNum, channel int) int32 {
		v := int32(sampleNum*7+channel*1000) - 200000
		if v > max {
			v = max - int32(sampleNum%17)
		}
		if v < min {
			v = min + int32(sampleNum%17)
		}
		return v
	}
}

// decodeAll decodes the whole stream and returns interleaved PCM in the same
// layout as TrackView.Read.
func decodeAll(r io.Reader, bytesPerSample, channels int) ([]byte, error) {
	stream, err := flac.New(r)
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		for i := 0; i < len(f.Subframes[0].Samples); i++ {
			for c := 0; c < channels; c++ {
				v := f.Subframes[c].Samples[i]
				for b := 0; b < bytesPerSample; b++ {
					out = append(out, byte(v>>(8*uint(b))))
				}
			}
		}
	}
}

// decodeSamples decodes the whole stream and returns samples[channel][sample].
func decodeSamples(r io.Reader, channels int) ([][]int32, error) {
	stream, err := flac.New(r)
	if err != nil {
		return nil, err
	}
	out := make([][]int32, channels)
	for {
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return nil, err
		}
		for c := 0; c < channels; c++ {
			out[c] = append(out[c], f.Subframes[c].Samples...)
		}
	}
}

// mustEncode encodes the fixture or panics.
func mustEncode(f *trackFixture) []byte {
	b, err := f.encode()
	if err != nil {
		panic(fmt.Sprintf("unable to encode fixture %q: %v", f.name, err))
	}
	return b
}
