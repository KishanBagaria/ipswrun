package rangefs

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	apfs "github.com/blacktop/go-apfs"
	"github.com/blacktop/go-apfs/pkg/disk/dmg"
	"github.com/blacktop/go-apfs/pkg/disk/gpt"
	"github.com/blacktop/go-apfs/types"
	"github.com/blacktop/ipsw/pkg/aea"
	"github.com/blacktop/ipsw/pkg/info"
)

// Volumes of a restore image.
const (
	FileSystem = "fs"  // the system volume
	SystemOS   = "sys" // the SystemOS cryptex
)

// Image is a remote restore image whose volumes are opened on first use.
type Image struct {
	// OpenMember turns a ZIP member into random-access bytes; OpenStoredMember unless replaced.
	OpenMember MemberOpener

	remote  *rangeReader
	archive *zip.Reader
	info    *info.Info
	device  string
	volumes map[string]*apfs.Reader
	names   map[string]string
}

// Open reads the ZIP directory and build manifest of the restore image at url. device is the
// BuildManifest product type or device class used to choose disk images; cacheDir, when not
// empty, keeps what was downloaded between runs.
func Open(url, cacheDir, device string) (*Image, error) {
	remote, err := newRangeReader(url, cacheDir)
	if err != nil {
		return nil, err
	}
	archive, err := zip.NewReader(remote, remote.size)
	if err != nil {
		return nil, err
	}
	parsed, err := info.ParseZipFiles(archive.File)
	if err != nil {
		return nil, err
	}
	if parsed.Plists == nil || parsed.Plists.BuildManifest == nil {
		return nil, errors.New("restore image has no BuildManifest.plist")
	}
	return &Image{OpenMember: OpenStoredMember, remote: remote, archive: archive, info: parsed, device: device,
		volumes: map[string]*apfs.Reader{}, names: map[string]string{}}, nil
}

// Version is the product version in the build manifest, e.g. "15.6.1".
func (m *Image) Version() string { return m.info.Plists.BuildManifest.ProductVersion }

// Build is the product build version in the build manifest, e.g. "24G90".
func (m *Image) Build() string { return m.info.Plists.BuildManifest.ProductBuildVersion }

// Size is the length of the whole restore image in bytes.
func (m *Image) Size() int64 { return m.remote.size }

// Stats reports the range requests made so far and the bytes they returned.
func (m *Image) Stats() (requests, bytes int64) {
	return m.remote.requests.Load(), m.remote.bytes.Load()
}

// image opens a disk image member as an APFS container, through AEA, UDIF and GPT as needed.
func (m *Image) image(name string) (*apfs.Reader, error) {
	var member *zip.File
	for _, f := range m.archive.File {
		if f.Name == name {
			member = f
		}
	}
	if member == nil {
		return nil, fmt.Errorf("no ZIP member %s", name)
	}
	source, size, err := m.OpenMember(m.remote, member)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	if _, err := source.ReadAt(magic, 0); err != nil {
		return nil, err
	}
	if string(magic) == aea.Magic {
		metadata, err := aea.ReadMetadata(source, size)
		if err != nil {
			return nil, err
		}
		key, err := metadata.SymmetricKey(nil, "", "", false)
		if err != nil {
			return nil, fmt.Errorf("resolve AEA key: %w", err)
		}
		reader, err := aea.NewReader(source, size, key)
		if err != nil {
			return nil, err
		}
		source, size = reader, reader.Size()
	}
	if size >= 512 {
		footer := make([]byte, 4)
		if _, err := source.ReadAt(footer, size-512); err != nil {
			return nil, err
		}
		if string(footer) == "koly" {
			image, err := dmg.NewDMG(io.NewSectionReader(source, 0, size))
			if err != nil {
				return nil, err
			}
			if err := image.Load(); err != nil {
				return nil, err
			}
			source, size = image, int64(image.GetSize())
		}
	}
	if source, size, err = apfsPartition(source, size); err != nil {
		return nil, err
	}
	return apfs.OpenReader(source, size)
}

