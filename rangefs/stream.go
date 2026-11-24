package rangefs

import (
	"archive/zip"
	"compress/flate"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// AllowStreaming lets the image read deflate-compressed disk image members. A deflate stream has
// one exact entry point, its start, so such a member is downloaded front to back as one range
// (requested again from the byte reached if the connection breaks) and inflated to a file: kept
// in the cache directory when there is one (later opens reuse it), otherwise unlinked as soon as
// it is created.
func (m *Image) AllowStreaming() { m.streaming = true }

func (m *Image) inflate(member *zip.File) (*os.File, error) {
	if m.remote != nil {
		return m.remote.inflated(member)
	}
	dir := ""
	if m.cacheDir != "" {
		dir = filepath.Join(m.cacheDir, "local")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return inflateStream(dir, member, func() (io.ReadCloser, error) { return member.Open() })
}

func (r *rangeReader) inflated(member *zip.File) (*os.File, error) {
	return inflateStream(r.dir, member, func() (io.ReadCloser, error) {
		offset, err := member.DataOffset()
		if err != nil {
			return nil, err
		}
		end := offset + int64(member.CompressedSize64)
		client := *r.client
		client.Timeout = 0 // gigabytes; the transfer is bounded by its length instead
		body := r.open(&client, offset, end)
		return &inflateBody{ReadCloser: flate.NewReader(body), underlying: body}, nil
	})
}

type inflateBody struct {
	io.ReadCloser
	underlying io.Closer
}

func (b *inflateBody) Close() error {
	return errors.Join(b.ReadCloser.Close(), b.underlying.Close())
}

func inflateStream(dir string, member *zip.File, open func() (io.ReadCloser, error)) (*os.File, error) {
	size := int64(member.UncompressedSize64)
	kept := ""
	if dir != "" {
		kept = filepath.Join(dir, fmt.Sprintf("member-%08x-%s", member.CRC32, strings.ReplaceAll(member.Name, "/", "_")))
		if f, err := os.Open(kept); err == nil {
			if st, err := f.Stat(); err == nil && st.Size() == size {
				return f, nil // verified before it was given this name
			}
			f.Close()
		}
	}
	src, err := open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	file, err := os.CreateTemp(dir, "member-*.partial")
	if err != nil {
		return nil, err
	}
	discard := func(err error) (*os.File, error) {
		file.Close()
		os.Remove(file.Name())
		return nil, err
	}
	sum := crc32.NewIEEE()
	written, err := io.Copy(io.MultiWriter(file, sum), src)
	if err != nil {
		return discard(err)
	}
	if written != size || sum.Sum32() != member.CRC32 {
		return discard(errors.New("inflated member does not match its declared size and CRC-32"))
	}
	if kept == "" {
		os.Remove(file.Name()) // lives only as long as it stays open
		return file, nil
	}
	if err := os.Rename(file.Name(), kept); err != nil {
		return discard(err)
	}
	return file, nil
}
