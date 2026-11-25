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
	"time"
)

const (
	indexMagic      = "IRFC\x00\x00\x00\x01"
	indexHeaderSize = 16
	indexRecordSize = 16
	compactSlack    = 16 // rewrite index when at least this many redundant records accumulate
)

// diskCache stores fetched remote extents for one image in a single sparse file ("data") plus a
// small append-only binary index ("index"). All index reads and writes coordinate across
// concurrent processes via an exclusive flock on the data file.
type diskCache struct {
	dir       string
	size      int64
	dataFile  *os.File
	indexPath string
}

// span is a coalesced half-open interval [off, end) present in the sparse data file.
type span struct {
	off, end int64
}

func openDiskCache(cacheDir, url string, size int64) (*diskCache, []span, error) {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", url, size)))
	dir := filepath.Join(cacheDir, hex.EncodeToString(sum[:8]))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	// Record last-used time on the release directory so PruneCache can evict oldest-used first.
	now := time.Now()
	_ = os.Chtimes(dir, now, now)

	dataPath := filepath.Join(dir, "data")
	dataFile, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, nil, err
	}
	dc := &diskCache{
		dir:       dir,
		size:      size,
		dataFile:  dataFile,
		indexPath: filepath.Join(dir, "index"),
	}
	spans, err := dc.load()
	if err != nil {
		dataFile.Close()
		return nil, nil, err
	}
	return dc, spans, nil
}

func (c *diskCache) close() error {
	if c == nil || c.dataFile == nil {
		return nil
	}
	err := c.dataFile.Close()
	c.dataFile = nil
	return err
}

func (c *diskCache) lock(how int) error {
	return syscall.Flock(int(c.dataFile.Fd()), how)
}

func (c *diskCache) unlock() {
	_ = syscall.Flock(int(c.dataFile.Fd()), syscall.LOCK_UN)
}

// load reads the index (and migrates any legacy per-extent hex files into the sparse store),
// coalescing overlapping or adjacent spans and compacting the index when redundant records exist.
func (c *diskCache) load() ([]span, error) {
	if err := c.lock(syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer c.unlock()

	st, err := c.dataFile.Stat()
	if err != nil {
		return nil, err
	}
	dataSize := st.Size()

	var spans []span
	rawRecords := 0
	corruptIndex := false
	if raw, err := os.ReadFile(c.indexPath); err == nil {
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
				spans = insertSpan(spans, span{off: off, end: off + n})
			}
		} else if len(raw) > 0 {
			corruptIndex = true
		}
	}

	migrated, err := c.migrateLegacy(&spans)
	if err != nil {
		return nil, err
	}

	if corruptIndex || migrated || rawRecords-len(spans) >= compactSlack {
		_ = c.writeIndexLocked(spans)
	}
	return spans, nil
}

// migrateLegacy imports any old-format per-extent files ("00000c0ffee0-1000") into the sparse data
// file and removes them once copied, so existing caches upgrade seamlessly without re-downloading.
func (c *diskCache) migrateLegacy(spans *[]span) (bool, error) {
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
		*spans = insertSpan(*spans, span{off: off, end: off + n})
		legacy = append(legacy, path)
	}
	if len(legacy) == 0 {
		return false, nil
	}
	// Persist the index before removing legacy files so a crash never loses cached bytes.
	if err := c.writeIndexLocked(*spans); err != nil {
		return false, err
	}
	for _, path := range legacy {
		_ = os.Remove(path)
	}
	return true, nil
}

func (c *diskCache) writeIndexLocked(spans []span) error {
	buf := make([]byte, indexHeaderSize+len(spans)*indexRecordSize)
	copy(buf[:8], indexMagic)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(c.size))
	for i, s := range spans {
		at := indexHeaderSize + i*indexRecordSize
		binary.LittleEndian.PutUint64(buf[at:], uint64(s.off))
		binary.LittleEndian.PutUint64(buf[at+8:], uint64(s.end-s.off))
	}
	tmp, err := os.CreateTemp(c.dir, "index-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(buf)
	if err = errors.Join(err, tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), c.indexPath); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
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
	if st.Size() == 0 {
		var hdr [indexHeaderSize + indexRecordSize]byte
		copy(hdr[:8], indexMagic)
		binary.LittleEndian.PutUint64(hdr[8:16], uint64(c.size))
		binary.LittleEndian.PutUint64(hdr[16:24], uint64(off))
		binary.LittleEndian.PutUint64(hdr[24:32], uint64(len(b)))
		_, err = f.Write(hdr[:])
		return err
	}
	var rec [indexRecordSize]byte
	binary.LittleEndian.PutUint64(rec[0:8], uint64(off))
	binary.LittleEndian.PutUint64(rec[8:16], uint64(len(b)))
	_, err = f.Write(rec[:])
	return err
}

func (c *diskCache) readAt(p []byte, off int64) error {
	n, err := c.dataFile.ReadAt(p, off)
	if err != nil || n != len(p) {
		return errors.Join(err, errors.New("short read from cache"))
	}
	return nil
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
func metaPath(cacheDir, url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(cacheDir, "head-"+hex.EncodeToString(sum[:8]))
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
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(cacheDir, "head-*.tmp")
	if err != nil {
		return
	}
	_, err = fmt.Fprintf(tmp, "%d\n", size)
	if err = errors.Join(err, tmp.Close()); err != nil {
		os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), metaPath(cacheDir, url)); err != nil {
		os.Remove(tmp.Name())
	}
}

// PruneCache removes cached restore image directories under cacheDir, oldest-used first, until
// the allocated disk usage is at most maxBytes. Passing maxBytes <= 0 removes all cached images.
func PruneCache(cacheDir string, maxBytes int64) (removed int, freed int64, err error) {
	entries, err := os.ReadDir(cacheDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	type item struct {
		path  string
		mtime time.Time
		bytes int64
	}
	var items []item
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(cacheDir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		switch {
		case entry.IsDir() && (isCacheHashDir(name) || name == "local"):
			sz := dirAllocatedBytes(path)
			items = append(items, item{path: path, mtime: info.ModTime(), bytes: sz})
			total += sz
		case !entry.IsDir() && strings.HasPrefix(name, "head-") && maxBytes <= 0:
			_ = os.Remove(path)
		}
	}
	if maxBytes > 0 && total <= maxBytes {
		return 0, 0, nil
	}
	slices.SortFunc(items, func(a, b item) int { return a.mtime.Compare(b.mtime) })
	for _, it := range items {
		if maxBytes > 0 && total <= maxBytes {
			break
		}
		if err := os.RemoveAll(it.path); err != nil {
			return removed, freed, err
		}
		removed++
		freed += it.bytes
		total -= it.bytes
	}
	return removed, freed, nil
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
