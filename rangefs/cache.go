package rangefs

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	indexMagic      = "IRFC\x00\x00\x00\x01"
	indexHeaderSize = 16
	indexRecordSize = 16
	compactSlack    = 16 // rewrite index when at least this many redundant records accumulate
)

// diskCache stores fetched remote extents for one image in a single sparse file ("data") plus a
// small append-only binary index ("index"). All index reads and writes coordinate across
// concurrent processes via an exclusive flock on the data file. While open, it holds a shared
// flock on "lock" so PruneCache leaves the directory alone.
type diskCache struct {
	dir       string
	size      int64
	lockFile  *os.File
	dataFile  *os.File
	indexPath string
	spans     []span // coalesced intervals present in the data file
}

// span is a coalesced half-open interval [off, end) present in the sparse data file.
type span struct {
	off, end int64
}

func openDiskCache(cacheDir, url string, size int64) (*diskCache, error) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", url, size)))
	dir := filepath.Join(cacheDir, hex.EncodeToString(sum[:8]))
	lockFile, err := lockInUse(dir)
	if err != nil {
		return nil, err
	}
	dataFile, err := os.OpenFile(filepath.Join(dir, "data"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		lockFile.Close()
		return nil, err
	}
	dc := &diskCache{
		dir:       dir,
		size:      size,
		lockFile:  lockFile,
		dataFile:  dataFile,
		indexPath: filepath.Join(dir, "index"),
	}
	if err := dc.load(); err != nil {
		dc.close()
		return nil, err
	}
	return dc, nil
}

// lockInUse creates dir if needed and takes a shared flock on its "lock" file, which is released
// when the returned file is closed. If PruneCache removed dir while this waited for the lock, it
// starts over with a fresh directory.
func lockInUse(dir string) (*os.File, error) {
	path := filepath.Join(dir, "lock")
	for {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
			f.Close()
			return nil, err
		}
		held, err1 := f.Stat()
		named, err2 := os.Stat(path)
		if err1 == nil && err2 == nil && os.SameFile(held, named) {
			return f, nil
		}
		f.Close()
	}
}

func (c *diskCache) close() error {
	return errors.Join(c.dataFile.Close(), c.lockFile.Close())
}

func (c *diskCache) lock(how int) error {
	return syscall.Flock(int(c.dataFile.Fd()), how)
}

func (c *diskCache) unlock() {
	_ = syscall.Flock(int(c.dataFile.Fd()), syscall.LOCK_UN)
}

// load reads the index (or, when there is none yet, migrates any legacy per-extent hex files into
// the sparse store), coalescing overlapping or adjacent spans and compacting the index when
// redundant records exist.
func (c *diskCache) load() error {
	if err := c.lock(syscall.LOCK_EX); err != nil {
		return err
	}
	defer c.unlock()

	st, err := c.dataFile.Stat()
	if err != nil {
		return err
	}
	dataSize := st.Size()

	rawRecords := 0
	corruptIndex := false
	raw, err := os.ReadFile(c.indexPath)
	if err == nil {
		if len(raw) >= indexHeaderSize &&
			string(raw[:8]) == indexMagic && int64(binary.LittleEndian.Uint64(raw[8:16])) == c.size {
			corruptIndex = (len(raw)-indexHeaderSize)%indexRecordSize != 0
			for at := indexHeaderSize; at+indexRecordSize <= len(raw); at += indexRecordSize {
				off := int64(binary.LittleEndian.Uint64(raw[at:]))
				n := int64(binary.LittleEndian.Uint64(raw[at+8:]))
				rawRecords++
				if off < 0 || n <= 0 || n > c.size-off || off+n > dataSize {
					corruptIndex = true
					continue
				}
				c.spans = insertSpan(c.spans, span{off: off, end: off + n})
			}
		} else if len(raw) > 0 {
			corruptIndex = true
		}
	} else {
		// Legacy files can only predate the index, so look for them only when it is missing.
		migrated, err := c.migrateLegacy()
		if err != nil || migrated {
			return err
		}
	}

	if corruptIndex || rawRecords-len(c.spans) >= compactSlack {
		_ = c.writeIndexLocked()
	}
	return nil
}

// migrateLegacy imports any old-format per-extent files ("00000c0ffee0-1000") into the sparse data
// file and removes them once copied, so existing caches upgrade seamlessly without re-downloading.
func (c *diskCache) migrateLegacy() (bool, error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return false, err
	}
	var legacy []string
	for _, entry := range entries {
		name := entry.Name()
		first, second, ok := strings.Cut(name, "-")
		if !ok {
			continue
		}
		off, err1 := strconv.ParseInt(first, 16, 64)
		n, err2 := strconv.ParseInt(second, 16, 64)
		if err1 != nil || err2 != nil || name != extentName(off, n) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || n <= 0 || off < 0 || n > c.size-off || info.Size() != n {
			continue
		}
		path := filepath.Join(c.dir, name)
		b, err := os.ReadFile(path)
		if err != nil || int64(len(b)) != n {
			continue
		}
		if _, err := c.dataFile.WriteAt(b, off); err != nil {
			return false, err
		}
		c.spans = insertSpan(c.spans, span{off: off, end: off + n})
		legacy = append(legacy, path)
	}
	if len(legacy) == 0 {
		return false, nil
	}
	// Persist the index before removing legacy files so a crash never loses cached bytes.
	if err := c.writeIndexLocked(); err != nil {
		return false, err
	}
	for _, path := range legacy {
		_ = os.Remove(path)
	}
	return true, nil
}

func appendIndexHeader(b []byte, size int64) []byte {
	b = append(b, indexMagic...)
	return binary.LittleEndian.AppendUint64(b, uint64(size))
}

