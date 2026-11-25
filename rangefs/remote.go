// Package rangefs reads files inside the disk images of a remote macOS restore image (IPSW).
// Nothing is downloaded whole: the ZIP directory, the image containers (AEA, UDIF, GPT), APFS
// metadata and file contents are all read through HTTP range requests.
package rangefs

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	smallRead  = 32 << 10  // a read this small that continues the previous one earns read-ahead
	readAhead  = 256 << 10 // what such a read fetches
	maxRequest = 8 << 20   // longest range asked for at once
	memBytes   = 64 << 20  // fetched data kept in memory when no disk cache is used

	// A range is given up after this many requests in a row that brought nothing, or this many
	// requests in all.
	stalledRequests = 4
	rangeRequests   = 32
)

// extent is a run of remote bytes held in memory when no disk cache is configured.
type extent struct {
	off, n int64
	data   []byte
}

// rangeReader is an HTTP io.ReaderAt. A read fetches the bytes it asks for and no more, unless it
// is small and starts where the previous read ended: scattered lookups stay exact, and only a
// sequential stream of small reads is read ahead. With a cache directory, fetched bytes are kept
// in a single sparse file plus a compact binary index; without one, they are kept in memory up to
// memBytes. A response that is not exactly the requested range is an error.
type rangeReader struct {
	mu          sync.Mutex
	client      *http.Client
	url         string
	size        int64
	dir         string
	disk        *diskCache
	spans       []span    // coalesced cached intervals on disk (when disk != nil)
	extents     []*extent // in-memory extents by offset (when disk == nil)
	loaded      []*extent // in-memory extents, oldest first (when disk == nil)
	held        int64
	lastEnd     int64 // where the previous read ended
	requests    atomic.Int64
	bytes       atomic.Int64
	streamStart atomic.Int64
	streamTotal atomic.Int64
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
		// Fall back to cached Content-Length so warm caches remain usable offline.
		if cached, ok := readCachedSize(cacheDir, url); ok {
			r.size = cached
		} else {
			return nil, err
		}
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.ContentLength <= 0 {
			return nil, &StatusError{URL: url, Status: resp.Status}
		}
		if resp.Header.Get("Accept-Ranges") != "bytes" {
			return nil, errors.New("server does not advertise byte ranges")
		}
		r.size = resp.ContentLength
		writeCachedSize(cacheDir, url, r.size)
	}
	if cacheDir != "" {
		dc, spans, err := openDiskCache(cacheDir, url, r.size)
		if err != nil {
			return nil, err
		}
		r.disk, r.dir, r.spans = dc, dc.dir, spans
	}
	return r, nil
}

func (r *rangeReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disk != nil {
		err := r.disk.close()
		r.disk = nil
		return err
	}
	return nil
}

// add files e in the in-memory index unless a known extent already covers it, dropping those it covers.
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

// find returns the in-memory extent holding the byte at pos, or nil and where the next extent begins.
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

// hold keeps e's bytes in memory, releasing the oldest held to stay within memBytes.
func (r *rangeReader) hold(e *extent, b []byte) {
	e.data = b
	r.loaded = append(r.loaded, e)
	r.held += e.n
	for r.held > memBytes && len(r.loaded) > 1 {
		oldest := r.loaded[0]
		r.loaded = r.loaded[1:]
		r.held -= oldest.n
		oldest.data = nil
		r.drop(oldest)
	}
}

// fetch downloads [start, end) and stores it in the disk cache (or in memory when uncached).
func (r *rangeReader) fetch(start, end int64) error {
	b := make([]byte, end-start)
	body := r.open(r.client, start, end)
	_, err := io.ReadFull(body, b)
	body.Close()
	if err != nil {
		return err
	}
	if r.disk != nil {
		if err := r.disk.put(start, b); err != nil {
			return fmt.Errorf("write cache: %w", err)
		}
		r.spans = insertSpan(r.spans, span{off: start, end: end})
		return nil
	}
	e := &extent{off: start, n: end - start}
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
		if r.disk != nil {
			s, next := findSpan(r.spans, pos, r.size)
			if s.end > pos {
				take := min(end, s.end) - pos
				if err := r.disk.readAt(p[n:n+int(take)], pos); err == nil {
					n += int(take)
					pos += take
					continue
				}
				// Cache read failed (e.g. external truncation): drop stale span and re-fetch.
				r.spans = slices.DeleteFunc(r.spans, func(x span) bool { return x == s })
				continue
			}
			stop := min(end, next)
			if ahead {
				stop = min(max(stop, pos+readAhead), next)
			}
			if err := r.fetch(pos, min(stop, pos+maxRequest)); err != nil {
				return n, err
			}
			continue
		}
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
		c := copy(p[n:], e.data[pos-e.off:])
		n += c
		pos += int64(c)
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
