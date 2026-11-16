package flac_test

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/mewkiz/flac"
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

func ExampleStream_OpenTrack() {
	// Cue sheet aware streams are opened with NewSeek, which parses all
	// metadata blocks including the CueSheet.
	f, err := os.Open("meta/testdata/input-SCPAP.flac")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	stream, err := flac.NewSeek(f)
	if err != nil {
		log.Fatal(err)
	}

	// Track 2 of this file starts at sample 2940; the lead-out track starts at
	// sample 5880.
	track, err := stream.OpenTrack(2)
	if err != nil {
		log.Fatal(err)
	}
	defer track.Close()

	fmt.Printf("track %d: samples [%d, %d), %d samples, %d bytes\n",
		track.Number(), track.Start(), track.End(), track.NSamples(), track.Length())

	// Seek to the beginning of the track and read its first sample frame.
	if _, err := track.SeekSample(0, io.SeekStart); err != nil {
		log.Fatal(err)
	}
	samples := [][]int32{make([]int32, 1), make([]int32, 1)}
	if _, err := track.ReadSamples(samples); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("first sample: left=%d right=%d\n", samples[0][0], samples[1][0])

	// Output:
	// track 2: samples [2940, 5880), 2940 samples, 11760 bytes
	// first sample: left=0 right=0
}
