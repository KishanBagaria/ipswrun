package rangefs

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	apfs "github.com/blacktop/go-apfs"
	"github.com/blacktop/go-apfs/pkg/disk/dmg"
	"github.com/blacktop/go-apfs/pkg/disk/gpt"
	"github.com/blacktop/go-apfs/types"
)

// Volumes of a restore image.
const (
	FileSystem = "fs"  // the system volume
	SystemOS   = "sys" // the SystemOS cryptex
)

// ErrNoSystemOS is in the error of a SystemOS read on a restore image that has no SystemOS cryptex:
// every release before macOS 13 and iOS 16, which keep everything in the system volume.
var ErrNoSystemOS = errors.New("cryptex not found")

// Image is a remote or local restore image whose volumes are opened on first use.
type Image struct {
	remote     *rangeReader // nil for a local file
	source     io.ReaderAt
	file       *os.File // local restore image, closed by Close
	cacheDir   string
	archive    *zip.Reader
	manifest   *buildManifest
	device     string
	identities []buildIdentity // cached device-filtered identities
	streaming  bool            // see AllowStreaming
	volumes    map[string]*apfs.Reader
	opened     []*os.File // inflated members, closed by Close
}

// Open reads the ZIP directory and build manifest of the restore image at location, which may be
// an HTTP(S) URL or a local file path (optionally prefixed with file://). device is the
// BuildManifest product type or device class used to choose disk images; cacheDir, when not
// empty, keeps what was downloaded or inflated between runs.
func Open(location, cacheDir, device string) (*Image, error) {
	if !IsRemote(location) {
		return OpenFile(strings.TrimPrefix(location, "file://"), cacheDir, device)
	}
	remote, err := newRangeReader(location, cacheDir)
	if err != nil {
		return nil, err
	}
	archive, manifest, err := openArchive(remote, remote.size)
	if err != nil {
		remote.Close()
		return nil, err
	}
	return &Image{remote: remote, source: remote, cacheDir: cacheDir, archive: archive, manifest: manifest, device: device, volumes: map[string]*apfs.Reader{}}, nil
}

// IsRemote reports whether Open fetches location over HTTP(S), rather than reading a local file.
func IsRemote(location string) bool {
	return strings.HasPrefix(location, "https://") || strings.HasPrefix(location, "http://")
}

// OpenFile opens a local restore image (.ipsw) from disk. Deflate-compressed disk image members
// are inflated on demand without needing AllowStreaming, as nothing is downloaded for them.
func OpenFile(path, cacheDir, device string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s is a directory", path)
	}
	archive, manifest, err := openArchive(f, st.Size())
	if err != nil {
		f.Close()
		return nil, err
	}
	return &Image{source: f, file: f, cacheDir: cacheDir, archive: archive, manifest: manifest, device: device, streaming: true, volumes: map[string]*apfs.Reader{}}, nil
}

// openArchive reads a restore image's ZIP directory and build manifest.
func openArchive(source io.ReaderAt, size int64) (*zip.Reader, *buildManifest, error) {
	archive, err := zip.NewReader(source, size)
	if err != nil {
		return nil, nil, err
	}
	manifest, err := parseBuildManifest(archive.File)
	if err != nil {
		return nil, nil, err
	}
	return archive, manifest, nil
}

// Close releases any local image file, disk cache file, and inflated member files opened for this image.
func (m *Image) Close() error {
	var errs []error
	for _, f := range m.opened {
		errs = append(errs, f.Close())
	}
	m.opened = nil
	if m.file != nil {
		errs = append(errs, m.file.Close())
		m.file = nil
	}
	if m.remote != nil {
		errs = append(errs, m.remote.Close())
	}
	return errors.Join(errs...)
}

// Version is the product version in the build manifest, e.g. "15.6.1".
func (m *Image) Version() string { return m.manifest.ProductVersion }

// Build is the product build version in the build manifest, e.g. "24G90".
func (m *Image) Build() string { return m.manifest.ProductBuildVersion }

// Device is the BuildManifest product type or device class used to pick disk images.
func (m *Image) Device() string { return m.device }

