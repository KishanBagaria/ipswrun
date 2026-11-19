package rangefs

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
)

// MemberOpener exposes one member of the restore image's ZIP as random-access bytes plus their
// length. remote reads the ZIP file itself. Image.OpenMember holds the one in use; replace it to
// read members OpenStoredMember refuses.
type MemberOpener func(remote io.ReaderAt, member *zip.File) (io.ReaderAt, int64, error)

// ErrCompressedMember is what OpenStoredMember refuses a deflate-compressed member with.
var ErrCompressedMember = errors.New("cannot be read by range")

// OpenStoredMember is the default MemberOpener. A stored member is a plain window into the ZIP; a
// deflate-compressed one has no random access and is refused rather than downloaded.
func OpenStoredMember(remote io.ReaderAt, member *zip.File) (io.ReaderAt, int64, error) {
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
