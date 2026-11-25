// Package rangefs reads files inside the disk images of a macOS or iOS restore image (IPSW),
// remote or on disk.
// Nothing is downloaded whole: the ZIP directory, the image containers (AEA, UDIF, GPT), APFS
// metadata and file contents are all read through HTTP range requests.
package rangefs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	smallRead  = 32 << 10  // a read this small that continues the previous one earns read-ahead
	readAhead  = 256 << 10 // what such a read fetches
	maxRequest = 8 << 20   // longest range asked for at once
	memBytes   = 64 << 20  // fetched data kept in memory

	// A range is given up after this many requests in a row that brought nothing, or this many
	// requests in all.
	stalledRequests = 4
	rangeRequests   = 32
)

// extent is a run of remote bytes that has been fetched: held in memory, kept in a file of the
// cache directory, or both.
type extent struct {
	off, n int64
	file   string
	data   []byte
}

// rangeReader is an HTTP io.ReaderAt. A read fetches the bytes it asks for and no more, unless it
// is small and starts where the previous read ended: scattered lookups stay exact, and only a
// sequential stream of small reads is read ahead. What was fetched is kept as extents, in memory
// and optionally on disk. A response that is not exactly the requested range is an error.
type rangeReader struct {
	mu              sync.Mutex
	client          *http.Client
	url             string
	size            int64
	dir             string
	extents         []*extent // by offset; none lies inside another, so ends are ordered too
	loaded          []*extent // those holding data, oldest first
	held            int64
	lastEnd         int64 // where the previous read ended
	requests, bytes atomic.Int64
}

// StatusError is a restore image URL that did not answer the opening request with its size.
type StatusError struct {
	URL, Status string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("remote size unavailable: %s answered %s", e.URL, e.Status)
}

func newRangeReader(url, cacheDir string) (*rangeReader, error) {
	r := &rangeReader{client: &http.Client{Timeout: 5 * time.Minute}, url: url, lastEnd: -1}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
		return nil, &StatusError{URL: url, Status: resp.Status}
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return nil, errors.New("server does not advertise byte ranges")
	}
	r.size = resp.ContentLength
	if cacheDir != "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", url, r.size)))
		r.dir = filepath.Join(cacheDir, hex.EncodeToString(sum[:8]))
		if err := os.MkdirAll(r.dir, 0o755); err != nil {
			return nil, err
		}
		if err := r.index(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// index lists what the cache directory already holds. An extent is a file named by its offset
// and length in hex, "00000c0ffee0-1000". Anything else, and any file whose size is not what its
// name says, is left alone.
func (r *rangeReader) index() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		first, second, _ := strings.Cut(name, "-")
		off, err1 := strconv.ParseInt(first, 16, 64)
		n, err2 := strconv.ParseInt(second, 16, 64)
		if err1 != nil || err2 != nil || name != extentName(off, n) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || n <= 0 || off < 0 || n > r.size-off || info.Size() != n {
			continue
		}
		r.add(&extent{off: off, n: n, file: filepath.Join(r.dir, name)})
	}
	return nil
}

func extentName(off, n int64) string { return fmt.Sprintf("%012x-%x", off, n) }

// add files e in the index unless a known extent already covers it, dropping those it covers.
func (r *rangeReader) add(e *extent) {
	i := sort.Search(len(r.extents), func(i int) bool { return r.extents[i].off >= e.off })
	if i > 0 && r.extents[i-1].off+r.extents[i-1].n >= e.off+e.n {
		return
	}
	if i < len(r.extents) && r.extents[i].off == e.off && r.extents[i].n >= e.n {
		return
	}
	j := i
	for j < len(r.extents) && r.extents[j].off+r.extents[j].n <= e.off+e.n {
		j++
	}
	r.extents = slices.Replace(r.extents, i, j, e)
}

func (r *rangeReader) drop(e *extent) {
	if i := slices.Index(r.extents, e); i >= 0 {
		r.extents = slices.Delete(r.extents, i, i+1)
	}
}

// find returns the extent holding the byte at pos, or nil and where the next extent begins.
func (r *rangeReader) find(pos int64) (*extent, int64) {
	i := sort.Search(len(r.extents), func(i int) bool { return r.extents[i].off > pos })
	if i > 0 && pos < r.extents[i-1].off+r.extents[i-1].n {
		return r.extents[i-1], 0
	}
	if i < len(r.extents) {
		return nil, r.extents[i].off
	}
	return nil, r.size
}

// contents returns e's bytes, or nil when its file is no longer usable; e is then forgotten.
func (r *rangeReader) contents(e *extent) []byte {
	if e.data == nil {
		b, err := os.ReadFile(e.file)
		if err != nil || int64(len(b)) != e.n {
			r.drop(e)
			return nil
		}
		r.hold(e, b)
	}
	return e.data
}