// Board is the device class of the build identity Device picks, as the system reports it in
// hw.model: "D321AP" for an iPhone11,2. Empty when the manifest names none.
func (m *Image) Board() string {
	identities, err := m.deviceIdentities()
	if err != nil {
		return ""
	}
	for _, identity := range identities {
		if class := identity.Info.DeviceClass; class != "" {
			return strings.ToUpper(class)
		}
	}
	return ""
}

// DeviceTree is the device tree the restore image has for Device, as the boot loader is given
// it: flattened, with the values the boot loader fills in still to be filled. The error is
// ErrEncrypted's for a tree only the device can decrypt.
func (m *Image) DeviceTree() ([]byte, error) {
	return m.firmware("DeviceTree", "device tree", maxDeviceTreeSize)
}

// KernelCache is the kernel the restore image has for Device, with the extensions it starts
// with, unpacked. The error is ErrEncrypted's for one only the device can decrypt.
func (m *Image) KernelCache() ([]byte, error) {
	return m.firmware("KernelCache", "kernelcache", maxKernelCacheSize)
}

const maxKernelCacheSize = 256 << 20

// KernelReading is what read makes of the kernelcache, under a name for what that is. Unpacking
// a kernelcache takes a second or so, so where there is a cache directory the reading is kept
// there, for this kernelcache (the release's build, and its file in the restore image by name, size and checksum) and
// this name, and a later run is given it without the kernelcache being read at all. A name
// says how the reading is made as well as what it is of: one made another way has another name.
func (m *Image) KernelReading(name string, read func(kernelcache []byte) ([]byte, error)) ([]byte, error) {
	member, err := m.firmwareMember("KernelCache", "kernelcache")
	if err != nil {
		return nil, err
	}
	kept := ""
	if m.cacheDir != "" {
		kept = urlCachePath(m.cacheDir, "kept-", fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%08x", name, m.Build(), member.Name, member.UncompressedSize64, member.CRC32), "")
		// A reading is kept with its checksum before it: a file that is cut short or is not
		// what was written is no reading, and the kernelcache is read again.
		if file, err := os.ReadFile(kept); err == nil && len(file) >= sha256.Size {
			if reading := file[sha256.Size:]; sha256.Sum256(reading) == [sha256.Size]byte(file[:sha256.Size]) {
				return reading, nil
			}
		}
	}
	kernelcache, err := m.KernelCache()
	if err != nil {
		return nil, err
	}
	reading, err := read(kernelcache)
	if err == nil && kept != "" {
		if err := os.MkdirAll(m.cacheDir, 0o755); err == nil {
			sum := sha256.Sum256(reading)
			_ = writeFileAtomic(m.cacheDir, "kept-*.tmp", kept, append(sum[:], reading...)) // kept if it can be: the reading is good either way
		}
	}
	return reading, err
}

// firmware is the payload of the file the restore image's manifest has for Device under a key.
func (m *Image) firmware(key, what string, max int) ([]byte, error) {
	f, err := m.firmwareMember(key, what)
	if err != nil {
		return nil, err
	}
	raw, err := readMember(f, max)
	if err != nil {
		return nil, err
	}
	data, err := im4pPayloadData(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name, err)
	}
	return data, nil
}

