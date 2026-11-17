package flac

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/icza/bitio"
	"github.com/mewkiz/flac/meta"
)

// MetadataUpdate describes a restricted in-place metadata update of a FLAC
// stream. Each non-nil field replaces the corresponding metadata block of the
// stream; a field left nil keeps the corresponding block (if any) untouched.
//
// The update is performed in place: the new blocks must fit within the
// combined capacity of the metadata blocks they replace and the padding blocks
// of the stream. Any remaining capacity is re-emitted as a padding block, so
// the offset of the first audio frame never changes and no audio data is
// touched.
type MetadataUpdate struct {
	// VorbisComment replaces the VorbisComment metadata block of the stream
	// (e.g. to edit the title tag); nil to leave it unchanged.
	VorbisComment *meta.VorbisComment
	// Picture replaces the Picture metadata block of the stream (e.g. to
	// update the cover art); nil to leave it unchanged.
	Picture *meta.Picture
}

// Errors returned by UpdateMetadata.
var (
	// ErrNoPadding reports that the stream contains no padding block to
	// absorb the growth of the updated metadata blocks.
	ErrNoPadding = errors.New("flac.UpdateMetadata: no padding block available to absorb metadata growth")
	// ErrInsufficientCapacity reports that the updated metadata blocks do not
	// fit within the combined capacity of the blocks they replace and the
	// padding of the stream.
	ErrInsufficientCapacity = errors.New("flac.UpdateMetadata: insufficient metadata and padding capacity for in-place update")
	// ErrDuplicateBlock reports that the stream contains more than one
	// metadata block of a type subject to update, making an in-place update
	// ambiguous.
	ErrDuplicateBlock = errors.New("flac.UpdateMetadata: multiple conflicting metadata blocks of the same type")
)

const (
	// blockHeaderSize is the size in bytes of a metadata block header.
	blockHeaderSize = 4
	// maxBlockBodyLength is the maximum length in bytes of a metadata block
	// body, as constrained by the 24-bit length field of the block header.
	maxBlockBodyLength = 1<<24 - 1
)

// blockLayout records the on-disk layout of a metadata block.
type blockLayout struct {
	// Metadata block body type.
	typ meta.Type
	// Offset of the block header relative to the start of the metadata
	// region.
	off int64
	// Length of the block body in bytes.
	length int64
}

// total returns the total size in bytes of the block, header included.
func (b blockLayout) total() int64 {
	return blockHeaderSize + b.length
}

