package rangefs

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// origin serves one blob by range and records what was asked of it.
type origin struct {
	data   []byte
	ranges [][2]int64 // start and length of each GET
	// breakAfter, when positive, cuts each of the next breaks bodies after that many bytes.
	breakAfter, breaks int
	// shift makes the server answer with a range other than the one requested.
	shift int64
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Accept-Ranges", "bytes")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", fmt.Sprint(len(o.data)))
		return
	}
	var start, last int64
	if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &last); err != nil {
		w.Write(o.data)
		return
	}
	o.ranges = append(o.ranges, [2]int64{start, last + 1 - start})
	start, last = start+o.shift, last+o.shift
	body := o.data[start : last+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, last, len(o.data)))
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	if o.breaks > 0 && o.breakAfter < len(body) {
		o.breaks--
		w.Write(body[:o.breakAfter])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // drops the connection mid-body
	}
	w.Write(body)
}

func serve(t *testing.T, size int) (*origin, string) {
	o := &origin{data: make([]byte, size)}
	rand.New(rand.NewSource(1)).Read(o.data)
	server := httptest.NewServer(o)
	t.Cleanup(server.Close)
	return o, server.URL
}

func read(t *testing.T, r *rangeReader, o *origin, off, n int64) {
	t.Helper()
	p := make([]byte, n)
	if _, err := r.ReadAt(p, off); err != nil {
		t.Fatalf("read %d+%d: %v", off, n, err)
	}
	if !bytes.Equal(p, o.data[off:off+n]) {
		t.Fatalf("read %d+%d returned the wrong bytes", off, n)
	}
}

func TestScatteredReadsAreExact(t *testing.T) {
	o, url := serve(t, 4<<20)
	r, err := newRangeReader(url, "")
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 1<<20, 4096)
	read(t, r, o, 3<<20, 100)
	read(t, r, o, 2<<20, 300000) // large and not sequential
	read(t, r, o, 1<<20-10, 4116)
	want := [][2]int64{{1 << 20, 4096}, {3 << 20, 100}, {2 << 20, 300000}, {1<<20 - 10, 10}, {1<<20 + 4096, 10}}
	if fmt.Sprint(o.ranges) != fmt.Sprint(want) {
		t.Fatalf("requested %v, want %v", o.ranges, want)
	}
	if requests, fetched := r.requests.Load(), r.bytes.Load(); requests != 5 || fetched != 4096+100+300000+20 {
		t.Fatalf("counted %d requests and %d bytes", requests, fetched)
	}
}

func TestSequentialSmallReadsAreReadAhead(t *testing.T) {
	o, url := serve(t, 4<<20)
	r, err := newRangeReader(url, "")
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 2<<20, 4096) // already there when the read-ahead reaches it
	for off := int64(2<<20 - 300000); off < 2<<20+8192; off += 4096 {
		read(t, r, o, off, 4096)
	}
	// Sequential but large: exact.
	read(t, r, o, 3<<20, 4096)
	read(t, r, o, 3<<20+4096, smallRead+1)
	want := [][2]int64{{2 << 20, 4096}, {2<<20 - 300000, 4096}, {2<<20 - 300000 + 4096, readAhead}, {2<<20 - 300000 + 4096 + readAhead, 300000 - 4096 - readAhead},
		{2<<20 + 4096, readAhead}, {3 << 20, 4096}, {3<<20 + 4096, smallRead + 1}}
	if fmt.Sprint(o.ranges) != fmt.Sprint(want) {
		t.Fatalf("requested %v, want %v", o.ranges, want)
	}
	// The end of the file bounds read-ahead.
	read(t, r, o, 4<<20-5000, 1000)
	read(t, r, o, 4<<20-4000, 1000)
	if last := o.ranges[len(o.ranges)-1]; last != [2]int64{4<<20 - 4000, 4000} {
		t.Fatalf("requested %v at the end", last)
	}
}

func TestBrokenBodyResumesWhereItStopped(t *testing.T) {
	o, url := serve(t, 4<<20)
	r, err := newRangeReader(url, "")
	if err != nil {
		t.Fatal(err)
	}
	o.breakAfter, o.breaks = 100000, 3
	read(t, r, o, 1000, 1<<20)
	want := [][2]int64{{1000, 1 << 20}, {101000, 1<<20 - 100000}, {201000, 1<<20 - 200000}, {301000, 1<<20 - 300000}}
	if fmt.Sprint(o.ranges) != fmt.Sprint(want) {
		t.Fatalf("requested %v, want %v", o.ranges, want)
	}
	if fetched := r.bytes.Load(); fetched != 1<<20 {
		t.Fatalf("counted %d bytes", fetched)
	}
	// Progress resets the count of fruitless requests, but the total is bounded.
	o.ranges, o.breaks, o.breakAfter = nil, 1000, 1000
	if _, err := r.ReadAt(make([]byte, 1<<20), 2<<20); err == nil || len(o.ranges) != rangeRequests {
		t.Fatalf("after %d requests: %v", len(o.ranges), err)
	}
}