// hold keeps e's bytes in memory, releasing the oldest held to stay within memBytes. An extent
// that is in no file is forgotten when its bytes are released.
func (r *rangeReader) hold(e *extent, b []byte) {
	e.data = b
	r.loaded = append(r.loaded, e)
	r.held += e.n
	for r.held > memBytes && len(r.loaded) > 1 {
		oldest := r.loaded[0]
		r.loaded = r.loaded[1:]
		r.held -= oldest.n
		oldest.data = nil
		if oldest.file == "" {
			r.drop(oldest)
		}
	}
}

// fetch downloads [start, end) and keeps it as one extent.
func (r *rangeReader) fetch(start, end int64) error {
	b := make([]byte, end-start)
	body := r.open(r.client, start, end)
	_, err := io.ReadFull(body, b)
	body.Close()
	if err != nil {
		return err
	}
	e := &extent{off: start, n: end - start}
	if r.dir != "" {
		// Another process may be filling the same directory: only complete files get a name.
		if tmp, err := os.CreateTemp(r.dir, "partial-*"); err == nil {
			name := filepath.Join(r.dir, extentName(e.off, e.n))
			_, err := tmp.Write(b)
			if err = errors.Join(err, tmp.Close()); err == nil {
				err = os.Rename(tmp.Name(), name)
			}
			if err == nil {
				e.file = name
			} else {
				os.Remove(tmp.Name())
			}
		}
	}
	r.add(e)
	r.hold(e, b)
	return nil
}

func (r *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	end := min(off+int64(len(p)), r.size)
	ahead := off == r.lastEnd && len(p) <= smallRead
	r.lastEnd = end
	n := 0
	for pos := off; pos < end; {
		e, next := r.find(pos)
		if e == nil {
			// Exactly the gap this read needs, or up to readAhead of it; never what is already here.
			stop := min(end, next)
			if ahead {
				stop = min(max(stop, pos+readAhead), next)
			}
			if err := r.fetch(pos, min(stop, pos+maxRequest)); err != nil {
				return n, err
			}
			continue
		}
		if b := r.contents(e); b != nil {
			c := copy(p[n:], b[pos-e.off:])
			n += c
			pos += int64(c)
		}
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// rangeBody reads the remote bytes [off, end) front to back. When a response body breaks partway
// it carries on from the byte reached with a new range request instead of starting over, as it
// does after a request that failed or met a server error. Every response has to be exactly the
// range asked for; one that is not ends the read at once.
type rangeBody struct {
	r        *rangeReader
	client   *http.Client
	off, end int64
	body     io.ReadCloser
	requests int
	stalled  int
	refused  error // an answer that was not the range asked for: final, never retried
}

func (r *rangeReader) open(client *http.Client, off, end int64) *rangeBody {
	return &rangeBody{r: r, client: client, off: off, end: end}
}

func (b *rangeBody) request() error {
	req, err := http.NewRequest(http.MethodGet, b.r.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", b.off, b.end-1))
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		resp.Body.Close()
		return fmt.Errorf("server answered %s", resp.Status)
	}
	want := fmt.Sprintf("bytes %d-%d/%d", b.off, b.end-1, b.r.size)
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != want ||
		(resp.ContentLength >= 0 && resp.ContentLength != b.end-b.off) {
		resp.Body.Close()
		b.refused = fmt.Errorf("server did not honor the range request (%s, %q, %d bytes): refusing a full download",
			resp.Status, resp.Header.Get("Content-Range"), resp.ContentLength)
		return b.refused
	}
	b.body = resp.Body
	b.r.requests.Add(1)
	return nil
}

func (b *rangeBody) Read(p []byte) (int, error) {
	for b.off < b.end {
		if b.refused != nil {
			return 0, b.refused
		}
		var n int
		var err error
		if b.body == nil {
			if b.requests == rangeRequests || b.stalled == stalledRequests {
				return 0, fmt.Errorf("range request abandoned at byte %d", b.off)
			}
			if b.stalled > 0 {
				time.Sleep(time.Duration(b.stalled) * time.Second)
			}
			b.requests++
			b.stalled++
			err = b.request()
		}
		if err == nil {
			n, err = b.body.Read(p[:min(int64(len(p)), b.end-b.off)])
			b.off += int64(n)
			b.r.bytes.Add(int64(n))
		}
		if n > 0 {
			b.stalled = 0
		}
		if err != nil {
			b.Close()
			if b.off < b.end && b.refused == nil && (b.requests == rangeRequests || b.stalled == stalledRequests) {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return n, fmt.Errorf("range request failed at byte %d: %w", b.off, err)
			}
		}
		if n > 0 {
			return n, nil
		}
	}
	return 0, io.EOF
}

func (b *rangeBody) Close() error {
	if b.body != nil {
		b.body.Close()
		b.body = nil
	}
	return nil
}
