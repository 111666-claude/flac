package flac_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/meta"
)

// --- [ helpers ] -------------------------------------------------------------

// rawBlock returns the raw bytes of a metadata block with the given type and
// body.
func rawBlock(typ byte, last bool, body []byte) []byte {
	b := make([]byte, 4+len(body))
	b[0] = typ & 0x7f
	if last {
		b[0] |= 0x80
	}
	b[1] = byte(len(body) >> 16)
	b[2] = byte(len(body) >> 8)
	b[3] = byte(len(body))
	copy(b[4:], body)
	return b
}

// streamInfoBody returns a valid StreamInfo block body.
func streamInfoBody() []byte {
	body := make([]byte, 34)
	binary.BigEndian.PutUint16(body[0:], 4096) // BlockSizeMin
	binary.BigEndian.PutUint16(body[2:], 4096) // BlockSizeMax
	// 20 bits: sample rate, 3 bits: nchannels-1, 5 bits: bps-1, 36 bits: nsamples.
	x := uint64(44100)<<44 | uint64(2-1)<<41 | uint64(16-1)<<36
	binary.BigEndian.PutUint64(body[10:], x)
	return body
}

// vorbisBody returns the body of a VorbisComment block.
func vorbisBody(vendor string, tags ...[2]string) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, uint32(len(vendor)))
	buf.WriteString(vendor)
	binary.Write(&buf, binary.LittleEndian, uint32(len(tags)))
	for _, tag := range tags {
		s := tag[0] + "=" + tag[1]
		binary.Write(&buf, binary.LittleEndian, uint32(len(s)))
		buf.WriteString(s)
	}
	return buf.Bytes()
}

// pictureBody returns the body of a Picture block.
func pictureBody(pic *meta.Picture) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, pic.Type)
	binary.Write(&buf, binary.BigEndian, uint32(len(pic.MIME)))
	buf.WriteString(pic.MIME)
	binary.Write(&buf, binary.BigEndian, uint32(len(pic.Desc)))
	buf.WriteString(pic.Desc)
	binary.Write(&buf, binary.BigEndian, pic.Width)
	binary.Write(&buf, binary.BigEndian, pic.Height)
	binary.Write(&buf, binary.BigEndian, pic.Depth)
	binary.Write(&buf, binary.BigEndian, pic.NPalColors)
	binary.Write(&buf, binary.BigEndian, uint32(len(pic.Data)))
	buf.Write(pic.Data)
	return buf.Bytes()
}

// frameData returns fixed dummy frame data of the given length.
func frameData(n int) []byte {
	data := make([]byte, n)
	for i := range data {
		data[i] = byte(i*31 + 7)
	}
	return data
}

// rawBlockInfo describes the layout of a metadata block within a FLAC stream.
type rawBlockInfo struct {
	typ   byte
	off   int // offset of block header
	total int // header + body length
	last  bool
}

// walkBlocks walks the metadata block headers of a FLAC stream (starting with
// the "fLaC" signature) and returns the block layouts and the offset of the
// first audio frame.
func walkBlocks(t *testing.T, data []byte) ([]rawBlockInfo, int) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("fLaC")) {
		t.Fatal("missing fLaC signature")
	}
	var blocks []rawBlockInfo
	off := 4
	for {
		if off+4 > len(data) {
			t.Fatal("truncated block header")
		}
		last := data[off]&0x80 != 0
		typ := data[off] & 0x7f
		length := int(data[off+1])<<16 | int(data[off+2])<<8 | int(data[off+3])
		blocks = append(blocks, rawBlockInfo{typ: typ, off: off, total: 4 + length, last: last})
		off += 4 + length
		if last {
			return blocks, off
		}
	}
}

// buildSyntheticFLAC assembles a FLAC stream from the given metadata blocks
// (a StreamInfo block is prepended) followed by fixed frame data.
func buildSyntheticFLAC(frames []byte, blocks ...[]byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("fLaC")
	buf.Write(rawBlock(0, false, streamInfoBody()))
	for _, b := range blocks {
		buf.Write(b)
	}
	buf.Write(frames)
	return buf.Bytes()
}

