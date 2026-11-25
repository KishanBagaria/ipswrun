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
	"slices"
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
	o, url := serve(t, 1<<20)
	dir := t.TempDir()
	r, err := newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Scattered reads at high and low remote offsets pack sequentially in data without sparse holes.
	read(t, r, o, 900000, 100)
	read(t, r, o, 5000, 100)
	if st, err := os.Stat(filepath.Join(r.dir, "data")); err != nil || st.Size() != 200 {
		t.Fatalf("packed data file stat = %v (err %v), want exact 200 bytes", st, err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "index")); err != nil {
		t.Fatalf("missing index file: %v", err)
	}
	// Sequential fetches append contiguously in both remote and file space, coalescing into one span
	// and triggering index compaction on reopen.
	for i := 0; i < compactSlack+4; i++ {
		off := int64(10000 + i*(smallRead+100))
		read(t, r, o, off, smallRead+100)
	}
	wantDataSize := int64(200 + (compactSlack+4)*(smallRead+100))
	if st, err := os.Stat(filepath.Join(r.dir, "data")); err != nil || st.Size() != wantDataSize {
		t.Fatalf("packed data size = %v (err %v), want %d", st, err, wantDataSize)
	}
	cacheSubdir := r.dir
	r.Close()

	// Append corrupt trailing bytes to index; reopen must recover valid spans and rewrite aligned index.
	f, err := os.OpenFile(filepath.Join(cacheSubdir, "index"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("corrupt"))
	f.Close()

	o.ranges = nil
	r, err = newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 900000, 100)
	read(t, r, o, 5000, 100)
	read(t, r, o, 10000, int64(compactSlack+4)*(smallRead+100))
	if len(o.ranges) != 0 {
		t.Fatalf("expected cache hits, got ranges %v", o.ranges)
	}
	// Index should have been compacted down to header + 3 spans (5000, coalesced 10000.., 900000).
	st, err := os.Stat(filepath.Join(cacheSubdir, "index"))
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(indexHeaderSize + 3*indexRecordSize); st.Size() != want {
		t.Fatalf("compacted index size = %d, want %d", st.Size(), want)
	}

	// Truncating data below a cached span causes re-fetch on read.
	if err := os.Truncate(filepath.Join(cacheSubdir, "data"), 150); err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 5000, 100)
	if len(o.ranges) == 0 {
		t.Fatalf("expected re-fetch after data truncation")
	}
	r.Close()

	// An incompatible/older index magic resets data to empty and starts clean.
	if err := os.WriteFile(filepath.Join(cacheSubdir, "index"), []byte("IRFC\x00\x00\x00\x01xxxxxxxx"), 0o644); err != nil {
		t.Fatal(err)
	}
	o.ranges = nil
	r, err = newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st, err := os.Stat(filepath.Join(cacheSubdir, "data")); err != nil || st.Size() != 0 {
		t.Fatalf("stale-version data size = %v (err %v), want 0", st, err)
	}
	read(t, r, o, 900000, 100)
	if len(o.ranges) != 1 {
		t.Fatalf("expected re-fetch after version reset, got %v", o.ranges)
	}
}

func TestInsertSpanOverlapAndCoalesce(t *testing.T) {
	var spans []span
	// Adjacent in remote and file space coalesces.
	spans = insertSpan(spans, span{off: 100, end: 200, fileOff: 0})
	spans = insertSpan(spans, span{off: 200, end: 300, fileOff: 100})
	if len(spans) != 1 || spans[0] != (span{off: 100, end: 300, fileOff: 0}) {
		t.Fatalf("contiguous coalesce = %v", spans)
	}
	// Adjacent in remote space but non-contiguous in file space stays two spans.
	spans = insertSpan(spans, span{off: 300, end: 400, fileOff: 500})
	if len(spans) != 2 {
		t.Fatalf("non-contiguous adjacent = %v, want 2 spans", spans)
	}
	// Overlapping insert splits the covered span cleanly with adjusted fileOff.
	spans = insertSpan(spans, span{off: 150, end: 250, fileOff: 900})
	want := []span{
		{off: 100, end: 150, fileOff: 0},
		{off: 150, end: 250, fileOff: 900},
		{off: 250, end: 300, fileOff: 150},
		{off: 300, end: 400, fileOff: 500},
	}
	if !slices.Equal(spans, want) {
		t.Fatalf("overlap split = %v, want %v", spans, want)
	}
}

