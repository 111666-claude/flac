package flac_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/meta"
)

// silenceFixture contains a SeekTable, a VorbisComment, a Picture and a
// Padding metadata block.
const silenceFixture = "meta/testdata/silence.flac"

// blockSpan records the type and the byte range (header and body) of a
// metadata block within a FLAC stream.
type blockSpan struct {
	typ        meta.Type
	isLast     bool
	start, end int
}

// metadataSpans returns the byte offset of the FLAC signature end, the spans
// of the metadata blocks, and the offset of the first audio frame of the
// given FLAC stream.
func metadataSpans(t *testing.T, data []byte) (int, []blockSpan, int) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte("fLaC")) {
		t.Fatal("invalid FLAC signature")
	}
	r := bytes.NewReader(data)
	if _, err := r.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var spans []blockSpan
	for {
		start := len(data) - r.Len()
		block, err := meta.New(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := block.Skip(); err != nil {
			t.Fatal(err)
		}
		end := len(data) - r.Len()
		spans = append(spans, blockSpan{typ: block.Type, isLast: block.IsLast, start: start, end: end})
		if block.IsLast {
			return 4, spans, end
		}
	}
}

// frameBytes returns the raw audio frame bytes of the given FLAC stream.
func frameBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	_, _, dataStart := metadataSpans(t, data)
	return data[dataStart:]
}

// decodeAll decodes all audio frames of the given FLAC stream and returns
// their samples, one slice per channel.
func decodeAll(t *testing.T, data []byte) [][]int32 {
	t.Helper()
	stream, err := flac.Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var channels [][]int32
	for {
		f, err := stream.ParseNext()
		if err == io.EOF {
			return channels
		}
		if err != nil {
			t.Fatal(err)
		}
		for i := range f.Subframes {
			for len(channels) <= i {
				channels = append(channels, nil)
			}
			channels[i] = append(channels[i], f.Subframes[i].Samples...)
		}
	}
}

// blockTypes returns the types of the metadata blocks of the given FLAC
// stream, starting with StreamInfo.
func blockTypes(t *testing.T, data []byte) []meta.Type {
	t.Helper()
	_, spans, _ := metadataSpans(t, data)
	types := make([]meta.Type, len(spans))
	for i, span := range spans {
		types[i] = span.typ
	}
	return types
}

func TestEditMetadata(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the VorbisComment (new title) and the Picture (new cover).
	comment := &meta.VorbisComment{
		Vendor: "flac.EditMetadata",
		Tags:   [][2]string{{"TITLE", "New Title"}, {"ARTIST", "New Artist"}},
	}
	picture := &meta.Picture{
		Type: 3, // cover (front)
		MIME: "image/png",
		Desc: "new cover",
		Data: []byte("fake PNG data"),
	}
	var out bytes.Buffer
	err = flac.EditMetadata(&out, bytes.NewReader(src),
		flac.MetadataEdit{Type: meta.TypeVorbisComment, Body: comment},
		flac.MetadataEdit{Type: meta.TypePicture, Body: picture},
	)
	if err != nil {
		t.Fatal(err)
	}
	edited := out.Bytes()

	// Verify metadata block order; untouched blocks retain their original
	// order.
	want := []meta.Type{
		meta.TypeStreamInfo,
		meta.TypeSeekTable,
		meta.TypeVorbisComment,
		meta.TypePicture,
		meta.TypePadding,
	}
	if got := blockTypes(t, edited); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata block order mismatch; expected %v, got %v", want, got)
	}

	// Verify that the last-metadata-block flag is set on the final block only.
	_, spans, _ := metadataSpans(t, edited)
	for i, span := range spans {
		if want := i == len(spans)-1; span.isLast != want {
			t.Errorf("block %d (%v): IsLast = %v, expected %v", i, span.typ, span.isLast, want)
		}
	}

	// Verify the replaced blocks.
	stream, err := flac.Parse(bytes.NewReader(edited))
	if err != nil {
		t.Fatal(err)
	}
	gotComment := stream.Blocks[1].Body.(*meta.VorbisComment)
	if !reflect.DeepEqual(gotComment, comment) {
		t.Errorf("vorbis comment mismatch; expected %+v, got %+v", comment, gotComment)
	}
	gotPicture := stream.Blocks[2].Body.(*meta.Picture)
	if !reflect.DeepEqual(gotPicture, picture) {
		t.Errorf("picture mismatch; expected %+v, got %+v", picture, gotPicture)
	}

	// Verify that the audio frame bytes are untouched.
	if !bytes.Equal(frameBytes(t, src), frameBytes(t, edited)) {
		t.Fatal("audio frame bytes differ after metadata edit")
	}

	// Verify that the decoded audio is unchanged.
	srcSamples := decodeAll(t, src)
	editedSamples := decodeAll(t, edited)
	if !reflect.DeepEqual(srcSamples, editedSamples) {
		t.Fatal("decoded audio samples differ after metadata edit")
	}
}