// writeTemp writes data to a temporary file and returns its path.
func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.flac")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// openRW opens a file for in-place update.
func openRW(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// --- [ tests ] ---------------------------------------------------------------

// TestUpdateMetadataInPlace verifies that an in-place update of the
// VorbisComment and Picture blocks preserves unknown blocks verbatim, keeps
// the audio data offset unchanged, and produces a stream readable by the
// regular parser.
func TestUpdateMetadataInPlace(t *testing.T) {
	frames := frameData(512)
	appBody := append([]byte("TEST"), []byte{1, 2, 3, 4, 5}...)
	unknownBody := []byte{0xde, 0xad, 0xbe, 0xef, 0x42}
	orig := buildSyntheticFLAC(frames,
		rawBlock(2, false, appBody),       // application
		rawBlock(100, false, unknownBody), // unknown (reserved) block
		rawBlock(4, false, vorbisBody("orig", [2]string{"TITLE", "Old Title"})),
		rawBlock(1, true, make([]byte, 4096)), // padding
	)
	path := writeTemp(t, orig)

	_, dataStartBefore := walkBlocks(t, orig)

	comment := &meta.VorbisComment{
		Vendor: "flac",
		Tags: [][2]string{
			{"TITLE", "New Title"},
			{"ARTIST", "Someone"},
		},
	}
	pic := &meta.Picture{
		Type:   3, // cover (front)
		MIME:   "image/png",
		Desc:   "cover",
		Width:  10,
		Height: 10,
		Depth:  24,
		Data:   frameData(1000),
	}
	f := openRW(t, path)
	if err := flac.UpdateMetadata(f, &flac.MetadataUpdate{VorbisComment: comment, Picture: pic}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The file size and the audio data offset must be unchanged, and the
	// frame region must be byte-identical.
	if len(updated) != len(orig) {
		t.Fatalf("file size changed: %d -> %d", len(orig), len(updated))
	}
	blocks, dataStartAfter := walkBlocks(t, updated)
	if dataStartAfter != dataStartBefore {
		t.Fatalf("audio data offset changed: %d -> %d", dataStartBefore, dataStartAfter)
	}
	if sha256.Sum256(updated[dataStartAfter:]) != sha256.Sum256(frames) {
		t.Fatal("frame region modified")
	}

	// The application and unknown blocks precede the replaced blocks, so
	// their bytes must be preserved verbatim at their original offsets.
	appOff := 4 + 4 + 34 // after signature and StreamInfo
	if !bytes.Equal(updated[appOff:appOff+len(appBody)+4], rawBlock(2, false, appBody)) {
		t.Fatal("application block not preserved verbatim")
	}
	unkOff := appOff + 4 + len(appBody)
	if !bytes.Equal(updated[unkOff:unkOff+len(unknownBody)+4], rawBlock(100, false, unknownBody)) {
		t.Fatal("unknown block not preserved verbatim")
	}

	// Exactly one block, the last one, must have the last-block flag set.
	for i, b := range blocks {
		if want := i == len(blocks)-1; b.last != want {
			t.Fatalf("block %d (type %d): IsLast=%v, want %v", i, b.typ, b.last, want)
		}
	}

	// The regular parser must be able to read back the updated stream.
	stream, err := flac.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile after update: %v", err)
	}
	defer stream.Close()
	wantTypes := []meta.Type{
		meta.TypeApplication,
		meta.Type(100), // unknown block preserved in order
		meta.TypeVorbisComment,
		meta.TypePicture,
		meta.TypePadding,
	}
	if len(stream.Blocks) != len(wantTypes) {
		t.Fatalf("got %d blocks, want %d", len(stream.Blocks), len(wantTypes))
	}
	for i, want := range wantTypes {
		if stream.Blocks[i].Header.Type != want {
			t.Fatalf("block %d: got type %v, want %v", i, stream.Blocks[i].Header.Type, want)
		}
	}
	// Verify the updated tags.
	vc := stream.Blocks[2].Body.(*meta.VorbisComment)
	if vc.Vendor != "flac" {
		t.Errorf("vendor: got %q, want %q", vc.Vendor, "flac")
	}
	wantTags := map[string]string{"TITLE": "New Title", "ARTIST": "Someone"}
	if len(vc.Tags) != len(wantTags) {
		t.Fatalf("got %d tags, want %d", len(vc.Tags), len(wantTags))
	}
	for _, tag := range vc.Tags {
		if wantTags[tag[0]] != tag[1] {
			t.Errorf("tag %q: got %q, want %q", tag[0], tag[1], wantTags[tag[0]])
		}
	}
	// Verify the updated picture.
	gotPic := stream.Blocks[3].Body.(*meta.Picture)
	if gotPic.Type != 3 || gotPic.MIME != "image/png" || gotPic.Desc != "cover" ||
		gotPic.Width != 10 || gotPic.Height != 10 || gotPic.Depth != 24 ||
		!bytes.Equal(gotPic.Data, pic.Data) {
		t.Error("picture block mismatch after update")
	}
}

// TestUpdateMetadataRealFile verifies an in-place update of a real FLAC file,
// checking that the audio frames remain untouched and decodable.
func TestUpdateMetadataRealFile(t *testing.T) {
	src, err := os.ReadFile("testdata/love.flac")
	if err != nil {
		t.Fatal(err)
	}
	_, dataStartBefore := walkBlocks(t, src)
	framesHash := sha256.Sum256(src[dataStartBefore:])

	path := filepath.Join(t.TempDir(), "love.flac")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}

	comment := &meta.VorbisComment{
		Vendor: "flac",
		Tags: [][2]string{
			{"TITLE", "Love " + strings.Repeat("~", 2000)},
			{"ARTIST", "Test Artist"},
		},
	}
	pic := &meta.Picture{
		Type: 3,
		MIME: "image/jpeg",
		Data: frameData(500),
	}
	f := openRW(t, path)
	if err := flac.UpdateMetadata(f, &flac.MetadataUpdate{VorbisComment: comment, Picture: pic}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != len(src) {
		t.Fatalf("file size changed: %d -> %d", len(src), len(updated))
	}
	_, dataStartAfter := walkBlocks(t, updated)
	if dataStartAfter != dataStartBefore {
		t.Fatalf("audio data offset changed: %d -> %d", dataStartBefore, dataStartAfter)
	}
	if sha256.Sum256(updated[dataStartAfter:]) != framesHash {
		t.Fatal("frame region modified")
	}

	// The updated stream must parse and decode.
	stream, err := flac.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile after update: %v", err)
	}
	defer stream.Close()
	var gotVC *meta.VorbisComment
	var gotPic *meta.Picture
	for _, block := range stream.Blocks {
		switch body := block.Body.(type) {
		case *meta.VorbisComment:
			gotVC = body
		case *meta.Picture:
			gotPic = body
		}
	}
	if gotVC == nil || gotPic == nil {
		t.Fatal("updated vorbis comment or picture block missing")
	}
	if gotVC.Tags[0][0] != "TITLE" || gotVC.Tags[0][1] != comment.Tags[0][1] {
		t.Errorf("title tag mismatch: got %v", gotVC.Tags[0])
	}
	if !bytes.Equal(gotPic.Data, pic.Data) {
		t.Error("picture data mismatch")
	}
	if _, err := stream.ParseNext(); err != nil {
		t.Fatalf("unable to decode first audio frame after update: %v", err)
	}
}

