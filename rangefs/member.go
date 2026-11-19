package rangefs

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
)

// ErrCompressedMember is what a deflate-compressed member is refused with unless streaming is allowed.
var ErrCompressedMember = errors.New("cannot be read by range")

// openMember exposes one member of the restore image's ZIP as random-access bytes plus their length.
func (m *Image) openMember(member *zip.File) (io.ReaderAt, int64, error) {
	if m.streaming && member.Method == zip.Deflate {
		file, err := m.remote.inflated(member)
		if err != nil {
			return nil, 0, fmt.Errorf("stream %s: %w", member.Name, err)
		}
		return file, int64(member.UncompressedSize64), nil
	}
	return openStoredMember(m.remote, member)
}

// openStoredMember is a plain window into the ZIP. A deflate-compressed member has no random
// access and is refused rather than downloaded.
func openStoredMember(remote io.ReaderAt, member *zip.File) (io.ReaderAt, int64, error) {
	if member.Method != zip.Store || member.CompressedSize64 != member.UncompressedSize64 {
		return nil, 0, fmt.Errorf("%s is a compressed ZIP member (%d bytes) and %w", member.Name, member.CompressedSize64, ErrCompressedMember)
	}
	offset, err := member.DataOffset()
	if err != nil {
		return nil, 0, err
	}
	size := int64(member.UncompressedSize64)
	return io.NewSectionReader(remote, offset, size), size, nil
}