func TestEditMetadataRemove(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}

	// Remove the VorbisComment and Picture blocks.
	var out bytes.Buffer
	err = flac.EditMetadata(&out, bytes.NewReader(src),
		flac.MetadataEdit{Type: meta.TypeVorbisComment},
		flac.MetadataEdit{Type: meta.TypePicture},
	)
	if err != nil {
		t.Fatal(err)
	}
	edited := out.Bytes()

	want := []meta.Type{meta.TypeStreamInfo, meta.TypeSeekTable, meta.TypePadding}
	if got := blockTypes(t, edited); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata block order mismatch; expected %v, got %v", want, got)
	}
	_, spans, _ := metadataSpans(t, edited)
	if !spans[len(spans)-1].isLast {
		t.Fatal("last-metadata-block flag not set on final block")
	}

	// Verify that the audio frame bytes are untouched and that the edited
	// stream still decodes.
	if !bytes.Equal(frameBytes(t, src), frameBytes(t, edited)) {
		t.Fatal("audio frame bytes differ after metadata edit")
	}
	if got, want := decodeAll(t, edited), decodeAll(t, src); !reflect.DeepEqual(got, want) {
		t.Fatal("decoded audio samples differ after metadata edit")
	}
}

func TestEditMetadataSeek(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the VorbisComment block, changing the size of the metadata
	// section; the frame-relative offsets of the SeekTable must remain valid.
	var out bytes.Buffer
	comment := &meta.VorbisComment{
		Vendor: "flac.EditMetadata",
		Tags:   [][2]string{{"TITLE", "A considerably longer title than the original one"}},
	}
	err = flac.EditMetadata(&out, bytes.NewReader(src),
		flac.MetadataEdit{Type: meta.TypeVorbisComment, Body: comment},
	)
	if err != nil {
		t.Fatal(err)
	}
	edited := out.Bytes()

	srcStream, err := flac.NewSeek(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	editedStream, err := flac.NewSeek(bytes.NewReader(edited))
	if err != nil {
		t.Fatal(err)
	}

	// Seek to random sample positions and verify that both streams land on
	// the same frame and decode the same samples.
	nsamples := srcStream.Info.NSamples
	// Note: positions are kept clear of the final frame, for which Stream.Seek
	// is known to fail (see https://github.com/mewkiz/flac/pull/73).
	for _, sampleNum := range []uint64{0, 1, nsamples / 3, nsamples / 2, nsamples * 3 / 4} {
		srcPos, err := srcStream.Seek(sampleNum)
		if err != nil {
			t.Fatal(err)
		}
		editedPos, err := editedStream.Seek(sampleNum)
		if err != nil {
			t.Fatalf("seek to sample %d of edited stream: %v", sampleNum, err)
		}
		if editedPos != srcPos {
			t.Fatalf("seek to sample %d: source landed on %d, edited stream on %d", sampleNum, srcPos, editedPos)
		}
		srcFrame, err := srcStream.ParseNext()
		if err != nil {
			t.Fatal(err)
		}
		editedFrame, err := editedStream.ParseNext()
		if err != nil {
			t.Fatalf("parse frame after seek to sample %d of edited stream: %v", sampleNum, err)
		}
		for i := range srcFrame.Subframes {
			if !reflect.DeepEqual(srcFrame.Subframes[i].Samples, editedFrame.Subframes[i].Samples) {
				t.Fatalf("decoded samples differ after seek to sample %d", sampleNum)
			}
		}
	}
}

func TestEditMetadataPreserveUnknownBlock(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}

	// Splice a reserved (unknown) metadata block with a non-empty body into
	// the source stream, right before the Padding block.
	_, spans, _ := metadataSpans(t, src)
	padding := spans[len(spans)-1]
	if padding.typ != meta.TypePadding {
		t.Fatalf("expected last block to be padding, got %v", padding.typ)
	}
	unknownBody := []byte("unknown body")
	unknown := append([]byte{7, 0, 0, byte(len(unknownBody))}, unknownBody...) // type 7, not last
	var spliced bytes.Buffer
	spliced.Write(src[:padding.start])
	spliced.Write(unknown)
	spliced.Write(src[padding.start:])

	// Replace the VorbisComment block.
	var out bytes.Buffer
	comment := &meta.VorbisComment{Vendor: "flac.EditMetadata"}
	err = flac.EditMetadata(&out, &spliced,
		flac.MetadataEdit{Type: meta.TypeVorbisComment, Body: comment},
	)
	if err != nil {
		t.Fatal(err)
	}
	edited := out.Bytes()

	// Verify that the unknown block is preserved verbatim and in order.
	want := []meta.Type{
		meta.TypeStreamInfo,
		meta.TypeSeekTable,
		meta.TypeVorbisComment,
		meta.TypePicture,
		meta.Type(7),
		meta.TypePadding,
	}
	if got := blockTypes(t, edited); !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata block order mismatch; expected %v, got %v", want, got)
	}
	_, editedSpans, _ := metadataSpans(t, edited)
	got := edited[editedSpans[4].start:editedSpans[4].end]
	if !bytes.Equal(got, unknown) {
		t.Fatalf("unknown block not preserved verbatim; expected %x, got %x", unknown, got)
	}
	if !bytes.Equal(frameBytes(t, src), frameBytes(t, edited)) {
		t.Fatal("audio frame bytes differ after metadata edit")
	}
}