func TestWrongRangeIsRefused(t *testing.T) {
	o, url := serve(t, 4<<20)
	r, err := newRangeReader(url, "")
	if err != nil {
		t.Fatal(err)
	}
	o.shift = 1
	if _, err := r.ReadAt(make([]byte, 100), 5000); err == nil || !strings.Contains(err.Error(), "did not honor") {
		t.Fatalf("a shifted range was accepted: %v", err)
	}
	if r.bytes.Load() != 0 || len(o.ranges) != 1 {
		t.Fatalf("%d bytes were taken from %d responses", r.bytes.Load(), len(o.ranges))
	}
}

func TestCacheDirectory(t *testing.T) {
	o, url := serve(t, 300000)
	dir := t.TempDir()
	r, err := newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 5000, 100)
	if b, err := os.ReadFile(filepath.Join(r.dir, extentName(5000, 100))); err != nil || !bytes.Equal(b, o.data[5000:5100]) {
		t.Fatalf("extent file: %v", err)
	}
	// Files that are not what their names say.
	os.WriteFile(filepath.Join(r.dir, "00000000"), o.data[:1000], 0o644)
	os.WriteFile(filepath.Join(r.dir, extentName(9000, 50)), o.data[:49], 0o644)
	os.WriteFile(filepath.Join(r.dir, "member-00000000-x.dmg"), o.data[:10], 0o644)

	o.ranges = nil
	r, err = newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 5000, 100)
	read(t, r, o, 9000, 50)
	want := [][2]int64{{9000, 50}}
	if fmt.Sprint(o.ranges) != fmt.Sprint(want) {
		t.Fatalf("requested %v, want %v", o.ranges, want)
	}
}

func TestMemoryIsBounded(t *testing.T) {
	o, url := serve(t, memBytes+3*maxRequest)
	r, err := newRangeReader(url, "")
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 0, int64(len(o.data)))
	if r.held > memBytes || len(r.extents) != len(r.loaded) {
		t.Fatalf("holding %d bytes in %d extents, %d indexed", r.held, len(r.loaded), len(r.extents))
	}
	read(t, r, o, 100, 1000) // released, so fetched again
	if last := o.ranges[len(o.ranges)-1]; last != [2]int64{100, 1000} {
		t.Fatalf("requested %v", last)
	}
}

func TestStreamedMemberResumes(t *testing.T) {
	content := bytes.Repeat([]byte("a deflate stream has one entry point, its start. "), 40000)
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	member, err := w.Create("disk.dmg")
	if err != nil {
		t.Fatal(err)
	}
	rand.New(rand.NewSource(2)).Read(content[:200000]) // keeps the compressed member long enough to break
	member.Write(content)
	w.Close()
	o := &origin{data: archive.Bytes()}
	server := httptest.NewServer(o)
	t.Cleanup(server.Close)
	r, err := newRangeReader(server.URL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files, err := zip.NewReader(r, r.size)
	if err != nil {
		t.Fatal(err)
	}
	o.ranges, o.breakAfter, o.breaks = nil, 50000, 3
	f, err := r.inflated(files.File[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(io.NewSectionReader(f, 0, int64(len(content))+1))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("inflated copy differs (%v)", err)
	}
	// The member's local header, then its data in four pieces.
	if len(o.ranges) != 5 || o.ranges[2][0] != o.ranges[1][0]+50000 || o.ranges[4] != [2]int64{o.ranges[1][0] + 150000, o.ranges[1][1] - 150000} {
		t.Fatalf("requested %v", o.ranges)
	}
}

// A URL that does not answer with the image's size fails with the URL and the status it gave.
func TestRefusedURLIsNamed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	_, err := Open(server.URL+"/x.ipsw", "", "Macmini9,1")
	var status *StatusError
	if !errors.As(err, &status) || status.URL != server.URL+"/x.ipsw" || status.Status != "403 Forbidden" {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), server.URL+"/x.ipsw answered 403 Forbidden") {
		t.Fatalf("message %q does not name the URL", err)
	}
}

func TestCompressedMemberIsRecognisable(t *testing.T) {
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	member, err := w.Create("disk.dmg") // deflated
	if err != nil {
		t.Fatal(err)
	}
	member.Write(bytes.Repeat([]byte("apfs"), 4096))
	w.Close()
	r, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = openStoredMember(bytes.NewReader(archive.Bytes()), r.File[0])
	if !errors.Is(err, ErrCompressedMember) || !strings.Contains(err.Error(), "disk.dmg is a compressed ZIP member") {
		t.Fatalf("got %v", err)
	}
}
