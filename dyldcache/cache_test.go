package dyldcache

import (
	"bytes"
	"strings"
	"testing"
)

const (
	testBase  = 0x180000000
	dataOff   = 0x4000 // where a test file's mapped pages start
	tableOff  = 0x200  // mapping table, inside the header block
	textsOff  = 0x300  // image text table
	stringOff = 0x380
)

// blob is a cache file held in memory.
type blob []byte

func (b blob) Size() int64 { return int64(len(b)) }

func (b blob) Pread(off, n int64) ([]byte, error) {
	if off >= int64(len(b)) {
		return nil, nil
	}
	return bytes.Clone(b[off:min(off+n, int64(len(b)))]), nil
}

type testMapping struct {
	address   uint64
	pages     []byte
	slideInfo []byte
}

// cacheFile lays out a cache file: a header block with the mapping table, then each mapping's
// pages followed by its slide info.
func cacheFile(uuid byte, headerSize uint32, mappings ...testMapping) blob {
	b := make([]byte, dataOff)
	copy(b, "dyld_v1  arm64e")
	le.PutUint32(b[0x10:], headerSize)
	for i := 0x58; i < 0x68; i++ {
		b[i] = uuid
	}
	le.PutUint32(b[0x138:], tableOff)
	le.PutUint32(b[0x13C:], uint32(len(mappings)))
	for i, m := range mappings {
		e := b[tableOff+i*56:]
		le.PutUint64(e, m.address)
		le.PutUint64(e[8:], uint64(len(m.pages)))
		le.PutUint64(e[16:], uint64(len(b)))
		b = append(b, m.pages...)
		if len(m.slideInfo) > 0 {
			le.PutUint64(b[tableOff+i*56+24:], uint64(len(b)))
			le.PutUint64(b[tableOff+i*56+32:], uint64(len(m.slideInfo)))
			b = append(b, m.slideInfo...)
		}
	}
	return b
}