// failAfterWriter fails with errFail after n bytes have been written.
type failAfterWriter struct {
	n int
}

var errFail = errors.New("write failed")

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errFail
	}
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, nil
	}
	w.n -= len(p)
	return len(p), nil
}

func TestEditMetadataErrors(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}
	_, spans, _ := metadataSpans(t, src)
	var pictureSpan, vorbisSpan blockSpan
	for _, span := range spans {
		switch span.typ {
		case meta.TypePicture:
			pictureSpan = span
		case meta.TypeVorbisComment:
			vorbisSpan = span
		}
	}

	t.Run("truncated block", func(t *testing.T) {
		// Truncate the source stream in the middle of the Picture block body.
		truncated := src[:pictureSpan.start+4+10]
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(truncated))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("truncated block header", func(t *testing.T) {
		// Truncate the source stream in the middle of a block header.
		truncated := src[:pictureSpan.start+2]
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(truncated))
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("duplicate vorbis comment", func(t *testing.T) {
		// Splice a duplicate VorbisComment block into the source stream.
		var dup bytes.Buffer
		dup.Write(src[:vorbisSpan.end])
		dup.Write(src[vorbisSpan.start:vorbisSpan.end])
		dup.Write(src[vorbisSpan.end:])
		var out bytes.Buffer
		err := flac.EditMetadata(&out, &dup)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("block too large", func(t *testing.T) {
		// The replacement block body exceeds the 24-bit length limit of the
		// metadata block header.
		picture := &meta.Picture{
			MIME: "image/png",
			Data: make([]byte, 1<<24),
		}
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(src),
			flac.MetadataEdit{Type: meta.TypePicture, Body: picture},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("edit stream info", func(t *testing.T) {
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(src),
			flac.MetadataEdit{Type: meta.TypeStreamInfo},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("duplicate edits", func(t *testing.T) {
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(src),
			flac.MetadataEdit{Type: meta.TypePicture},
			flac.MetadataEdit{Type: meta.TypePicture},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("type and body mismatch", func(t *testing.T) {
		var out bytes.Buffer
		err := flac.EditMetadata(&out, bytes.NewReader(src),
			flac.MetadataEdit{Type: meta.TypePicture, Body: &meta.VorbisComment{}},
		)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})

	t.Run("write failure", func(t *testing.T) {
		err := flac.EditMetadata(&failAfterWriter{n: 42}, bytes.NewReader(src))
		if !errors.Is(err, errFail) {
			t.Fatalf("expected write error, got %v", err)
		}
	})

	t.Run("read failure", func(t *testing.T) {
		err := flac.EditMetadata(io.Discard, &failAfterReader{})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
	})
}

// failAfterReader fails with errFail on read.
type failAfterReader struct{}

func (r *failAfterReader) Read(p []byte) (int, error) {
	return 0, errFail
}

func TestEditMetadataFile(t *testing.T) {
	src, err := os.ReadFile(silenceFixture)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.flac")
	if err := os.WriteFile(srcPath, src, 0o666); err != nil {
		t.Fatal(err)
	}

	// Replace the title of the archived FLAC file, writing to a new file.
	dstPath := filepath.Join(dir, "dst.flac")
	comment := &meta.VorbisComment{
		Vendor: "flac.EditMetadata",
		Tags:   [][2]string{{"TITLE", "New Title"}},
	}
	err = flac.EditMetadataFile(dstPath, srcPath,
		flac.MetadataEdit{Type: meta.TypeVorbisComment, Body: comment},
	)
	if err != nil {
		t.Fatal(err)
	}
	edited, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frameBytes(t, src), frameBytes(t, edited)) {
		t.Fatal("audio frame bytes differ after metadata edit")
	}
	stream, err := flac.Parse(bytes.NewReader(edited))
	if err != nil {
		t.Fatal(err)
	}
	got := stream.Blocks[1].Body.(*meta.VorbisComment)
	if !reflect.DeepEqual(got, comment) {
		t.Fatalf("vorbis comment mismatch; expected %+v, got %+v", comment, got)
	}

	// The destination file must not be overwritten.
	if err := flac.EditMetadataFile(dstPath, srcPath); err == nil {
		t.Fatal("expected error when destination file exists, got nil")
	}

	// A failed edit must not leave a partial output file behind.
	truncPath := filepath.Join(dir, "truncated.flac")
	if err := os.WriteFile(truncPath, src[:len(src)/2], 0o666); err != nil {
		t.Fatal(err)
	}
	badDst := filepath.Join(dir, "bad.flac")
	err = flac.EditMetadataFile(badDst, truncPath)
	if err == nil {
		t.Fatal("expected error for truncated source, got nil")
	}
	if _, statErr := os.Stat(badDst); !os.IsNotExist(statErr) {
		t.Fatalf("partial output file left behind after failed edit: %v", statErr)
	}
}
