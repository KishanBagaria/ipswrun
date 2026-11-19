// Package dyldcache reads an arm64e dyld shared cache: its mappings, and pages with the slide
// fixups applied the way the kernel's shared-region pager applies them (pointer-auth keys off).
package dyldcache

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sort"
)

// PageSize is the arm64 macOS page size.
const PageSize = 0x4000

var le = binary.LittleEndian

// File is one file of the cache (the main file or a subcache).
type File interface {
	Size() int64
	// Pread returns up to n bytes at off; fewer only at the end of the file. The bytes are the
	// caller's to change.
	Pread(off, n int64) ([]byte, error)
}

// Mapping is one region of the cache's address space and the file range behind it.
type Mapping struct {
	File      File
	Address   uint64
	Size      uint64
	Offset    uint64
	slideOff  uint64
	slideSize uint64
	slide     *slideInfo
}

type slideInfo struct {
	version  uint32
	pageSize uint32
	add      uint64
	starts   []uint16
	extras   []uint16 // v2
	mask     uint64   // v2
}

// Cache is a dyld shared cache with its subcaches.
type Cache struct {
	Mappings []*Mapping      // sorted by address
	Files    map[string]File // by file name suffix; "" is the main file
	UUID     [16]byte
	Base     uint64

	textsOff, textsCount uint64
	texts                []imageText // sorted by address; read on first use
}

// imageText is the text segment of one image in the cache.
type imageText struct {
	address    uint64
	size, path uint32
}