// TestUpdateMetadataNoPadding verifies that growing metadata without any
// padding block fails with ErrNoPadding and leaves the file unmodified.
func TestUpdateMetadataNoPadding(t *testing.T) {
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(4, true, vorbisBody("orig", [2]string{"TITLE", "Old"})),
	)
	path := writeTemp(t, orig)

	f := openRW(t, path)
	comment := &meta.VorbisComment{
		Vendor: "flac",
		Tags:   [][2]string{{"TITLE", "A much longer title than before"}},
	}
	err := flac.UpdateMetadata(f, &flac.MetadataUpdate{VorbisComment: comment})
	if !errors.Is(err, flac.ErrNoPadding) {
		t.Fatalf("got %v, want ErrNoPadding", err)
	}
	assertFileUnchanged(t, path, orig)
}

// TestUpdateMetadataInsufficientCapacity verifies that metadata not fitting
// within the available padding fails with ErrInsufficientCapacity and leaves
// the file unmodified.
func TestUpdateMetadataInsufficientCapacity(t *testing.T) {
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(4, false, vorbisBody("orig", [2]string{"TITLE", "Old"})),
		rawBlock(1, true, make([]byte, 16)), // small padding
	)
	path := writeTemp(t, orig)

	f := openRW(t, path)
	pic := &meta.Picture{Type: 3, MIME: "image/png", Data: frameData(100000)}
	err := flac.UpdateMetadata(f, &flac.MetadataUpdate{Picture: pic})
	if !errors.Is(err, flac.ErrInsufficientCapacity) {
		t.Fatalf("got %v, want ErrInsufficientCapacity", err)
	}
	assertFileUnchanged(t, path, orig)
}