func open(t *testing.T, files map[string]blob) *Cache {
	t.Helper()
	c, err := Open(func(suffix string) (File, error) { return files[suffix], nil })
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pages returns n pages of a repeating pattern, so a byte a fixup should not have touched is
// recognizable.
func pages(n int) []byte {
	p := make([]byte, n*PageSize)
	for i := range p {
		p[i] = byte(i*7 + 1)
	}
	return p
}

func u16s(values ...uint16) []byte {
	b := make([]byte, 2*len(values))
	for i, v := range values {
		le.PutUint16(b[2*i:], v)
	}
	return b
}

// chained is the slide info of formats 3 and 5, which share a header.
func chained(version uint32, add uint64, starts ...uint16) []byte {
	b := make([]byte, 24)
	le.PutUint32(b, version)
	le.PutUint32(b[4:], PageSize)
	le.PutUint32(b[8:], uint32(len(starts)))
	le.PutUint64(b[16:], add)
	return append(b, u16s(starts...)...)
}

type word struct {
	off      int
	raw, out uint64 // the pointer as stored, and rebased with no slide
}

// checkFixups writes the stored pointers into the mapping's pages, reads them back through
// Cache.Page with and without a slide, and expects exactly those pointers to have changed.
func checkFixups(t *testing.T, slideInfo []byte, words []word, slidNull bool) {
	t.Helper()
	original := pages(3)
	for _, w := range words {
		le.PutUint64(original[w.off:], w.raw)
	}
	c := open(t, map[string]blob{"": cacheFile(1, 0x188, testMapping{testBase, original, slideInfo})})
	for _, slide := range []uint64{0, 0x1238000} {
		want := bytes.Clone(original)
		for _, w := range words {
			out := w.out
			if out != 0 || slidNull {
				out += slide
			}
			le.PutUint64(want[w.off:], out)
		}
		got, err := c.Page(testBase, 3*PageSize, slide)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range words {
			if g, e := le.Uint64(got[w.off:]), le.Uint64(want[w.off:]); g != e {
				t.Errorf("slide %#x: pointer at %#x = %#x, want %#x", slide, w.off, g, e)
			}
		}
		if !bytes.Equal(got, want) {
			t.Errorf("slide %#x: bytes outside the fixup chains changed", slide)
		}
		// A page asked for on its own is rebased with its own chain, not the first page's.
		last, err := c.Page(testBase+2*PageSize, PageSize, slide)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(last, want[2*PageSize:]) {
			t.Errorf("slide %#x: the third page read alone differs from the same page read in a run", slide)
		}
	}
}

func TestSlideInfoV3(t *testing.T) {
	const add = testBase
	// A plain pointer keeps 43 bits of target and 8 bits (43–50) that become its top byte; an
	// authenticated one (bit 63) is a 32-bit offset from add, under diversity and key bits that
	// are dropped. Bits 51–61 are the distance to the next pointer in 8-byte units.
	checkFixups(t, chained(3, add, 0x10, 0xFFFF, 0), []word{
		{0x10, 0x180001234 | 0xAB<<43 | 2<<51, 0xAB00000180001234},
		{0x20, 1<<63 | 0x5678 | 0x12345<<32 | 3<<51, add + 0x5678},
		{0x38, 0x180002000, 0x180002000},
		{2 * PageSize, 1<<63 | 0x10 | 0x7FE<<51, add + 0x10},
		{2*PageSize + 0x7FE*8, 0x180003000, 0x180003000},
	}, true)
}

func TestSlideInfoV5(t *testing.T) {
	const add = testBase
	// Every pointer is a 34-bit offset from add. A plain one carries its top byte in bits 34–41;
	// an authenticated one (bit 63) has diversity and key bits there. Bits 52–62 are the distance.
	checkFixups(t, chained(5, add, 0x10, 0xFFFF, 0), []word{
		{0x10, 0x1234 | 0xCD<<34 | 2<<52, 0xCD00000000000000 | (add + 0x1234)},
		{0x20, 1<<63 | 0x5678 | 0x2ABCD<<34 | 3<<52, add + 0x5678},
		{0x38, 0x3FFFFFFF8, add + 0x3FFFFFFF8},
		{2 * PageSize, 1<<63 | 0x10 | 0x7FE<<52, add + 0x10},
		{2*PageSize + 0x7FE*8, 0x3000, add + 0x3000},
	}, true)
}

func TestSlideInfoV2(t *testing.T) {
	const add, mask = testBase, 0x00FFFF0000000000
	info := make([]byte, 40)
	le.PutUint32(info, 2)
	le.PutUint32(info[4:], PageSize)
	le.PutUint32(info[8:], 40) // page starts
	le.PutUint32(info[12:], 3)
	le.PutUint32(info[16:], 46) // extras
	le.PutUint32(info[20:], 2)
	le.PutUint64(info[24:], mask)
	le.PutUint64(info[32:], add)
	// Page 0 has one chain from 0x10; page 1 has no rebasing; page 2 has two chains, listed in the
	// extras table, from 0x10 and 0x400. Starts are in 4-byte units.
	info = append(info, u16s(4, 0x4000, 0x8000|0)...)
	info = append(info, u16s(4, 0x8000|0x100)...)
	// The masked bits are the distance to the next pointer in 4-byte units; the rest is the
	// target, relative to add. A null stays null whatever the slide.
	checkFixups(t, info, []word{
		{0x10, 0x1000 | 3<<40, add + 0x1000},
		{0x1C, 0 | 3<<40, 0},
		{0x28, 0xFF00000000002000, 0xFF00000000002000 + add},
		{2*PageSize + 0x10, 0x3000, add + 0x3000},
		{2*PageSize + 0x400, 0x4000, add + 0x4000},
	}, false)
}

func TestPagesWithoutSlideInfoAreVerbatim(t *testing.T) {
	text := pages(2)
	c := open(t, map[string]blob{"": cacheFile(1, 0x188, testMapping{address: testBase, pages: text})})
	got, err := c.Page(testBase+PageSize, PageSize, 0x4000)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, text[PageSize:]) {
		t.Error("a page of a mapping with no slide info was changed")
	}
}

