package flac

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/icza/bitio"
	"github.com/mewkiz/flac/meta"
)

// maxBlockBodyLen is the maximum length in bytes of a metadata block body, as
// constrained by the 24-bit length field of the metadata block header.
//
// ref: https://www.xiph.org/flac/format.html#metadata_block_header
const maxBlockBodyLen = 1<<24 - 1

// A MetadataEdit specifies the replacement or removal of a metadata block
// type when copying a FLAC stream with EditMetadata.
//
// Type identifies the metadata block type to edit; supported types are
// Padding (removal only), Application, SeekTable, VorbisComment, CueSheet and
// Picture. Body holds the replacement block body (e.g. *meta.VorbisComment)
// and its type must correspond to Type; if Body is nil, blocks of the given
// type are removed instead.
//
// The first block of the given type in the source stream is replaced (or
// removed); any subsequent blocks of the same type are removed. If the source
// stream contains no block of the given type, the replacement block is
// appended after the last metadata block of the source stream.
//
// The StreamInfo block is mandatory and cannot be replaced or removed.
type MetadataEdit struct {
	// Metadata block type to replace or remove.
	Type meta.Type
	// Replacement block body (e.g. *meta.VorbisComment), or nil to remove
	// blocks of the given type.
	Body interface{}
}

// EditMetadata copies the FLAC stream of r to w, applying the given metadata
// block edits, without re-encoding the audio. The audio frames are copied
// verbatim, so their raw bytes are left untouched and the frame-relative
// offsets of an existing (or replacement) SeekTable block remain valid.
//
// Metadata blocks not covered by an edit — including blocks of reserved
// (unknown) type — are copied verbatim and retain their original relative
// order. The metadata block lengths and the last-metadata-block flag of the
// output stream are recomputed to reflect the applied edits.
//
// An error is returned if the source stream is truncated or otherwise
// malformed (e.g. contains a duplicate of a block type of which at most one
// may exist), if an edit is invalid, if a replacement block body exceeds the
// maximum metadata block length, or if a read or write fails. If an error is
// returned, the contents written to w so far are incomplete and must be
// discarded; use EditMetadataFile to have incomplete output files removed
// automatically.
func EditMetadata(w io.Writer, r io.Reader, edits ...MetadataEdit) error {
	if err := validateMetadataEdits(edits); err != nil {
		return err
	}
	br := bufio.NewReader(r)

	// out receives the FLAC signature (with any prepended ID3v2 data) and the
	// edited metadata blocks. It is buffered in its entirety so that any
	// error while reading or editing the metadata of the source stream is
	// reported before a single byte is written to w.
	var out bytes.Buffer

	// Copy the FLAC signature, preserving any prepended ID3v2 data verbatim.
	var sig [4]byte
	if _, err := io.ReadFull(br, sig[:]); err != nil {
		return fmt.Errorf("flac.EditMetadata: unable to read FLAC signature: %w", err)
	}
	if bytes.Equal(sig[:3], id3Signature) {
		// The ID3v2 header consists of the 3 byte signature, 2 version bytes,
		// 1 flags byte and a 4 byte synchsafe size; 4 bytes have already been
		// read.
		var rest [6]byte
		if _, err := io.ReadFull(br, rest[:]); err != nil {
			return fmt.Errorf("flac.EditMetadata: truncated ID3v2 header: %w", unexpectedEOF(err))
		}
		size := int64(rest[2])<<21 | int64(rest[3])<<14 | int64(rest[4])<<7 | int64(rest[5])
		out.Write(sig[:])
		out.Write(rest[:])
		if _, err := io.CopyN(&out, br, size); err != nil {
			return fmt.Errorf("flac.EditMetadata: truncated ID3v2 data: %w", unexpectedEOF(err))
		}
		if _, err := io.ReadFull(br, sig[:]); err != nil {
			return fmt.Errorf("flac.EditMetadata: unable to read FLAC signature: %w", unexpectedEOF(err))
		}
	}
	if !bytes.Equal(sig[:], flacSignature) {
		return fmt.Errorf("flac.EditMetadata: invalid FLAC signature; expected %q, got %q", flacSignature, sig)
	}
	out.Write(sig[:])

	// Read the metadata blocks of the source stream, keeping unedited blocks
	// verbatim and applying edits.
	blocks, err := readEditedBlocks(br, edits)
	if err != nil {
		return err
	}

	// Encode the edited metadata blocks, recomputing block lengths and the
	// last-metadata-block flag.
	for i := range blocks {
		last := i == len(blocks)-1
		if err := blocks[i].encode(&out, last); err != nil {
			return err
		}
	}

	// Write the signature and edited metadata blocks, and copy the audio
	// frames verbatim.
	if _, err := w.Write(out.Bytes()); err != nil {
		return fmt.Errorf("flac.EditMetadata: unable to write metadata: %w", err)
	}
	if _, err := io.Copy(w, br); err != nil {
		return fmt.Errorf("flac.EditMetadata: unable to copy audio frames: %w", err)
	}
	return nil
}

