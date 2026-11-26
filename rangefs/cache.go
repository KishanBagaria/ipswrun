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
	indexMagic      = "IRFC\x00\x00\x00\x02"
	indexHeaderSize = 16
	indexRecordSize = 24
	compactSlack    = 16 // rewrite index when at least this many redundant records accumulate
)

// diskCache stores fetched remote extents for one image packed sequentially in an append-only
// "data" file plus a compact binary "index" mapping remote offsets to file offsets. All data
// appends and index updates coordinate across concurrent processes via an exclusive flock on the
// data file. While open, it holds a shared flock on "lock" so PruneCache leaves the directory
// alone.
type diskCache struct {
	dir       string
	size      int64
	lockFile  *os.File
	dataFile  *os.File
	indexPath string
	index     os.FileInfo // the index file as of the last sync; its Size is how much was read
	spans     []span      // sorted, disjoint remote intervals present in the data file
}

// span maps the remote half-open interval [off, end) to [fileOff, fileOff+(end-off)) in dataFile.
type span struct {
	off, end int64
	fileOff  int64
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

// load reads the index, coalescing contiguous spans and compacting the index when redundant or
// corrupt records exist. A missing or incompatible-version index yields no spans. Data past the
// last indexed byte (from an older format or a put whose index append never landed) is dropped.
func (c *diskCache) load() error {
	if err := c.lock(syscall.LOCK_EX); err != nil {
		return err
	}
	defer c.unlock()

	f, err := c.openIndex()
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize, records, corrupt, err := c.syncLocked(f)
	if err != nil {
		return err
	}
	var live int64
	for _, s := range c.spans {
		live = max(live, s.fileOff+(s.end-s.off))
	}
	if dataSize > live {
		_ = c.dataFile.Truncate(live)
	}
	if corrupt || records-len(c.spans) >= compactSlack {
		_ = c.writeIndexLocked()
	}
	return nil
}

func (c *diskCache) openIndex() (*os.File, error) {
	return os.OpenFile(c.indexPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
}

// refresh picks up extents other processes have stored since this one last read the index. It is
// best effort: on failure the caller just fetches the bytes again.
func (c *diskCache) refresh() {
	st, err := os.Stat(c.indexPath)
	if err != nil || c.index != nil && os.SameFile(c.index, st) && st.Size() == c.index.Size() {
		return
	}
	if c.lock(syscall.LOCK_SH) != nil {
		return
	}
	defer c.unlock()
	if f, err := os.Open(c.indexPath); err == nil {
		_, _, _, _ = c.syncLocked(f)
		f.Close()
	}
}

// syncLocked brings spans up to date with the index file f while the caller holds the data flock.
// When f is the file last read, only the records appended since are parsed; when the index was
// replaced (compacted) or never read, spans are rebuilt from the whole file. It returns the data
// file's size, how many records it parsed and whether any were malformed.
func (c *diskCache) syncLocked(f *os.File) (dataSize int64, records int, corrupt bool, err error) {
	ds, err := c.dataFile.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	dataSize = ds.Size()
	st, err := f.Stat()
	if err != nil {
		return 0, 0, false, err
	}
	var from int64
	if c.index != nil && os.SameFile(c.index, st) && st.Size() >= c.index.Size() {
		from = c.index.Size()
	}
	b := make([]byte, st.Size()-from)
	if _, err := f.ReadAt(b, from); err != nil {
		return 0, 0, false, err
	}
	c.index = st
	if from == 0 {
		c.spans = nil
		if len(b) < indexHeaderSize || string(b[:8]) != indexMagic || int64(binary.LittleEndian.Uint64(b[8:16])) != c.size {
			// Unrecognized or older cache format: keep no records.
			return dataSize, 0, len(b) > 0, nil
		}
		b = b[indexHeaderSize:]
	}
	corrupt = len(b)%indexRecordSize != 0
	for at := 0; at+indexRecordSize <= len(b); at += indexRecordSize {
		off := int64(binary.LittleEndian.Uint64(b[at:]))
		fileOff := int64(binary.LittleEndian.Uint64(b[at+8:]))
		n := int64(binary.LittleEndian.Uint64(b[at+16:]))
		records++
		if off < 0 || fileOff < 0 || n <= 0 || n > c.size-off || n > dataSize-fileOff {
			corrupt = true
			continue
		}
		c.spans = insertSpan(c.spans, span{off: off, end: off + n, fileOff: fileOff})
	}
	return dataSize, records, corrupt, nil
}

func appendIndexHeader(b []byte, size int64) []byte {
	b = append(b, indexMagic...)
	return binary.LittleEndian.AppendUint64(b, uint64(size))
}

func appendIndexRecord(b []byte, off, fileOff, n int64) []byte {
	b = binary.LittleEndian.AppendUint64(b, uint64(off))
	b = binary.LittleEndian.AppendUint64(b, uint64(fileOff))
	return binary.LittleEndian.AppendUint64(b, uint64(n))
}

func (c *diskCache) writeIndexLocked() error {
	buf := appendIndexHeader(make([]byte, 0, indexHeaderSize+len(c.spans)*indexRecordSize), c.size)
	for _, s := range c.spans {
		buf = appendIndexRecord(buf, s.off, s.fileOff, s.end-s.off)
	}
	c.index = nil // a new file: the next sync rereads it whole
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

// put appends the parts of b (the remote bytes at off) that no process has cached yet to the packed
// data file, and records each one's (off, fileOff, len) entry in the index.
func (c *diskCache) put(off int64, b []byte) error {
	if err := c.lock(syscall.LOCK_EX); err != nil {
		return err
	}
	defer c.unlock()

	f, err := c.openIndex()
	if err != nil {
		return err
	}
	defer f.Close()
	fileOff, _, _, err := c.syncLocked(f)
	if err != nil {
		return err
	}
	var buf []byte
	if c.index.Size() == 0 {
		buf = appendIndexHeader(buf, c.size)
	}
	gaps := make([]span, 0, 4)
	for pos, end := off, off+int64(len(b)); pos < end; {
		s, next := findSpan(c.spans, pos, end)
		if s.end > pos {
			pos = s.end
			continue
		}
		next = min(next, end)
		if _, err := c.dataFile.WriteAt(b[pos-off:next-off], fileOff); err != nil {
			return err
		}
		buf = appendIndexRecord(buf, pos, fileOff, next-pos)
		gaps = append(gaps, span{off: pos, end: next, fileOff: fileOff})
		fileOff += next - pos
		pos = next
	}
	if len(gaps) == 0 {
		return nil
	}
	if _, err := f.Write(buf); err != nil {
		return err
	}
	if c.index, err = f.Stat(); err != nil {
		c.index = nil
	}
	for _, g := range gaps {
		c.spans = insertSpan(c.spans, g)
	}
	return nil
}

// read copies into p what the data file holds at pos, or returns 0 and where the next cached byte
// is. On a miss it first picks up what other processes have stored. A span that can no longer be
// read back (say the file was truncated) is forgotten.
func (c *diskCache) read(p []byte, pos int64) (int, int64) {
	refreshed := false
	for {
		s, next := findSpan(c.spans, pos, c.size)
		if s.end <= pos {
			if refreshed {
				return 0, next
			}
			c.refresh()
			refreshed = true
			continue
		}
		take := min(int64(len(p)), s.end-pos)
		fileOff := s.fileOff + (pos - s.off)
		if n, err := c.dataFile.ReadAt(p[:take], fileOff); err == nil {
			return n, 0
		}
		c.spans = slices.DeleteFunc(c.spans, func(x span) bool { return x == s })
	}
}

// insertSpan inserts s into a sorted slice of disjoint spans, trimming any overlapped portions of
// existing spans and merging neighbors that are contiguous in both remote and file space.
func insertSpan(spans []span, s span) []span {
	if s.end <= s.off {
		return spans
	}
	// spans[i:j) are all spans that strictly overlap s.
	i := sort.Search(len(spans), func(i int) bool { return spans[i].end > s.off })
	j := i
	for j < len(spans) && spans[j].off < s.end {
		j++
	}
	var repl [3]span
	n := 0
	if i < j && spans[i].off < s.off {
		repl[n] = span{off: spans[i].off, end: s.off, fileOff: spans[i].fileOff}
		n++
	}
	repl[n] = s
	n++
	if i < j && spans[j-1].end > s.end {
		r := spans[j-1]
		repl[n] = span{off: s.end, end: r.end, fileOff: r.fileOff + (s.end - r.off)}
		n++
	}
	// Merge with immediately adjacent spans when contiguous in both remote and file offsets.
	if i > 0 && contiguous(spans[i-1], repl[0]) {
		repl[0].off = spans[i-1].off
		repl[0].fileOff = spans[i-1].fileOff
		i--
	}
	if j < len(spans) && contiguous(repl[n-1], spans[j]) {
		repl[n-1].end = spans[j].end
		j++
	}
	return slices.Replace(spans, i, j, repl[:n]...)
}

func contiguous(a, b span) bool {
	return a.end == b.off && a.fileOff+(a.end-a.off) == b.fileOff
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

// urlCachePath names a small file in cacheDir for a key, which is a URL for most: prefix, a hash
// of the key, then suffix.
func urlCachePath(cacheDir, prefix, key, suffix string) string {
	sum := sha256.Sum256([]byte(key))
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

// PruneCache removes every cached restore image under cacheDir, along with remembered sizes,
// cached AEA keys and kept readings of kernelcaches, leaving anything else there alone. An image another process has open is skipped.
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
		case !entry.IsDir() && (strings.HasPrefix(name, "head-") || strings.HasPrefix(name, "fcs-") || strings.HasPrefix(name, "kept-")):
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
	bytes := dirSize(dir)
	return bytes, true, os.RemoveAll(dir)
}

func isCacheHashDir(name string) bool {
	if len(name) != 16 {
		return false
	}
	_, err := hex.DecodeString(name)
	return err == nil
}

func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