func appendIndexRecord(b []byte, off, n int64) []byte {
	b = binary.LittleEndian.AppendUint64(b, uint64(off))
	return binary.LittleEndian.AppendUint64(b, uint64(n))
}

func (c *diskCache) writeIndexLocked() error {
	buf := appendIndexHeader(make([]byte, 0, indexHeaderSize+len(c.spans)*indexRecordSize), c.size)
	for _, s := range c.spans {
		buf = appendIndexRecord(buf, s.off, s.end-s.off)
	}
	return writeFileAtomic(c.dir, "index-*.tmp", c.indexPath, buf)
}

// writeFileAtomic writes b to a temporary file in dir and renames it to path.
func writeFileAtomic(dir, pattern, path string, b []byte) error {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if err = errors.Join(err, tmp.Close()); err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// put writes b at off in the sparse data file and appends its record to the index.
func (c *diskCache) put(off int64, b []byte) error {
	if _, err := c.dataFile.WriteAt(b, off); err != nil {
		return err
	}
	if err := c.lock(syscall.LOCK_EX); err != nil {
		return err
	}
	defer c.unlock()

	f, err := os.OpenFile(c.indexPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	var buf []byte
	if st.Size() == 0 {
		buf = appendIndexHeader(buf, c.size)
	}
	if _, err := f.Write(appendIndexRecord(buf, off, int64(len(b)))); err != nil {
		return err
	}
	c.spans = insertSpan(c.spans, span{off: off, end: off + int64(len(b))})
	return nil
}

// read copies into p what the data file holds at pos, or returns 0 and where the next cached byte
// is. A span that can no longer be read back (say the file was truncated) is forgotten.
func (c *diskCache) read(p []byte, pos int64) (int, int64) {
	for {
		s, next := findSpan(c.spans, pos, c.size)
		if s.end <= pos {
			return 0, next
		}
		take := min(int64(len(p)), s.end-pos)
		if n, err := c.dataFile.ReadAt(p[:take], pos); err == nil && int64(n) == take {
			return n, 0
		}
		c.spans = slices.DeleteFunc(c.spans, func(x span) bool { return x == s })
	}
}

func extentName(off, n int64) string { return fmt.Sprintf("%012x-%x", off, n) }

// insertSpan merges s into a sorted slice of disjoint, non-adjacent spans.
func insertSpan(spans []span, s span) []span {
	if s.end <= s.off {
		return spans
	}
	// First span that could overlap or touch s (end >= s.off).
	i := sort.Search(len(spans), func(i int) bool { return spans[i].end >= s.off })
	j := i
	for j < len(spans) && spans[j].off <= s.end {
		s.off = min(s.off, spans[j].off)
		s.end = max(s.end, spans[j].end)
		j++
	}
	return slices.Replace(spans, i, j, s)
}

// findSpan returns the span covering pos, or an empty span and the offset where the next span begins.
func findSpan(spans []span, pos, limit int64) (span, int64) {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].off > pos })
	if i > 0 && pos < spans[i-1].end {
		return spans[i-1], 0
	}
	if i < len(spans) {
		return span{}, spans[i].off
	}
	return span{}, limit
}

// metaPath stores the last-known remote Content-Length for url so warm caches work offline if
// HEAD fails due to a network outage.
func metaPath(cacheDir, url string) string { return urlCachePath(cacheDir, "head-", url, "") }

// urlCachePath names a small per-URL file in cacheDir: prefix, a hash of url, then suffix.
func urlCachePath(cacheDir, prefix, url, suffix string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(cacheDir, prefix+hex.EncodeToString(sum[:8])+suffix)
}

func readCachedSize(cacheDir, url string) (int64, bool) {
	if cacheDir == "" {
		return 0, false
	}
	raw, err := os.ReadFile(metaPath(cacheDir, url))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return n, err == nil && n > 0
}

func writeCachedSize(cacheDir, url string, size int64) {
	if cacheDir == "" || size <= 0 {
		return
	}
	if cached, ok := readCachedSize(cacheDir, url); ok && cached == size {
		return
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}
	_ = writeFileAtomic(cacheDir, "head-*.tmp", metaPath(cacheDir, url), []byte(fmt.Sprintf("%d\n", size)))
}

// PruneCache removes every cached restore image under cacheDir, along with remembered sizes and
// cached AEA keys, leaving anything else there alone. An image another process has open is skipped.
// It reports how many images it removed, how many it skipped, and the disk bytes it freed.
func PruneCache(cacheDir string) (removed, inUse int, freed int64, err error) {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(cacheDir, name)
		switch {
		case entry.IsDir() && (isCacheHashDir(name) || name == "local"):
			bytes, ok, err := removeUnused(path)
			if err != nil {
				return removed, inUse, freed, err
			}
			if !ok {
				inUse++
				continue
			}
			removed++
			freed += bytes
		case !entry.IsDir() && (strings.HasPrefix(name, "head-") || strings.HasPrefix(name, "fcs-")):
			_ = os.Remove(path)
		}
	}
	return removed, inUse, freed, nil
}

// removeUnused removes dir unless an open diskCache holds its lock, reporting the bytes it held.
// The exclusive lock is kept until dir is gone, so a process opening it meanwhile waits and then
// starts a fresh directory.
func removeUnused(dir string) (int64, bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	bytes := dirAllocatedBytes(dir)
	return bytes, true, os.RemoveAll(dir)
}

func isCacheHashDir(name string) bool {
	if len(name) != 16 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

// dirAllocatedBytes returns the actual disk bytes allocated under dir, using filesystem block
// counts so sparse data files report their true footprint rather than their logical length.
func dirAllocatedBytes(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		} else {
			total += info.Size()
		}
		return nil
	})
	return total
}
