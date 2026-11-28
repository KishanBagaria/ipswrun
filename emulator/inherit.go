package emulator

// What a copy of the process has of a stretch of memory (minherit): a copy of it, which is how
// memory that was mapped for oneself is; the very memory, as what was mapped to be shared is;
// or nothing. As measured on macOS 27.2.1 (tests/kernel/memory/inheritmem.c).

const inheritShare, inheritCopy, inheritNone = 0, 1, 2

// inheritance is how a copy of the process has a region: as was said of it, or as its kind is.
func (r *region) inheritance() uint32 {
	switch {
	case r.inherits != 0:
		return uint32(r.inherits - 1)
	case r.shared != nil:
		return inheritShare
	}
	return inheritCopy
}

// sysMinherit is minherit(address, length, how): the whole pages the bytes are in are had so by
// a copy of the process from then on. A way there is not is EINVAL; no length, and pages that
// are not there, are no error. Memory that is to be shared and was not is made one memory of
// the machine's own, holding what it held, so that a copy has the same and its copies too.
func sysMinherit(p *Process, a []uint64) int64 {
	how := uint32(a[2])
	if how > inheritNone {
		return -eINVAL
	}
	start, end := a[0]&tbiMask&^(pageSize-1), align(a[0]&tbiMask+a[1], pageSize)
	if a[1] == 0 || end <= start {
		return 0
	}
	p.split(start)
	p.split(end)
	for _, r := range p.overlaps(start, end) {
		if how == inheritShare && r.shared == nil {
			if errno := p.shareRegion(r); errno != 0 {
				return errno
			}
		}
		r.inherits = uint8(how + 1)
	}
	return 0
}

// shareRegion makes a region of the process's own one memory of the machine's, with what it holds.
func (p *Process) shareRegion(r *region) int64 {
	size := r.end - r.start
	held := p.read(r.start, size)
	s := p.m.newShared(size)
	if s == nil {
		return -eNOMEM
	}
	copy(s.data, held)
	for at := r.start; at < r.end; at += pageSize {
		if p.isMapped(at) {
			p.uc.memUnmap(at, pageSize)
			delete(p.mapped, at/pageSize)
		}
	}
	p.uc.forgetTranslations()
	r.kind, r.shared, r.off, r.data, r.file = sharedRegion, s.retain(), 0, nil, nil
	return 0
}

// inherited is the region a copy of the process has for one of the process's: none, the same,
// or one of its own that starts with what the shared memory holds now.
func (r *region) inherited() *region {
	copied := *r
	switch r.inheritance() {
	case inheritNone:
		return nil
	case inheritCopy:
		if r.shared != nil && r.inherits != 0 { // memory that is shared and is to be copied, as was said of it
			copied.kind, copied.shared, copied.off = fileRegion, nil, 0
			copied.data = append([]byte(nil), r.shared.data[r.off:r.off+(r.end-r.start)]...)
		}
	}
	return &copied
}