// Open parses the cache whose files opener returns by suffix ("" for the main file).
func Open(opener func(suffix string) (File, error)) (*Cache, error) {
	main, err := opener("")
	if err != nil {
		return nil, err
	}
	h, err := main.Pread(0, 0x400)
	if err != nil {
		return nil, err
	}
	if len(h) < 0x200 || string(bytes.TrimRight(h[:16], "\x00")) != "dyld_v1  arm64e" {
		return nil, errors.New("not an arm64e dyld cache")
	}
	c := &Cache{Files: map[string]File{"": main}, textsOff: le.Uint64(h[0x88:]), textsCount: le.Uint64(h[0x90:])}
	headerSize := le.Uint32(h[0x10:])
	copy(c.UUID[:], h[0x58:0x68])
	if err := c.add(main, h); err != nil {
		return nil, err
	}
	var subOff, subCount uint32
	if headerSize > 0x188 {
		subOff, subCount = le.Uint32(h[0x188:]), le.Uint32(h[0x18C:])
	}
	if subCount > 256 {
		return nil, errors.New("too many subcaches")
	}
	v1 := headerSize <= 0x1C8
	entrySize := int64(56)
	if v1 {
		entrySize = 24
	}
	var entries []byte
	if subCount > 0 {
		if entries, err = main.Pread(int64(subOff), int64(subCount)*entrySize); err != nil {
			return nil, err
		}
		if int64(len(entries)) != int64(subCount)*entrySize {
			return nil, errors.New("truncated subcache table")
		}
	}
	for i := int64(0); i < int64(subCount); i++ {
		e := entries[i*entrySize : (i+1)*entrySize]
		suffix := fmt.Sprintf(".%d", i+1)
		if !v1 {
			suffix = cstring(e[24:56])
		}
		f, err := opener(suffix)
		if err != nil {
			return nil, err
		}
		sh, err := f.Pread(0, 0x400)
		if err != nil {
			return nil, err
		}
		if len(sh) < 0x200 || !bytes.Equal(sh[0x58:0x68], e[:16]) {
			return nil, fmt.Errorf("subcache uuid mismatch %s", suffix)
		}
		c.Files[suffix] = f
		if err := c.add(f, sh); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(c.Mappings, func(i, j int) bool { return c.Mappings[i].Address < c.Mappings[j].Address })
	c.Base = c.Mappings[0].Address
	return c, nil
}

func cstring(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

func (c *Cache) add(f File, h []byte) error {
	off, n := le.Uint32(h[0x138:]), le.Uint32(h[0x13C:])
	if n == 0 || n > 64 {
		return errors.New("bad mapping count")
	}
	t, err := f.Pread(int64(off), int64(n)*56)
	if err != nil {
		return err
	}
	if len(t) != int(n)*56 {
		return errors.New("truncated mapping table")
	}
	for i := 0; i < int(n); i++ {
		e := t[i*56:]
		c.Mappings = append(c.Mappings, &Mapping{File: f, Address: le.Uint64(e), Size: le.Uint64(e[8:]), Offset: le.Uint64(e[16:]),
			slideOff: le.Uint64(e[24:]), slideSize: le.Uint64(e[32:])})
	}
	return nil
}

// Mapping returns the mapping containing a cache address, or nil.
func (c *Cache) Mapping(addr uint64) *Mapping {
	for _, m := range c.Mappings {
		if m.Address <= addr && addr < m.Address+m.Size {
			return m
		}
	}
	return nil
}

// Image names the image whose text segment holds the unslid address addr, with the offset into
// it; path is empty when no image's text does.
func (c *Cache) Image(addr uint64) (path string, offset uint64, err error) {
	main := c.Files[""]
	if c.texts == nil {
		t, err := main.Pread(int64(c.textsOff), int64(c.textsCount)*32)
		if err != nil {
			return "", 0, err
		}
		c.texts = []imageText{}
		for ; len(t) >= 32; t = t[32:] {
			c.texts = append(c.texts, imageText{le.Uint64(t[16:]), le.Uint32(t[24:]), le.Uint32(t[28:])})
		}
		sort.Slice(c.texts, func(i, j int) bool { return c.texts[i].address < c.texts[j].address })
	}
	i := sort.Search(len(c.texts), func(i int) bool { return c.texts[i].address > addr }) - 1
	if i < 0 || addr >= c.texts[i].address+uint64(c.texts[i].size) {
		return "", 0, nil
	}
	name, err := main.Pread(int64(c.texts[i].path), 200)
	if err != nil {
		return "", 0, err
	}
	return cstring(name), addr - c.texts[i].address, nil
}

func (m *Mapping) slideInfo() (*slideInfo, error) {
	if m.slide != nil || m.slideSize == 0 {
		return m.slide, nil
	}
	d, err := m.File.Pread(int64(m.slideOff), int64(m.slideSize))
	if err != nil {
		return nil, err
	}
	truncated := errors.New("truncated slide info")
	u16s := func(off, n uint32) ([]uint16, error) {
		if uint64(off)+uint64(n)*2 > uint64(len(d)) {
			return nil, truncated
		}
		out := make([]uint16, n)
		for i := range out {
			out[i] = le.Uint16(d[int(off)+i*2:])
		}
		return out, nil
	}
	if len(d) < 8 {
		return nil, truncated
	}
	s := &slideInfo{version: le.Uint32(d), pageSize: le.Uint32(d[4:])}
	// The header is 24 bytes in v3 and v5 and 40 in v2, and the info can end right after the
	// starts it counts: a one-page v5 mapping's is 26 bytes.
	switch s.version {
	case 3, 5:
		if len(d) < 24 {
			return nil, truncated
		}
		s.add = le.Uint64(d[16:])
		if s.starts, err = u16s(24, le.Uint32(d[8:])); err != nil {
			return nil, err
		}
	case 2:
		if len(d) < 40 {
			return nil, truncated
		}
		s.mask, s.add = le.Uint64(d[24:]), le.Uint64(d[32:])
		if s.starts, err = u16s(le.Uint32(d[8:]), le.Uint32(d[12:])); err != nil {
			return nil, err
		}
		if s.extras, err = u16s(le.Uint32(d[16:]), le.Uint32(d[20:])); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported slide info v%d", s.version)
	}
	if s.pageSize == 0 {
		return nil, errors.New("bad slide info page size")
	}
	m.slide = s
	return s, nil
}

// Page returns the rebased bytes for the unslid range [addr, addr+n), which must lie inside one
// mapping and start on a page boundary; slide is added to every rebased pointer.
func (c *Cache) Page(addr, n, slide uint64) ([]byte, error) {
	m := c.Mapping(addr)
	if m == nil {
		return nil, fmt.Errorf("address outside cache: %#x", addr)
	}
	data, err := m.File.Pread(int64(m.Offset+addr-m.Address), int64(n))
	if err != nil {
		return nil, err
	}
	if uint64(len(data)) < n { // the end of the file: the rest is zero
		data = append(data, make([]byte, n-uint64(len(data)))...)
	}
	s, err := m.slideInfo()
	if err != nil || s == nil {
		return data, err
	}
	psz := uint64(s.pageSize)
	first := (addr - m.Address) / psz
	for pi := first; pi < first+n/psz; pi++ {
		page := data[(pi-first)*psz:]
		if s.version == 2 {
			s.fixV2(page, pi, s.add+slide)
			continue
		}
		if pi >= uint64(len(s.starts)) || s.starts[pi] == 0xFFFF {
			continue
		}
		for off := uint64(s.starts[pi]); ; {
			v := le.Uint64(page[off:])
			var t, delta uint64
			if s.version == 3 {
				delta = (v >> 51) & 0x7FF
				if v>>63 != 0 {
					t = (v & 0xFFFFFFFF) + s.add
				} else {
					t = ((v & 0x7F80000000000) << 13) | (v & 0x7FFFFFFFFFF)
				}
			} else {
				delta = (v >> 52) & 0x7FF
				t = (v & 0x3FFFFFFFF) + s.add
				if v>>63 == 0 {
					t |= ((v >> 34) & 0xFF) << 56
				}
			}
			le.PutUint64(page[off:], t+slide)
			if delta == 0 {
				break
			}
			off += delta * 8
		}
	}
	return data, nil
}

func (s *slideInfo) fixV2(page []byte, pi, add uint64) {
	if pi >= uint64(len(s.starts)) {
		return
	}
	shift := uint(bits.TrailingZeros64(s.mask)) - 2
	chain := func(off uint64) {
		for {
			v := le.Uint64(page[off:])
			delta := (v & s.mask) >> shift
			t := v &^ s.mask
			if t != 0 {
				t += add
			}
			le.PutUint64(page[off:], t)
			if delta == 0 {
				break
			}
			off += delta
		}
	}
	e := s.starts[pi]
	switch {
	case e == 0x4000:
	case e&0x8000 != 0:
		for i := int(e & 0x3FFF); ; i++ {
			x := s.extras[i]
			chain(uint64(x&0x3FFF) * 4)
			if x&0x8000 != 0 {
				break
			}
		}
	default:
		chain(uint64(e) * 4)
	}
}