// firmwareMember is the file of the restore image that its manifest has for Device under a key.
func (m *Image) firmwareMember(key, what string) (*zip.File, error) {
	identities, err := m.deviceIdentities()
	if err != nil {
		return nil, err
	}
	var name string
	for _, identity := range identities {
		p := identity.Manifest[key].Info.Path
		if strings.Contains(identity.Info.Variant, "Recovery") || p == "" {
			continue
		}
		if name != "" && name != p {
			return nil, fmt.Errorf("several %ss for %s (%s, %s)", what, m.device, name, p)
		}
		name = p
	}
	if name == "" {
		return nil, fmt.Errorf("the restore image names no %s for %s", what, m.device)
	}
	for _, f := range m.archive.File {
		if f.Name == name {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no ZIP member %s", name)
}

// Stats reports the range requests made so far and the bytes they returned.
func (m *Image) Stats() (requests, bytes int64) {
	if m.remote == nil {
		return 0, 0
	}
	return m.remote.requests.Load(), m.remote.bytes.Load()
}

// StreamProgress reports bytes downloaded and total compressed bytes while a deflate-compressed
// ZIP member is being streamed; total is 0 when no member is streaming.
func (m *Image) StreamProgress() (done, total int64) {
	if m.remote == nil {
		return 0, 0
	}
	total = m.remote.streamTotal.Load()
	if total <= 0 {
		return 0, 0
	}
	return min(m.remote.streamDone.Load(), total), total
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
	source, size, err := m.openMember(member)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, 4)
	if _, err := source.ReadAt(magic, 0); err != nil {
		return nil, err
	}
	if string(magic) == aeaMagic {
		metadata, err := readAEAMetadata(source, size)
		if err != nil {
			return nil, err
		}
		key, err := resolveAEAKey(metadata, m.cacheDir)
		if err != nil {
			return nil, fmt.Errorf("resolve AEA key: %w", err)
		}
		reader, err := openAEA(source, size, key)
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

// deviceIdentities is the build manifest's identities for Device, read once.
func (m *Image) deviceIdentities() ([]buildIdentity, error) {
	if m.identities == nil {
		ids, err := m.manifest.forDevice(m.device, m.archive.File)
		if err != nil {
			return nil, err
		}
		m.identities = ids
	}
	return m.identities, nil
}

func (m *Image) volume(kind string) (*apfs.Reader, error) {
	if v, ok := m.volumes[kind]; ok {
		return v, nil
	}
	identities, err := m.deviceIdentities()
	if err != nil {
		return nil, err
	}
	var name string
	switch kind {
	case FileSystem:
		name, err = fileSystemDmg(identities)
	case SystemOS:
		name, err = systemOsDmg(identities)
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
	m.volumes[kind] = v
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
	// Mode is its permission bits, with the sticky and set-id bits, and UID and GID its owner, as
	// its inode has them.
	Mode     uint32
	UID, GID uint32
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
	out := Entry{Kind: Missing}
	switch entry.Type {
	case uint16(types.DT_DIR):
		out.Kind = Directory
	case uint16(types.DT_LNK):
		out.Kind = Symlink
		if out.Target, err = v.Readlink(entry.ID); err != nil {
			return fail(err)
		}
	case uint16(types.DT_REG):
		f, err := v.OpenFileID(entry.ID)
		if err != nil {
			return fail(err)
		}
		out.Kind, out.File = Regular, &File{file: f, name: vol + ":" + path}
		out.Mode, out.UID, out.GID = uint32(f.Mode.Perm()), f.UID, f.GID
		for _, bit := range []struct {
			named fs.FileMode
			bit   uint32
		}{{fs.ModeSticky, 0o1000}, {fs.ModeSetgid, 0o2000}, {fs.ModeSetuid, 0o4000}} {
			if f.Mode&bit.named != 0 {
				out.Mode |= bit.bit
			}
		}
		return out, nil
	default:
		return out, nil
	}
	info, err := v.StatID(entry.ID)
	if err != nil {
		return fail(err)
	}
	out.Mode, out.UID, out.GID = uint32(info.RawMode)&0o7777, info.UID, info.GID
	return out, nil
}

// Open returns the regular file at path, or nil when there is none.
func (m *Image) Open(vol, path string) (*File, error) {
	entry, err := m.Stat(vol, path)
	return entry.File, err
}

// ReadDir returns the sorted entry names of a directory in vol, or nil when path is not a directory.
func (m *Image) ReadDir(vol, path string) ([]string, error) {
	v, err := m.volume(vol)
	if err != nil {
		return nil, fmt.Errorf("%s:%s: %w", vol, path, err)
	}
	entry, err := v.Lookup(strings.TrimPrefix(path, "/"))
	if err != nil || entry.Type != uint16(types.DT_DIR) {
		return nil, nil
	}
	children, err := v.ReadDirID(entry.ID)
	if err != nil {
		return nil, fmt.Errorf("%s:%s: %w", vol, path, err)
	}
	names := make([]string, 0, len(children))
	for _, child := range children {
		names = append(names, child.Name)
	}
	return names, nil
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