func TestPagePastEndOfFileIsZeroFilled(t *testing.T) {
	file := cacheFile(1, 0x188, testMapping{address: testBase, pages: pages(2)})
	file = file[:len(file)-PageSize-0x100] // the mapping still claims two pages
	c := open(t, map[string]blob{"": file})
	got, err := c.Page(testBase, 2*PageSize, 0)
	if err != nil {
		t.Fatal(err)
	}
	kept := PageSize - 0x100
	if len(got) != 2*PageSize || !bytes.Equal(got[:kept], pages(2)[:kept]) || !bytes.Equal(got[kept:], make([]byte, 2*PageSize-kept)) {
		t.Error("want the file's bytes followed by zeros up to the requested size")
	}
}

func TestPageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		slideInfo []byte
		addr      uint64
		want      string
	}{
		"outside":   {nil, testBase + 3*PageSize, "outside cache"},
		"version 1": {chained(1, 0, 0), testBase, "unsupported slide info v1"},
		"version 4": {chained(4, 0, 0), testBase, "unsupported slide info v4"},
		"short":     {chained(3, 0, 0)[:23], testBase, "truncated slide info"},
		"starts":    {chained(3, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0)[:40], testBase, "truncated slide info"},
		"page size": {func() []byte { b := chained(3, 0, 0); le.PutUint32(b[4:], 0); return b }(), testBase, "page size"},
	} {
		c := open(t, map[string]blob{"": cacheFile(1, 0x188, testMapping{testBase, pages(3), tc.slideInfo})})
		if _, err := c.Page(tc.addr, PageSize, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want one mentioning %q", name, err, tc.want)
		}
	}
}

// withSubcaches adds a subcache table to a main file: 24-byte entries (a UUID; the files are
// numbered) up to header size 0x1C8, 56-byte entries carrying the file suffix after it.
func withSubcaches(main blob, headerSize uint32, uuids []byte, suffixes []string) blob {
	le.PutUint32(main[0x10:], headerSize)
	le.PutUint32(main[0x188:], uint32(len(main)))
	le.PutUint32(main[0x18C:], uint32(len(uuids)))
	for i, u := range uuids {
		entry := make([]byte, 24)
		for j := 0; j < 16; j++ {
			entry[j] = u
		}
		if suffixes != nil {
			entry = append(entry, make([]byte, 32)...)
			copy(entry[24:], suffixes[i])
		}
		main = append(main, entry...)
	}
	return main
}

func TestOpenSubcaches(t *testing.T) {
	high := testMapping{address: testBase + 0x100000, pages: pages(1)}
	mid := testMapping{address: testBase + 0x40000, pages: pages(1)}
	low := testMapping{address: testBase, pages: pages(1)}
	for name, tc := range map[string]struct {
		headerSize uint32
		suffixes   []string
		files      [2]string
	}{
		"numbered": {0x1C8, nil, [2]string{".1", ".2"}},
		"named":    {0x1D0, []string{".01", ".dylddata"}, [2]string{".01", ".dylddata"}},
	} {
		// The main file's mapping is not the lowest, and the table lists the subcaches out of
		// address order.
		main := withSubcaches(cacheFile(1, 0, mid), tc.headerSize, []byte{2, 3}, tc.suffixes)
		files := map[string]blob{"": main, tc.files[0]: cacheFile(2, 0, high), tc.files[1]: cacheFile(3, 0, low)}
		c := open(t, files)
		if c.Base != testBase || len(c.Mappings) != 3 || c.Mappings[1].Address != mid.address || c.Mappings[2].Address != high.address {
			t.Errorf("%s: mappings are not sorted by address from base %#x", name, c.Base)
		}
		if c.UUID != [16]byte{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1} {
			t.Errorf("%s: UUID %x is not the main file's", name, c.UUID)
		}
		if len(c.Files) != 3 || c.Files[tc.files[0]] == nil || c.Files[tc.files[1]] == nil {
			t.Errorf("%s: files %v", name, c.Files)
		}
		if m := c.Mapping(high.address + PageSize - 1); m == nil || m.Address != high.address {
			t.Errorf("%s: the last byte of a subcache mapping was not found", name)
		}
		if c.Mapping(high.address+PageSize) != nil || c.Mapping(testBase-1) != nil || c.Mapping(testBase+PageSize) != nil {
			t.Errorf("%s: an address between mappings was found", name)
		}

		files[tc.files[1]] = cacheFile(9, 0, low)
		_, err := Open(func(suffix string) (File, error) { return files[suffix], nil })
		if err == nil || !strings.Contains(err.Error(), "uuid mismatch") {
			t.Errorf("%s: a subcache with another UUID gave %v", name, err)
		}
	}
}