// UpdateMetadata applies the given restricted metadata update to the FLAC
// stream of rws, modifying only the metadata region of the stream in place.
//
// The VorbisComment and Picture blocks specified by update are encoded within
// the combined capacity of the blocks they replace and the padding blocks of
// the stream; leftover capacity is re-emitted as a padding block. All other
// metadata blocks (including unknown reserved blocks) are preserved verbatim
// and in order, block lengths and last-block flags are recomputed as needed,
// and the offset of the first audio frame is left unchanged.
//
// If the updated blocks do not fit, an error wrapping ErrNoPadding (when the
// stream has no padding to consume) or ErrInsufficientCapacity is returned
// and the stream is left unmodified. If the stream contains more than one
// VorbisComment or Picture block subject to update, an error wrapping
// ErrDuplicateBlock is returned. If the underlying write fails midway,
// UpdateMetadata attempts to restore the original metadata region before
// returning the write error.
func UpdateMetadata(rws io.ReadWriteSeeker, update *MetadataUpdate) error {
	if update == nil || (update.VorbisComment == nil && update.Picture == nil) {
		return nil
	}
	if rws == nil {
		return errors.New("flac.UpdateMetadata: nil io.ReadWriteSeeker")
	}
	// Verify that the stream is seekable before touching it.
	if _, err := rws.Seek(0, io.SeekCurrent); err != nil {
		return fmt.Errorf("flac.UpdateMetadata: input stream is not seekable: %w", err)
	}

	// Locate the start of the metadata region (after the FLAC signature,
	// skipping any prepended ID3v2 data).
	metaStart, err := seekToFlacSignature(rws)
	if err != nil {
		return err
	}

	// Walk the metadata block headers and record the layout of each block.
	var blocks []blockLayout
	off := int64(0)
	for {
		block, err := meta.New(rws)
		if err != nil {
			return fmt.Errorf("flac.UpdateMetadata: unable to parse metadata block header: %w", err)
		}
		blocks = append(blocks, blockLayout{
			typ:    block.Header.Type,
			off:    off,
			length: block.Header.Length,
		})
		if err := block.Skip(); err != nil {
			return fmt.Errorf("flac.UpdateMetadata: unable to skip metadata block body: %w", err)
		}
		off += blockHeaderSize + block.Header.Length
		if block.Header.IsLast {
			break
		}
	}
	if len(blocks) == 0 || blocks[0].typ != meta.TypeStreamInfo {
		return errors.New("flac.UpdateMetadata: first metadata block is not StreamInfo")
	}
	regionLen := off // total size of the metadata region; audio data starts here.

	// Read the original metadata region; it is used both to preserve
	// unmodified blocks verbatim and to roll back on write failure.
	orig := make([]byte, regionLen)
	if _, err := rws.Seek(metaStart, io.SeekStart); err != nil {
		return fmt.Errorf("flac.UpdateMetadata: unable to seek to metadata region: %w", err)
	}
	if _, err := io.ReadFull(rws, orig); err != nil {
		return fmt.Errorf("flac.UpdateMetadata: unable to read metadata region: %w", err)
	}

	// Count block types and determine which blocks the update consumes.
	var nVorbis, nPicture, nPadding int
	consumed := make([]bool, len(blocks))
	for i, b := range blocks {
		switch b.typ {
		case meta.TypePadding:
			nPadding++
			consumed[i] = true
		case meta.TypeVorbisComment:
			nVorbis++
			if update.VorbisComment != nil {
				consumed[i] = true
			}
		case meta.TypePicture:
			nPicture++
			if update.Picture != nil {
				consumed[i] = true
			}
		}
	}
	if update.VorbisComment != nil && nVorbis > 1 {
		return fmt.Errorf("%w: %d vorbis comment blocks", ErrDuplicateBlock, nVorbis)
	}
	if update.Picture != nil && nPicture > 1 {
		return fmt.Errorf("%w: %d picture blocks", ErrDuplicateBlock, nPicture)
	}

	// The capacity available to the updated blocks is the combined size of
	// the blocks they replace and the padding of the stream.
	var capacity int64
	for i, b := range blocks {
		if consumed[i] {
			capacity += b.total()
		}
	}

	// Encode the updated blocks (header included, last-block flag cleared;
	// the flag is fixed up during assembly below).
	encode := func(encodeBody func(bw *bitio.Writer, last bool) error) ([]byte, error) {
		var buf bytes.Buffer
		bw := bitio.NewWriter(&buf)
		if err := encodeBody(bw, false); err != nil {
			return nil, err
		}
		if err := bw.Close(); err != nil {
			return nil, err
		}
		if buf.Len()-blockHeaderSize > maxBlockBodyLength {
			return nil, fmt.Errorf("flac.UpdateMetadata: encoded metadata block body (%d bytes) exceeds the 24-bit block length limit", buf.Len()-blockHeaderSize)
		}
		return buf.Bytes(), nil
	}
	var vorbisBytes, pictureBytes []byte
	if update.VorbisComment != nil {
		comment := update.VorbisComment
		vorbisBytes, err = encode(func(bw *bitio.Writer, last bool) error {
			return encodeVorbisComment(bw, comment, last)
		})
		if err != nil {
			return err
		}
	}
	if update.Picture != nil {
		pic := update.Picture
		pictureBytes, err = encode(func(bw *bitio.Writer, last bool) error {
			return encodePicture(bw, pic, last)
		})
		if err != nil {
			return err
		}
	}

	need := int64(len(vorbisBytes) + len(pictureBytes))
	if need > capacity {
		if nPadding == 0 {
			return fmt.Errorf("%w: need %d bytes, have %d", ErrNoPadding, need, capacity)
		}
		return fmt.Errorf("%w: need %d bytes, have %d", ErrInsufficientCapacity, need, capacity)
	}
	leftover := capacity - need
	if leftover > 0 && leftover < blockHeaderSize {
		return fmt.Errorf("%w: %d leftover bytes cannot hold a padding block header", ErrInsufficientCapacity, leftover)
	}
	var paddingBytes []byte
	if leftover > 0 {
		paddingBytes, err = encode(func(bw *bitio.Writer, last bool) error {
			return encodePadding(bw, leftover-blockHeaderSize, last)
		})
		if err != nil {
			return err
		}
	}

	// Assemble the new metadata region: unmodified blocks are copied
	// verbatim and in order; the updated blocks and the padding absorbing
	// the leftover capacity take the place of the first consumed block.
	var segs [][]byte
	inserted := false
	for i, b := range blocks {
		if consumed[i] {
			if !inserted {
				inserted = true
				if vorbisBytes != nil {
					segs = append(segs, vorbisBytes)
				}
				if pictureBytes != nil {
					segs = append(segs, pictureBytes)
				}
				if paddingBytes != nil {
					segs = append(segs, paddingBytes)
				}
			}
			continue
		}
		seg := make([]byte, b.total())
		copy(seg, orig[b.off:b.off+b.total()])
		segs = append(segs, seg)
	}

	// Recompute the last-block flag of every block: set on the final block
	// of the region, clear on all others.
	for i, seg := range segs {
		if i == len(segs)-1 {
			seg[0] |= 0x80
		} else {
			seg[0] &^= 0x80
		}
	}
	var region bytes.Buffer
	for _, seg := range segs {
		region.Write(seg)
	}
	if int64(region.Len()) != regionLen {
		return fmt.Errorf("flac.UpdateMetadata: internal error: new metadata region (%d bytes) does not match original region (%d bytes)", region.Len(), regionLen)
	}

	// Overwrite the metadata region with a single write. On failure, attempt
	// to restore the original metadata region.
	if _, err := rws.Seek(metaStart, io.SeekStart); err != nil {
		return fmt.Errorf("flac.UpdateMetadata: unable to seek to metadata region: %w", err)
	}
	if n, err := rws.Write(region.Bytes()); err != nil || n != region.Len() {
		if err == nil {
			err = io.ErrShortWrite
		}
		if _, rbErr := rws.Seek(metaStart, io.SeekStart); rbErr == nil {
			if _, rbErr = rws.Write(orig); rbErr == nil {
				return fmt.Errorf("flac.UpdateMetadata: write failed (%w); original metadata region restored", err)
			}
			return fmt.Errorf("flac.UpdateMetadata: write failed (%w) and rollback failed (%v); stream may be corrupt", err, rbErr)
		} else {
			return fmt.Errorf("flac.UpdateMetadata: write failed (%w) and rollback seek failed (%v); stream may be corrupt", err, rbErr)
		}
	}
	return nil
}

