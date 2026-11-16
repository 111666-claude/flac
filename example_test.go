package flac_test

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

func ExampleParseFile() {
	// Parse metadata of love.flac
	stream, err := flac.ParseFile("testdata/love.flac")
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	fmt.Printf("unencoded audio md5sum: %032x\n", stream.Info.MD5sum[:])
	for i, block := range stream.Blocks {
		fmt.Printf("block %d: %v\n", i, block.Type)
	}
	// Output:
	// unencoded audio md5sum: bdf6f7d31f77cb696a02b2192d192a89
	// block 0: seek table
	// block 1: vorbis comment
	// block 2: padding
}

func ExampleOpen() {
	// Open love.flac for audio streaming without parsing metadata.
	stream, err := flac.Open("testdata/love.flac")
	if err != nil {
		log.Fatal(err)
	}
	defer stream.Close()

	// Parse audio samples and verify the MD5 signature of the decoded audio
	// samples.
	md5sum := md5.New()
	for {
		// Parse one frame of audio samples at the time, each frame containing one
		// subframe per audio channel.
		frame, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Fatal(err)
		}
		frame.Hash(md5sum)

		// Print first three samples from each channel of the first five frames.
		if frame.Num < 5 {
			fmt.Printf("frame %d\n", frame.Num)
			for i, subframe := range frame.Subframes {
				fmt.Printf("  subframe %d\n", i)
				for j, sample := range subframe.Samples {
					if j >= 3 {
						break
					}
					fmt.Printf("    sample %d: %v\n", j, sample)
				}
			}
		}
	}
	fmt.Println()

	got, want := md5sum.Sum(nil), stream.Info.MD5sum[:]
	fmt.Println("decoded audio md5sum valid:", bytes.Equal(got, want))
	// Output:
	// frame 0
	//   subframe 0
	//     sample 0: 126
	//     sample 1: 126
	//     sample 2: 126
	//   subframe 1
	//     sample 0: 126
	//     sample 1: 126
	//     sample 2: 126
	// frame 1
	//   subframe 0
	//     sample 0: 126
	//     sample 1: 126
	//     sample 2: 126
	//   subframe 1
	//     sample 0: 126
	//     sample 1: 126
	//     sample 2: 126
	// frame 2
	//   subframe 0
	//     sample 0: 121
	//     sample 1: 130
	//     sample 2: 137
	//   subframe 1
	//     sample 0: 121
	//     sample 1: 130
	//     sample 2: 137
	// frame 3
	//   subframe 0
	//     sample 0: -9501
	//     sample 1: -6912
	//     sample 2: -3916
	//   subframe 1
	//     sample 0: -9501
	//     sample 1: -6912
	//     sample 2: -3916
	// frame 4
	//   subframe 0
	//     sample 0: 513
	//     sample 1: 206
	//     sample 2: 152
	//   subframe 1
	//     sample 0: 513
	//     sample 1: 206
	//     sample 2: 152
	//
	// decoded audio md5sum valid: true
}

func ExampleEncoder_EnableSeekTable() {
	// Encode a synthetic stream to a rewindable in-memory writer, and generate
	// a seek table with at most 16 index points.
	info := &meta.StreamInfo{
		BlockSizeMin:  1024,
		BlockSizeMax:  1024,
		SampleRate:    44100,
		NChannels:     1,
		BitsPerSample: 16,
	}
	// A seek table is reserved while writing the metadata section and filled
	// in when the encoder is closed, and therefore requires an io.WriteSeeker.
	// *bytes.Buffer is not one; use a file or another rewindable writer.
	ws := &exampleWriteSeeker{}
	enc, err := flac.NewEncoder(ws, info)
	if err != nil {
		log.Fatal(err)
	}
	if err := enc.EnableSeekTable(16); err != nil {
		log.Fatal(err)
	}
	const nframes = 40
	for n := 0; n < nframes; n++ {
		f := &frame.Frame{Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         1024,
			SampleRate:        44100,
			Channels:          frame.ChannelsMono,
			BitsPerSample:     16,
		}}
		samples := make([]int32, 1024)
		f.Subframes = []*frame.Subframe{{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   samples,
			NSamples:  len(samples),
		}}
		if err := enc.WriteFrame(f); err != nil {
			log.Fatal(err)
		}
	}
	if err := enc.Close(); err != nil {
		log.Fatal(err)
	}

	// Read back the generated seek table.
	stream, err := flac.Parse(bytes.NewReader(ws.buf))
	if err != nil {
		log.Fatal(err)
	}
	for _, block := range stream.Blocks {
		if table, ok := block.Body.(*meta.SeekTable); ok {
			npoints := 0
			for _, point := range table.Points {
				if point.SampleNum == meta.PlaceholderPoint {
					continue
				}
				npoints++
			}
			fmt.Printf("seek points: %d\n", npoints)
		}
	}
	// Output:
	// seek points: 16
}

// exampleWriteSeeker is an in-memory io.WriteSeeker for the example.
type exampleWriteSeeker struct {
	buf []byte
	pos int64
}

func (w *exampleWriteSeeker) Write(p []byte) (int, error) {
	end := w.pos + int64(len(p))
	if end > int64(len(w.buf)) {
		w.buf = append(w.buf, make([]byte, end-int64(len(w.buf)))...)
	}
	copy(w.buf[w.pos:], p)
	w.pos = end
	return len(p), nil
}

func (w *exampleWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		w.pos = offset
	case io.SeekCurrent:
		w.pos += offset
	case io.SeekEnd:
		w.pos = int64(len(w.buf)) + offset
	}
	return w.pos, nil
}