// TestUpdateMetadataDuplicateBlocks verifies that an update is refused when
// the stream contains multiple conflicting blocks of a type subject to
// update.
func TestUpdateMetadataDuplicateBlocks(t *testing.T) {
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(4, false, vorbisBody("one", [2]string{"TITLE", "First"})),
		rawBlock(4, false, vorbisBody("two", [2]string{"TITLE", "Second"})),
		rawBlock(6, false, pictureBody(&meta.Picture{Type: 3, MIME: "image/png", Data: []byte{1}})),
		rawBlock(6, false, pictureBody(&meta.Picture{Type: 4, MIME: "image/png", Data: []byte{2}})),
		rawBlock(1, true, make([]byte, 4096)),
	)
	path := writeTemp(t, orig)

	f := openRW(t, path)
	comment := &meta.VorbisComment{Vendor: "flac", Tags: [][2]string{{"TITLE", "New"}}}
	err := flac.UpdateMetadata(f, &flac.MetadataUpdate{VorbisComment: comment})
	if !errors.Is(err, flac.ErrDuplicateBlock) {
		t.Fatalf("vorbis comment: got %v, want ErrDuplicateBlock", err)
	}
	pic := &meta.Picture{Type: 3, MIME: "image/png", Data: []byte{3}}
	err = flac.UpdateMetadata(f, &flac.MetadataUpdate{Picture: pic})
	if !errors.Is(err, flac.ErrDuplicateBlock) {
		t.Fatalf("picture: got %v, want ErrDuplicateBlock", err)
	}
	assertFileUnchanged(t, path, orig)
}

// TestUpdateMetadataLeftoverTooSmall verifies the error reported when the
// leftover capacity cannot hold a padding block header.
func TestUpdateMetadataLeftoverTooSmall(t *testing.T) {
	// vorbisTotal computes the total size (header included) of an encoded
	// VorbisComment block.
	vorbisTotal := func(vendor string, tags ...[2]string) int {
		total := 4 + 4 + len(vendor) + 4
		for _, tag := range tags {
			total += 4 + len(tag[0]) + 1 + len(tag[1])
		}
		return total
	}
	origVC := [2]string{"TITLE", "Old"}
	const padBody = 100
	capacity := vorbisTotal("orig", origVC) + 4 + padBody
	// Craft an updated vorbis comment 2 bytes smaller than the capacity, so
	// that the leftover (2 bytes) cannot hold a padding block header.
	n := capacity - 2 - vorbisTotal("v", [2]string{"A", ""})
	if n < 0 {
		t.Fatalf("test setup: negative tag length %d", n)
	}
	comment := &meta.VorbisComment{
		Vendor: "v",
		Tags:   [][2]string{{"A", strings.Repeat("x", n)}},
	}
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(4, false, vorbisBody("orig", origVC)),
		rawBlock(1, true, make([]byte, padBody)),
	)
	path := writeTemp(t, orig)

	f := openRW(t, path)
	err := flac.UpdateMetadata(f, &flac.MetadataUpdate{VorbisComment: comment})
	if !errors.Is(err, flac.ErrInsufficientCapacity) {
		t.Fatalf("got %v, want ErrInsufficientCapacity", err)
	}
	assertFileUnchanged(t, path, orig)
}