func TestOpenRejects(t *testing.T) {
	good := func() blob { return cacheFile(1, 0x188, testMapping{address: testBase, pages: pages(1)}) }
	for name, spoil := range map[string]func(b blob) blob{
		"arm64 cache":      func(b blob) blob { copy(b, "dyld_v1   arm64\x00"); return b },
		"short header":     func(b blob) blob { return b[:0x100] },
		"no mappings":      func(b blob) blob { le.PutUint32(b[0x13C:], 0); return b },
		"mapping count":    func(b blob) blob { le.PutUint32(b[0x13C:], 65); return b },
		"mapping table":    func(b blob) blob { le.PutUint32(b[0x138:], uint32(len(b)-8)); return b },
		"subcache count":   func(b blob) blob { le.PutUint32(b[0x10:], 0x1C8); le.PutUint32(b[0x18C:], 257); return b },
		"subcache table":   func(b blob) blob { return withSubcaches(b, 0x1C8, []byte{2}, nil)[:len(b)+8] },
		"missing subcache": func(b blob) blob { return withSubcaches(b, 0x1C8, []byte{2}, nil) },
	} {
		file := spoil(good())
		if _, err := Open(func(suffix string) (File, error) {
			if suffix != "" {
				return blob(nil), nil
			}
			return file, nil
		}); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

func TestImage(t *testing.T) {
	main := cacheFile(1, 0x188, testMapping{address: testBase, pages: pages(1)})
	le.PutUint64(main[0x88:], textsOff)
	le.PutUint64(main[0x90:], 2)
	// Out of address order, as the table is when images were added to the cache out of order.
	for i, image := range []struct {
		address uint64
		size    uint32
		path    string
	}{{testBase + 0x9000, 0x3000, "/usr/lib/libB.dylib"}, {testBase + 0x1000, 0x2000, "/usr/lib/libA.dylib"}} {
		e := main[textsOff+i*32:]
		le.PutUint64(e[16:], image.address)
		le.PutUint32(e[24:], image.size)
		le.PutUint32(e[28:], uint32(stringOff+i*0x40))
		copy(main[stringOff+i*0x40:], image.path)
	}
	c := open(t, map[string]blob{"": main})
	for _, tc := range []struct {
		addr   uint64
		path   string
		offset uint64
	}{
		{testBase + 0x1000, "/usr/lib/libA.dylib", 0},
		{testBase + 0x2FFF, "/usr/lib/libA.dylib", 0x1FFF},
		{testBase + 0x3000, "", 0}, // between the two
		{testBase + 0xBFFF, "/usr/lib/libB.dylib", 0x2FFF},
		{testBase + 0xC000, "", 0},
		{testBase, "", 0}, // below the first
	} {
		path, offset, err := c.Image(tc.addr)
		if err != nil || path != tc.path || offset != tc.offset {
			t.Errorf("Image(%#x) = %q+%#x, %v; want %q+%#x", tc.addr, path, offset, err, tc.path, tc.offset)
		}
	}
}

func TestPlatform(t *testing.T) {
	for _, want := range []Platform{MacOS, IOS} {
		main := cacheFile(1, 0x188, testMapping{address: testBase, pages: pages(1)})
		// The platform is the low byte of a bitfield whose other bits say more of the cache.
		le.PutUint32(main[0xD8:], uint32(want)|1<<8)
		if got := open(t, map[string]blob{"": main}).Platform; got != want {
			t.Errorf("platform %v read as %v", want, got)
		}
	}
}
