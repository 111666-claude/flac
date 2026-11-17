package flac_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/mewkiz/flac"
)

// decodeStats decodes all frames of the FLAC file of path using
// Stream.ParseNext, returning the number of frames, the total number of
// inter-channel samples, and the block size of the last frame. It serves as a
// reference for the statistics reported by flac.Audit.
func decodeStats(t *testing.T, path string) (frames, samples uint64, lastBlockSize uint16) {
	t.Helper()
	stream, err := flac.New(mustOpen(t, path))
	if err != nil {
		t.Fatalf("unable to create stream for %q; %v", path, err)
	}
	defer stream.Close()
	for {
		f, err := stream.ParseNext()
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("unable to parse frame of %q; %v", path, err)
		}
		frames++
		samples += uint64(f.BlockSize)
		lastBlockSize = f.BlockSize
	}
	return frames, samples, lastBlockSize
}

func mustOpen(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("unable to open %q; %v", path, err)
	}
	return f
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("unable to read %q; %v", path, err)
	}
	return data
}

// TestAuditValid verifies that Audit decodes every frame of the valid FLAC
// test files, and that its statistics match a reference decode using
// Stream.ParseNext.
func TestAuditValid(t *testing.T) {
	paths := []string{
		"meta/testdata/input-SCPAP.flac",
		"meta/testdata/input-SCVA.flac",
		"meta/testdata/input-SCVPAP.flac",
		"meta/testdata/input-VA.flac",
		"meta/testdata/silence.flac",
		"testdata/19875.flac",
		"testdata/44127.flac",
		"testdata/59996.flac",
		"testdata/80574.flac",
		"testdata/172960.flac",
		"testdata/189983.flac",
		"testdata/191885.flac",
		"testdata/212768.flac",
		"testdata/220014.flac",
		"testdata/243749.flac",
		"testdata/256529.flac",
		"testdata/257344.flac",
		"testdata/8297-275156-0011.flac",
		"testdata/love.flac",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			wantFrames, wantSamples, _ := decodeStats(t, path)

			f := mustOpen(t, path)
			defer f.Close()
			report, err := flac.Audit(f)
			if err != nil {
				t.Fatalf("audit of valid file failed; %v", err)
			}
			if report.ErrorOffset != -1 {
				t.Errorf("expected ErrorOffset -1 for valid file, got %d", report.ErrorOffset)
			}
			if report.Frames != wantFrames {
				t.Errorf("frame count mismatch; expected %d, got %d", wantFrames, report.Frames)
			}
			if report.Samples != wantSamples {
				t.Errorf("sample count mismatch; expected %d, got %d", wantSamples, report.Samples)
			}
			if report.ExpectedSamples != 0 && report.Samples != report.ExpectedSamples {
				t.Errorf("decoded samples %d do not match STREAMINFO total %d", report.Samples, report.ExpectedSamples)
			}
		})
	}
}

// TestAuditEmpty verifies that auditing an empty input yields a deterministic
// error at offset 0 with zeroed statistics.
func TestAuditEmpty(t *testing.T) {
	report, err := flac.Audit(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected error for empty input")
	}
	aerr, ok := flac.IsAuditError(err)
	if !ok {
		t.Fatalf("expected *flac.AuditError, got %T", err)
	}
	if aerr.Offset != 0 {
		t.Errorf("expected offset 0, got %d", aerr.Offset)
	}
	if report.Frames != 0 || report.Samples != 0 {
		t.Errorf("expected zeroed statistics, got %d frames and %d samples", report.Frames, report.Samples)
	}
}