// errInjected is returned by failingWriter after the configured number of
// bytes.
var errInjected = errors.New("injected write failure")

// failingWriter fails the first write after writing limit bytes, simulating
// an interrupted write. Subsequent writes (e.g. the rollback) succeed.
type failingWriter struct {
	f      *os.File
	limit  int
	failed bool
}

func (w *failingWriter) Read(p []byte) (int, error) { return w.f.Read(p) }
func (w *failingWriter) Seek(off int64, whence int) (int64, error) {
	return w.f.Seek(off, whence)
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if !w.failed {
		w.failed = true
		if w.limit < len(p) {
			n, _ := w.f.Write(p[:w.limit])
			return n, errInjected
		}
		return 0, errInjected
	}
	return w.f.Write(p)
}

// TestUpdateMetadataWriteFailureRollback verifies that an interrupted write
// is reported and the original metadata region is restored.
func TestUpdateMetadataWriteFailureRollback(t *testing.T) {
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(4, false, vorbisBody("orig", [2]string{"TITLE", "Old"})),
		rawBlock(1, true, make([]byte, 4096)),
	)
	path := writeTemp(t, orig)

	f := openRW(t, path)
	// Fail midway through the write of the new metadata region.
	w := &failingWriter{f: f, limit: 100}
	comment := &meta.VorbisComment{
		Vendor: "flac",
		Tags:   [][2]string{{"TITLE", "New Title"}},
	}
	err := flac.UpdateMetadata(w, &flac.MetadataUpdate{VorbisComment: comment})
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("got %v, want write failure with rollback", err)
	}
	assertFileUnchanged(t, path, orig)
}

// notSeekable is an io.ReadWriteSeeker whose Seek always fails.
type notSeekable struct{}

func (notSeekable) Read(p []byte) (int, error)  { return 0, errors.New("no read") }
func (notSeekable) Write(p []byte) (int, error) { return 0, errors.New("no write") }
func (notSeekable) Seek(off int64, whence int) (int64, error) {
	return 0, errors.New("not seekable")
}

// TestUpdateMetadataNotSeekable verifies a clear error for non-seekable
// input.
func TestUpdateMetadataNotSeekable(t *testing.T) {
	comment := &meta.VorbisComment{Vendor: "flac"}
	err := flac.UpdateMetadata(notSeekable{}, &flac.MetadataUpdate{VorbisComment: comment})
	if err == nil || !strings.Contains(err.Error(), "not seekable") {
		t.Fatalf("got %v, want not-seekable error", err)
	}
}

// TestUpdateMetadataNoop verifies that an empty update leaves the stream
// untouched.
func TestUpdateMetadataNoop(t *testing.T) {
	orig := buildSyntheticFLAC(frameData(256),
		rawBlock(1, true, make([]byte, 64)),
	)
	path := writeTemp(t, orig)
	f := openRW(t, path)
	if err := flac.UpdateMetadata(f, nil); err != nil {
		t.Fatal(err)
	}
	if err := flac.UpdateMetadata(f, &flac.MetadataUpdate{}); err != nil {
		t.Fatal(err)
	}
	assertFileUnchanged(t, path, orig)
}

// assertFileUnchanged verifies that the file at path still contains orig.
func assertFileUnchanged(t *testing.T, path string, orig []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("file modified despite failed update")
	}
}