// EditMetadataFile copies the FLAC stream of the file src to the new file
// dst, applying the given metadata block edits, without re-encoding the
// audio. See EditMetadata for details.
//
// The file dst must not exist prior to the call; it is created exclusively
// so that an existing file — including src itself — is never overwritten. If
// an error occurs, the incomplete file dst is removed.
func EditMetadataFile(dst, src string, edits ...MetadataEdit) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	if err := EditMetadata(out, in, edits...); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// An editedBlock is a metadata block of the output stream of EditMetadata;
// its body is either copied verbatim from the source stream (raw) or encoded
// from a replacement body (body).
type editedBlock struct {
	typ  meta.Type
	raw  []byte      // verbatim block body copied from the source stream
	body interface{} // replacement block body
}

// encode encodes the metadata block to buf, writing the block header with the
// given last-metadata-block flag followed by the block body.
func (block *editedBlock) encode(buf *bytes.Buffer, last bool) error {
	if block.body == nil {
		// Verbatim copy; recompute only the header.
		bw := bitio.NewWriter(buf)
		hdr := &meta.Header{
			IsLast: last,
			Type:   block.typ,
			Length: int64(len(block.raw)),
		}
		if err := encodeBlockHeader(bw, hdr); err != nil {
			return err
		}
		if _, err := bw.Align(); err != nil {
			return err
		}
		_, err := buf.Write(block.raw)
		return err
	}
	// Encode the replacement block to a temporary buffer, so that an invalid
	// block body or a body exceeding the maximum metadata block length is
	// reported before any part of the block is written to the output stream.
	var tmp bytes.Buffer
	bw := bitio.NewWriter(&tmp)
	if err := encodeBlockBody(bw, block.typ, block.body, last); err != nil {
		return err
	}
	if _, err := bw.Align(); err != nil {
		return err
	}
	if bodyLen := tmp.Len() - 4; bodyLen > maxBlockBodyLen {
		return fmt.Errorf("flac.EditMetadata: %s block body of %d bytes exceeds the maximum metadata block length of %d bytes", block.typ, bodyLen, maxBlockBodyLen)
	}
	_, err := buf.Write(tmp.Bytes())
	return err
}

// readEditedBlocks reads the metadata blocks of the FLAC stream of r —
// positioned at the first metadata block header — and returns the metadata
// blocks of the edited output stream.
func readEditedBlocks(r io.Reader, edits []MetadataEdit) ([]*editedBlock, error) {
	// The first metadata block must be a StreamInfo block; it is always
	// copied verbatim.
	block, err := meta.New(r)
	if err != nil {
		return nil, fmt.Errorf("flac.EditMetadata: unable to read StreamInfo block header: %w", unexpectedEOF(err))
	}
	if block.Type != meta.TypeStreamInfo {
		return nil, fmt.Errorf("flac.EditMetadata: incorrect type of first metadata block; expected %v, got %v", meta.TypeStreamInfo, block.Type)
	}
	body, err := readBlockBody(r, block)
	if err != nil {
		return nil, err
	}
	blocks := []*editedBlock{{typ: block.Type, raw: body}}

	// seen records the block types of which at most one block may exist in a
	// FLAC stream.
	//
	// ref: https://www.xiph.org/flac/format.html#format_overview
	seen := map[meta.Type]bool{meta.TypeStreamInfo: true}
	applied := make([]bool, len(edits))
	for !block.IsLast {
		block, err = meta.New(r)
		if err != nil {
			return nil, fmt.Errorf("flac.EditMetadata: unable to read metadata block header: %w", unexpectedEOF(err))
		}
		if block.Type == 127 {
			return nil, fmt.Errorf("flac.EditMetadata: %w", meta.ErrInvalidType)
		}
		if singletonBlockType(block.Type) {
			if seen[block.Type] {
				return nil, fmt.Errorf("flac.EditMetadata: duplicate %s block", block.Type)
			}
			seen[block.Type] = true
		}
		if i := editIndex(edits, block.Type); i >= 0 {
			// The first block of an edited type is replaced (or removed);
			// any subsequent blocks of the same type are removed.
			if !applied[i] {
				applied[i] = true
				if edits[i].Body != nil {
					blocks = append(blocks, &editedBlock{typ: edits[i].Type, body: edits[i].Body})
				}
			}
			if err := skipBlockBody(r, block); err != nil {
				return nil, err
			}
			continue
		}
		// Copy unedited blocks — including blocks of reserved (unknown) type
		// — verbatim, retaining their original order.
		body, err := readBlockBody(r, block)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, &editedBlock{typ: block.Type, raw: body})
	}

	// Append replacement blocks of types not present in the source stream.
	for i := range edits {
		if !applied[i] && edits[i].Body != nil {
			blocks = append(blocks, &editedBlock{typ: edits[i].Type, body: edits[i].Body})
		}
	}
	return blocks, nil
}