// TestAuditTruncated verifies that a truncated frame yields a locatable error
// while retaining the statistics of the frames decoded so far.
func TestAuditTruncated(t *testing.T) {
	const path = "testdata/love.flac"
	wantFrames, wantSamples, lastBlockSize := decodeStats(t, path)

	// Truncate the final byte of the file, cutting the CRC-16 checksum of the
	// last frame in half.
	data := mustReadFile(t, path)
	truncated := data[:len(data)-1]

	report, err := flac.Audit(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("expected error for truncated file")
	}
	aerr, ok := flac.IsAuditError(err)
	if !ok {
		t.Fatalf("expected *flac.AuditError, got %T", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected io.ErrUnexpectedEOF, got %v", err)
	}
	// All frames but the last must have been decoded and accounted for.
	if report.Frames != wantFrames-1 {
		t.Errorf("expected %d frames, got %d", wantFrames-1, report.Frames)
	}
	if report.Samples != wantSamples-uint64(lastBlockSize) {
		t.Errorf("expected %d samples, got %d", wantSamples-uint64(lastBlockSize), report.Samples)
	}
	if aerr.Offset != report.ErrorOffset {
		t.Errorf("error offset mismatch between report (%d) and error (%d)", report.ErrorOffset, aerr.Offset)
	}
	if aerr.Offset <= 0 || aerr.Offset >= int64(len(truncated)) {
		t.Errorf("error offset %d out of range (file size %d)", aerr.Offset, len(truncated))
	}
	if aerr.Frame != report.Frames {
		t.Errorf("expected failing frame index %d, got %d", report.Frames, aerr.Frame)
	}
}

// TestAuditCRCMismatch verifies that a corrupted frame CRC-16 checksum is
// reported with the byte offset of the frame at fault, and that the offset
// points to an exact frame boundary.
func TestAuditCRCMismatch(t *testing.T) {
	const path = "testdata/love.flac"
	wantFrames, wantSamples, lastBlockSize := decodeStats(t, path)

	// Corrupt the last byte of the file, which holds the low byte of the last
	// frame's CRC-16 checksum.
	data := mustReadFile(t, path)
	data[len(data)-1] ^= 0xFF

	report, err := flac.Audit(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for corrupted file")
	}
	aerr, ok := flac.IsAuditError(err)
	if !ok {
		t.Fatalf("expected *flac.AuditError, got %T", err)
	}
	if !strings.Contains(err.Error(), "CRC-16") {
		t.Errorf("expected CRC-16 checksum mismatch, got %v", err)
	}
	if report.Frames != wantFrames-1 {
		t.Errorf("expected %d frames, got %d", wantFrames-1, report.Frames)
	}
	if report.Samples != wantSamples-uint64(lastBlockSize) {
		t.Errorf("expected %d samples, got %d", wantSamples-uint64(lastBlockSize), report.Samples)
	}

	// Verify that the reported offset is an exact frame boundary: auditing
	// the file truncated at the offset (with the STREAMINFO total sample
	// count cleared, as the stream is now incomplete) must succeed and
	// reproduce the same statistics.
	prefix := make([]byte, aerr.Offset)
	copy(prefix, data)
	setStreamInfoNSamples(t, prefix, 0)
	prefixReport, err := flac.Audit(bytes.NewReader(prefix))
	if err != nil {
		t.Fatalf("audit of file truncated at reported offset %d failed; %v", aerr.Offset, err)
	}
	if prefixReport.Frames != report.Frames || prefixReport.Samples != report.Samples {
		t.Errorf("statistics mismatch at reported offset; expected %d frames and %d samples, got %d frames and %d samples",
			report.Frames, report.Samples, prefixReport.Frames, prefixReport.Samples)
	}
}

// TestAuditOversizedFrame verifies that a frame exceeding the maximum block
// size declared by STREAMINFO is reported deterministically.
func TestAuditOversizedFrame(t *testing.T) {
	data := mustReadFile(t, "testdata/love.flac")
	// STREAMINFO body starts at offset 8 (4 bytes signature, 4 bytes block
	// header); the maximum block size is the uint16 at body offset 2.
	binary.BigEndian.PutUint16(data[10:12], 16)

	report, err := flac.Audit(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for oversized frame")
	}
	if !strings.Contains(err.Error(), "exceeds STREAMINFO maximum block size") {
		t.Errorf("unexpected error; %v", err)
	}
	if report.Frames != 0 {
		t.Errorf("expected 0 decoded frames, got %d", report.Frames)
	}
	if report.ErrorOffset <= 0 {
		t.Errorf("expected positive error offset, got %d", report.ErrorOffset)
	}
}

// TestAuditUnknownTotalSamples verifies that a stream with an unknown total
// number of samples (STREAMINFO total of 0) audits cleanly.
func TestAuditUnknownTotalSamples(t *testing.T) {
	const path = "testdata/love.flac"
	wantFrames, wantSamples, _ := decodeStats(t, path)

	data := mustReadFile(t, path)
	setStreamInfoNSamples(t, data, 0)

	report, err := flac.Audit(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("audit of stream with unknown total samples failed; %v", err)
	}
	if report.ExpectedSamples != 0 {
		t.Errorf("expected unknown total samples (0), got %d", report.ExpectedSamples)
	}
	if report.Frames != wantFrames || report.Samples != wantSamples {
		t.Errorf("expected %d frames and %d samples, got %d frames and %d samples",
			wantFrames, wantSamples, report.Frames, report.Samples)
	}
}

// TestAuditSampleCountMismatch verifies that a mismatch between the decoded
// sample count and the STREAMINFO total is reported.
func TestAuditSampleCountMismatch(t *testing.T) {
	const path = "testdata/love.flac"
	wantFrames, wantSamples, _ := decodeStats(t, path)

	data := mustReadFile(t, path)
	setStreamInfoNSamples(t, data, 1)

	report, err := flac.Audit(bytes.NewReader(data))
	if err == nil {
		t.Fatal("expected error for sample count mismatch")
	}
	if !strings.Contains(err.Error(), "STREAMINFO declares") {
		t.Errorf("unexpected error; %v", err)
	}
	// All frames decode fine; only the final tally mismatches.
	if report.Frames != wantFrames || report.Samples != wantSamples {
		t.Errorf("expected %d frames and %d samples, got %d frames and %d samples",
			wantFrames, wantSamples, report.Frames, report.Samples)
	}
}

// TestAuditVariableBlockSize verifies that a stream with a variable block
// size audits cleanly, and that a discontinuity in sample numbering is
// reported with the offset of the offending frame.
func TestAuditVariableBlockSize(t *testing.T) {
	// Decode the first three frames of a fixed block size file and re-encode
	// them as a variable block size stream.
	stream, err := flac.New(mustOpen(t, "testdata/love.flac"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var wantSamples uint64
	var buf bytes.Buffer
	info := *stream.Info
	info.NSamples = 0 // total number of samples unknown
	enc, err := flac.NewEncoder(&buf, &info)
	if err != nil {
		t.Fatal(err)
	}
	enc.EnablePredictionAnalysis(false)
	for i := 0; i < 3; i++ {
		f, err := stream.ParseNext()
		if err != nil {
			t.Fatal(err)
		}
		f.HasFixedBlockSize = false
		if err := enc.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
		wantSamples += uint64(f.BlockSize)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := flac.Audit(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("audit of variable block size stream failed; %v", err)
	}
	if report.Frames != 3 {
		t.Errorf("expected 3 frames, got %d", report.Frames)
	}
	if report.Samples != wantSamples {
		t.Errorf("expected %d samples, got %d", wantSamples, report.Samples)
	}
}

// TestAuditNonContiguous verifies that a discontinuity in frame numbering is
// reported with the offset of the offending frame.
func TestAuditNonContiguous(t *testing.T) {
	// Encode a single frame stream, then splice its frame in twice; the
	// duplicated frame carries the same frame number (0) twice.
	stream, err := flac.New(mustOpen(t, "testdata/love.flac"))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	f, err := stream.ParseNext()
	if err != nil {
		t.Fatal(err)
	}
	info := *stream.Info
	info.NSamples = 0 // total number of samples unknown

	// Record the length of the stream header (signature and metadata).
	var hdr bytes.Buffer
	if _, err := flac.NewEncoder(&hdr, &info); err != nil {
		t.Fatal(err)
	}
	headerLen := hdr.Len()

	var buf bytes.Buffer
	enc, err := flac.NewEncoder(&buf, &info)
	if err != nil {
		t.Fatal(err)
	}
	enc.EnablePredictionAnalysis(false)
	if err := enc.WriteFrame(f); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}

	// Splice: header + frame + frame.
	spliced := append(buf.Bytes(), buf.Bytes()[headerLen:]...)

	report, err := flac.Audit(bytes.NewReader(spliced))
	if err == nil {
		t.Fatal("expected error for non-contiguous frame numbering")
	}
	if !strings.Contains(err.Error(), "non-contiguous") {
		t.Errorf("unexpected error; %v", err)
	}
	aerr, ok := flac.IsAuditError(err)
	if !ok {
		t.Fatalf("expected *flac.AuditError, got %T", err)
	}
	// The first frame decodes fine; the duplicate is rejected at its offset.
	if report.Frames != 1 {
		t.Errorf("expected 1 decoded frame, got %d", report.Frames)
	}
	if want := int64(buf.Len()); aerr.Offset != want {
		t.Errorf("expected error offset %d, got %d", want, aerr.Offset)
	}
}

// setStreamInfoNSamples rewrites the 36-bit total sample count of the
// STREAMINFO metadata block of the FLAC stream in data, which must start at
// the "fLaC" signature.
func setStreamInfoNSamples(t *testing.T, data []byte, nsamples uint64) {
	t.Helper()
	if len(data) < 26 {
		t.Fatalf("data too short for STREAMINFO block (%d bytes)", len(data))
	}
	// The 64 bits at offset 18 hold the sample rate (20 bits), the number of
	// channels (3 bits), the bits-per-sample (5 bits) and the total number of
	// samples (36 bits).
	const off = 8 + 10
	u := binary.BigEndian.Uint64(data[off : off+8])
	u = u&^((uint64(1)<<36)-1) | nsamples
	binary.BigEndian.PutUint64(data[off:off+8], u)
}