// apfsPartition narrows a GPT image to its APFS partition; a bare container is returned unchanged.
func apfsPartition(source io.ReaderAt, size int64) (io.ReaderAt, int64, error) {
	var header gpt.Header
	if err := binary.Read(io.NewSectionReader(source, gpt.SectorSize, gpt.SectorSize), binary.LittleEndian, &header); err != nil {
		return nil, 0, err
	}
	if string(header.Signature[:]) != gpt.Signature {
		return source, size, nil
	}
	sectors := uint64(size / gpt.SectorSize)
	if header.EntriesStart > sectors || header.EntriesCount > 4096 || header.EntriesSize < 128 || header.EntriesSize > 4096 ||
		uint64(header.EntriesCount)*uint64(header.EntriesSize) > uint64(size)-header.EntriesStart*gpt.SectorSize {
		return nil, 0, errors.New("unsupported GPT table")
	}
	table := make([]byte, header.EntriesCount*header.EntriesSize)
	if _, err := source.ReadAt(table, int64(header.EntriesStart*gpt.SectorSize)); err != nil {
		return nil, 0, err
	}
	for ; len(table) > 0; table = table[header.EntriesSize:] {
		var partition gpt.Partition
		if err := binary.Read(bytes.NewReader(table), binary.LittleEndian, &partition); err != nil {
			return nil, 0, err
		}
		if partition.Type.String() != gpt.Apple_APFS {
			continue
		}
		if partition.StartingLBA > partition.EndingLBA || partition.EndingLBA >= sectors {
			return nil, 0, errors.New("invalid APFS GPT partition")
		}
		size = int64((partition.EndingLBA - partition.StartingLBA + 1) * gpt.SectorSize)
		return io.NewSectionReader(source, int64(partition.StartingLBA*gpt.SectorSize), size), size, nil
	}
	return nil, 0, errors.New("no APFS partition in GPT")
}

func (m *Image) volume(kind string) (*apfs.Reader, error) {
	if v, ok := m.volumes[kind]; ok {
		return v, nil
	}
	selected, err := m.info.ForDevice(m.device)
	if err != nil {
		return nil, err
	}
	var name string
	switch kind {
	case FileSystem:
		name, err = selected.GetFileSystemOsDmg()
	case SystemOS:
		name, err = selected.GetSystemOsDmg()
	default:
		err = fmt.Errorf("unknown volume %q", kind)
	}
	if err != nil {
		return nil, err
	}
	v, err := m.image(name)
	if err != nil {
		return nil, fmt.Errorf("%s volume %s: %w", kind, name, err)
	}
	m.volumes[kind], m.names[kind] = v, name
	return v, nil
}

// Kind is what a path in a volume names.
type Kind int

const (
	Missing   Kind = iota // nothing, or something that is not a file, directory or symlink
	Regular               // a regular file
	Directory             // a directory
	Symlink               // a symbolic link, not followed
)

// Entry describes a path in a volume. File is set for regular files only.
type Entry struct {
	Kind   Kind
	Target string
	File   *File
}

// File is a regular file inside a volume.
type File struct {
	file *apfs.File
	name string
}

// Stat looks a path up without following a final symlink. An error means the volume could not be
// opened or read; a path that does not exist is Missing.
func (m *Image) Stat(vol, path string) (Entry, error) {
	fail := func(err error) (Entry, error) { return Entry{}, fmt.Errorf("%s:%s: %w", vol, path, err) }
	v, err := m.volume(vol)
	if err != nil {
		return fail(err)
	}
	entry, err := v.Lookup(strings.TrimPrefix(path, "/"))
	if err != nil {
		return Entry{Kind: Missing}, nil
	}
	switch entry.Type {
	case uint16(types.DT_DIR):
		return Entry{Kind: Directory}, nil
	case uint16(types.DT_LNK):
		target, err := v.Readlink(entry.ID)
		if err != nil {
			return fail(err)
		}
		return Entry{Kind: Symlink, Target: target}, nil
	case uint16(types.DT_REG):
		f, err := v.OpenFileID(entry.ID)
		if err != nil {
			return fail(err)
		}
		return Entry{Kind: Regular, File: &File{file: f, name: vol + ":" + path}}, nil
	}
	return Entry{Kind: Missing}, nil
}

// Open returns the regular file at path, or nil when there is none.
func (m *Image) Open(vol, path string) (*File, error) {
	entry, err := m.Stat(vol, path)
	return entry.File, err
}

// Size is the file's length in bytes.
func (f *File) Size() int64 { return f.file.Size }

// ReadAt implements io.ReaderAt.
func (f *File) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("%s: read out of bounds", f.name)
	}
	if off >= f.file.Size {
		return 0, io.EOF
	}
	short := int64(len(p)) > f.file.Size-off
	if short {
		p = p[:f.file.Size-off]
	}
	if n, err := f.file.ReadAt(p, off); err != nil && !errors.Is(err, io.EOF) {
		return n, fmt.Errorf("%s: %w", f.name, err)
	}
	if short {
		return len(p), io.EOF
	}
	return len(p), nil
}

// Pread returns up to n bytes at off; fewer only at the end of the file.
func (f *File) Pread(off, n int64) ([]byte, error) {
	n = max(0, min(n, f.file.Size-off))
	out := make([]byte, n)
	if n == 0 {
		return out, nil
	}
	if _, err := f.ReadAt(out, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return out, nil
}

// ReadAll returns the whole file.
func (f *File) ReadAll() ([]byte, error) { return f.Pread(0, f.file.Size) }