func TestOfflineHeadFallback(t *testing.T) {
	o := &origin{data: make([]byte, 300000)}
	rand.New(rand.NewSource(3)).Read(o.data)
	server := httptest.NewServer(o)
	dir := t.TempDir()

	r, err := newRangeReader(server.URL, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r, o, 5000, 4096)
	r.Close()
	server.Close() // origin is now unreachable

	r2, err := newRangeReader(server.URL, dir)
	if err != nil {
		t.Fatalf("offline reopen failed: %v", err)
	}
	defer r2.Close()
	read(t, r2, o, 5000, 4096)
	if _, err := r2.ReadAt(make([]byte, 100), 100000); err == nil {
		t.Fatal("uncached read succeeded while offline")
	}
}

func TestPruneCache(t *testing.T) {
	o1, url1 := serve(t, 300000)
	o2, url2 := serve(t, 300000)
	dir := t.TempDir()

	r1, err := newRangeReader(url1, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r1, o1, 0, 128<<10)
	dir1 := r1.dir
	r1.Close()

	r2, err := newRangeReader(url2, dir)
	if err != nil {
		t.Fatal(err)
	}
	read(t, r2, o2, 0, 128<<10)
	dir2 := r2.dir
	r2.Close()

	// Plant an unrelated file that must never be pruned.
	keep := filepath.Join(dir, "keep-me")
	if err := os.WriteFile(keep, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	// An image another reader has open is left alone.
	open, err := newRangeReader(url2, dir)
	if err != nil {
		t.Fatal(err)
	}
	removed, inUse, freed, err := PruneCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || inUse != 1 || freed <= 0 {
		t.Fatalf("prune while open: removed=%d inUse=%d freed=%d, want 1, 1 and >0", removed, inUse, freed)
	}
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("open dir2 was removed: %v", err)
	}
	read(t, open, o2, 0, 128<<10)
	open.Close()

	removed, inUse, freed, err = PruneCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || inUse != 0 || freed <= 0 {
		t.Fatalf("prune: removed=%d inUse=%d freed=%d, want 1, 0 and >0", removed, inUse, freed)
	}
	if _, err := os.Stat(dir1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dir1 still exists after prune: %v", err)
	}
	if _, err := os.Stat(dir2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dir2 still exists after full prune: %v", err)
	}
	if _, err := os.Stat(metaPath(dir, url2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("head metadata file still exists after full prune: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
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
	defer r.Close()
	img := &Image{remote: r}
	files, err := zip.NewReader(r, r.size)
	if err != nil {
		t.Fatal(err)
	}
	o.ranges, o.breakAfter, o.breaks = nil, 50000, 3
	// Observe StreamProgress mid-stream via a wrapper around the origin on the resumed chunk.
	var midDone, midTotal int64
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if len(o.ranges) == 2 {
			midDone, midTotal = img.StreamProgress()
		}
		o.ServeHTTP(w, req)
	}))
	defer wrapped.Close()
	r.url = wrapped.URL
	f, err := r.inflated(files.File[0])
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if midTotal != int64(files.File[0].CompressedSize64) || midDone <= 0 || midDone >= midTotal {
		t.Fatalf("mid-stream progress = (%d, %d), want (0 < done < %d)", midDone, midTotal, files.File[0].CompressedSize64)
	}
	if done, total := img.StreamProgress(); done != 0 || total != 0 {
		t.Fatalf("post-stream progress = (%d, %d), want (0, 0)", done, total)
	}
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

func TestOpenLocalIPSW(t *testing.T) {
	manifest := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ProductVersion</key>
	<string>15.6.1</string>
	<key>ProductBuildVersion</key>
	<string>24G90</string>
	<key>SupportedProductTypes</key>
	<array><string>Macmini9,1</string></array>
</dict>
</plist>`
	storedData := bytes.Repeat([]byte("stored-member-data "), 256)
	deflatedData := bytes.Repeat([]byte("deflated-member-data "), 256)

	ipswPath := filepath.Join(t.TempDir(), "local.ipsw")
	f, err := os.Create(ipswPath)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	mw, err := w.Create("BuildManifest.plist")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mw.Write([]byte(manifest)); err != nil {
		t.Fatal(err)
	}
	sw, err := w.CreateHeader(&zip.FileHeader{Name: "stored.dmg", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sw.Write(storedData); err != nil {
		t.Fatal(err)
	}
	dw, err := w.CreateHeader(&zip.FileHeader{Name: "deflated.dmg", Method: zip.Deflate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dw.Write(deflatedData); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	for _, loc := range []string{ipswPath, "file://" + ipswPath} {
		img, err := Open(loc, cacheDir, "Mac16,1")
		if err != nil {
			t.Fatalf("Open(%q): %v", loc, err)
		}
		if img.Version() != "15.6.1" || img.Build() != "24G90" || img.Device() != "Mac16,1" {
			t.Errorf("Open(%q): version=%q build=%q device=%q", loc, img.Version(), img.Build(), img.Device())
		}
		if reqs, fetched := img.Stats(); reqs != 0 || fetched != 0 {
			t.Errorf("local image reported %d requests, %d bytes", reqs, fetched)
		}
		// Both stored and deflated members open directly from a local IPSW without AllowStreaming.
		for name, want := range map[string][]byte{"stored.dmg": storedData, "deflated.dmg": deflatedData} {
			var member *zip.File
			for _, zf := range img.archive.File {
				if zf.Name == name {
					member = zf
				}
			}
			ra, size, err := img.openMember(member)
			if err != nil || size != int64(len(want)) {
				t.Fatalf("openMember(%s): size=%d err=%v", name, size, err)
			}
			got := make([]byte, len(want))
			if _, err := ra.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("read %s: err=%v equal=%v", name, err, bytes.Equal(got, want))
			}
		}
		if err := img.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}

	if _, err := Open(t.TempDir(), "", "Macmini9,1"); err == nil {
		t.Error("Open on a directory succeeded")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.ipsw"), "", "Macmini9,1"); err == nil {
		t.Error("Open on a missing file succeeded")
	}
}

func TestCacheSharedBetweenReaders(t *testing.T) {
	o, url := serve(t, 1<<20)
	dir := t.TempDir()
	a, err := newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := newRangeReader(url, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	dataSize := func() int64 {
		st, err := os.Stat(filepath.Join(a.dir, "data"))
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}

	// What one reader fetched after the other opened the cache is a hit for the other.
	read(t, a, o, 6000, 100)
	o.ranges = nil
	read(t, b, o, 6000, 100)
	if len(o.ranges) != 0 || dataSize() != 100 {
		t.Fatalf("shared hit: ranges %v, data size %d, want none and 100", o.ranges, dataSize())
	}

	// A put overlapping bytes another reader already stored appends only the missing parts.
	read(t, a, o, 7000, 100)
	if err := b.disk.put(6900, o.data[6900:7200]); err != nil {
		t.Fatal(err)
	}
	if got := dataSize(); got != 400 {
		t.Fatalf("data size after overlapping put = %d, want 400", got)
	}
	o.ranges = nil
	read(t, a, o, 6900, 300)
	if len(o.ranges) != 0 {
		t.Fatalf("expected hits for the other reader's put, got ranges %v", o.ranges)
	}
}