// seekToFlacSignature seeks rws to the beginning of the stream, verifies the
// FLAC signature (skipping any prepended ID3v2 data), and returns the offset
// of the first metadata block.
func seekToFlacSignature(rws io.ReadWriteSeeker) (int64, error) {
	if _, err := rws.Seek(0, io.SeekStart); err != nil {
		return 0, fmt.Errorf("flac.UpdateMetadata: unable to seek to start of stream: %w", err)
	}
	var buf [4]byte
	if _, err := io.ReadFull(rws, buf[:]); err != nil {
		return 0, fmt.Errorf("flac.UpdateMetadata: unable to read FLAC signature: %w", err)
	}
	// Skip prepended ID3v2 data.
	if bytes.Equal(buf[:3], id3Signature) {
		// The ID3v2 header is 10 bytes: "ID3", 2 version bytes, 1 flags byte
		// and a 4-byte synchsafe size; the first 4 bytes are already read.
		var hdr [6]byte
		if _, err := io.ReadFull(rws, hdr[:]); err != nil {
			return 0, fmt.Errorf("flac.UpdateMetadata: unable to read ID3v2 header: %w", err)
		}
		size := int64(hdr[2]&0x7f)<<21 | int64(hdr[3]&0x7f)<<14 | int64(hdr[4]&0x7f)<<7 | int64(hdr[5]&0x7f)
		if _, err := rws.Seek(size, io.SeekCurrent); err != nil {
			return 0, fmt.Errorf("flac.UpdateMetadata: unable to skip ID3v2 data: %w", err)
		}
		if _, err := io.ReadFull(rws, buf[:]); err != nil {
			return 0, fmt.Errorf("flac.UpdateMetadata: unable to read FLAC signature: %w", err)
		}
	}
	if !bytes.Equal(buf[:], flacSignature) {
		return 0, fmt.Errorf("flac.UpdateMetadata: invalid FLAC signature; expected %q, got %q", flacSignature, buf)
	}
	return rws.Seek(0, io.SeekCurrent)
}