// readBlockBody reads the body of the given metadata block.
func readBlockBody(r io.Reader, block *meta.Block) ([]byte, error) {
	body := make([]byte, block.Length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("flac.EditMetadata: truncated %s block body: %w", block.Type, unexpectedEOF(err))
	}
	return body, nil
}

// skipBlockBody discards the body of the given metadata block.
func skipBlockBody(r io.Reader, block *meta.Block) error {
	if _, err := io.CopyN(io.Discard, r, block.Length); err != nil {
		return fmt.Errorf("flac.EditMetadata: truncated %s block body: %w", block.Type, unexpectedEOF(err))
	}
	return nil
}

// validateMetadataEdits validates the given metadata block edits.
func validateMetadataEdits(edits []MetadataEdit) error {
	seen := make(map[meta.Type]bool)
	for _, edit := range edits {
		switch edit.Type {
		case meta.TypeStreamInfo:
			return fmt.Errorf("flac.EditMetadata: the StreamInfo block is mandatory and cannot be replaced or removed")
		case meta.TypePadding:
			if edit.Body != nil {
				return fmt.Errorf("flac.EditMetadata: replacement of Padding blocks is not supported")
			}
		case meta.TypeApplication, meta.TypeSeekTable, meta.TypeVorbisComment, meta.TypeCueSheet, meta.TypePicture:
			// Supported block types.
		default:
			return fmt.Errorf("flac.EditMetadata: unsupported metadata block type %d", edit.Type)
		}
		if seen[edit.Type] {
			return fmt.Errorf("flac.EditMetadata: duplicate edit of %s block", edit.Type)
		}
		seen[edit.Type] = true
		if edit.Body != nil && blockBodyType(edit.Body) != edit.Type {
			return fmt.Errorf("flac.EditMetadata: mismatch between edit type %s and replacement block body of type %T", edit.Type, edit.Body)
		}
	}
	return nil
}

// editIndex returns the index of the edit applying to the given metadata
// block type, or -1 if no such edit exists.
func editIndex(edits []MetadataEdit, typ meta.Type) int {
	for i := range edits {
		if edits[i].Type == typ {
			return i
		}
	}
	return -1
}

// singletonBlockType reports whether at most one block of the given metadata
// block type may exist in a FLAC stream.
//
// ref: https://www.xiph.org/flac/format.html#format_overview
func singletonBlockType(typ meta.Type) bool {
	switch typ {
	case meta.TypeStreamInfo, meta.TypeSeekTable, meta.TypeVorbisComment, meta.TypeCueSheet:
		return true
	}
	return false
}

// blockBodyType returns the metadata block type corresponding to the given
// block body, or -1 if the body type is not a supported metadata block body.
func blockBodyType(body interface{}) meta.Type {
	switch body.(type) {
	case *meta.Application:
		return meta.TypeApplication
	case *meta.SeekTable:
		return meta.TypeSeekTable
	case *meta.VorbisComment:
		return meta.TypeVorbisComment
	case *meta.CueSheet:
		return meta.TypeCueSheet
	case *meta.Picture:
		return meta.TypePicture
	}
	return meta.Type(127)
}

// encodeBlockBody encodes a metadata block of the given type with the given
// block body, writing to bw. In contrast to encodeBlock, the block type is
// derived from the block body and an error is returned for unsupported block
// body types instead of panicking.
func encodeBlockBody(bw *bitio.Writer, typ meta.Type, body interface{}, last bool) error {
	if blockBodyType(body) != typ {
		return fmt.Errorf("flac.EditMetadata: mismatch between block type %s and block body of type %T", typ, body)
	}
	switch body := body.(type) {
	case *meta.Application:
		return encodeApplication(bw, body, last)
	case *meta.SeekTable:
		return encodeSeekTable(bw, body, last)
	case *meta.VorbisComment:
		return encodeVorbisComment(bw, body, last)
	case *meta.CueSheet:
		return encodeCueSheet(bw, body, last)
	case *meta.Picture:
		return encodePicture(bw, body, last)
	}
	return fmt.Errorf("flac.EditMetadata: support for metadata block body type %T not yet implemented", body)
}

// unexpectedEOF returns io.ErrUnexpectedEOF if err is io.EOF, and returns err
// otherwise.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
