package emulator

import "github.com/KishanBagaria/ipswrun/dyldcache"

// The kernel's ring for memory to be given back later (deferred reclamation), which the malloc
// of macOS 26 and later keeps: a process asks for one, writes into it the stretches of memory it
// has no use for and may want again, and the kernel takes them when it is asked to, or when it
// is short. Here memory is never short, so they are taken only when asked: by a flush, and
// before the ring is given another length.
//
// The ring is a header and then entries of 16 bytes: an address, a length, and what is to be
// done with the memory (2: it is unmapped, by whole pages; 1: it is left mapped as it is, to be
// used again). The kernel reads where the entries start and end from the header and writes
// back how far it has got (head and busy), and nothing else: as measured on macOS 27.2.1 of the
// kernel's own requests, apart from the library's (the probe is not a guest: a Mac's malloc has
// the process's one ring already, and asks for it before a guest's first line).
//
// The header is not the same in every release: 112 bytes in the source of macOS 26, with head,
// busy and tail at 80, 88 and 96, and 128 bytes as measured on macOS 27.2.1, with them 8 bytes
// on. Which one a guest's library has is told by what it asks for: it rounds the ring's length
// so that header and entries fill whole pages. The user allowed this one request to be
// answered by the release (2026-11-28).

type reclaimRing struct {
	addr, size uint64 // the mapping, which is as large as the most entries asked for need
	header     uint64 // 112 or 128
	length     uint64 // how many entries it is taken to have
	sampled    uint64 // when the kernel last looked at how much is in it, in Mach time; none yet at first
}

// indices is where in the header head, busy and tail are, one after the other: at 80 in the
// source of macOS 26, and at 88 as measured on macOS 27.2.1.
func (r *reclaimRing) indices() uint64 {
	if r.header == 128 {
		return 88
	}
	return 80
}

const (
	reclaimInvalidArgument  = 0x20004001 // VM_RECLAIM_INVALID_ARGUMENT
	reclaimNotSupported     = 0x20004002
	reclaimResourceShortage = 0x20004006
	reclaimEntry            = 16
	reclaimChunk            = 16
	reclaimPeriod           = 10 * 24_000_000 // ten seconds of Mach time, as the source has it and as the header's two times are apart on macOS 27.2.1
	kernInvalidTask         = 16
	kernAborted             = 14
)

// migReclaimAllocate is mach_vm_deferred_reclamation_buffer_allocate (4822): the ring, and a
// second value. In the order of the source: lengths of none, or more entries than the most, are
// KERN_INVALID_ARGUMENT; an iPhone's kernel has it turned off (from the source: not measured);
// more than 128 MB is KERN_NO_SPACE; a second ring is refused. All but the iPhone's as measured.
//
// The second value is, in the source of macOS 26, how long to leave between two reports to the
// kernel, and is written only where the request fails: a ring that is given comes with nothing
// there. On macOS 27.2.1 it was measured to be a time, and the header there holds a time ten
// seconds after the last report: it is taken to be when the next report is due, ten seconds
// from now, which was not measured of the kernel's answer itself.
func migReclaimAllocate(p *Process, body []byte) migReply {
	length, most := uint64(le.Uint32(body[32:])), uint64(le.Uint32(body[36:]))
	switch {
	case length == 0 || most == 0 || most < length:
		return migCode(kernInvalidArg)
	case p.cache.Platform == dyldcache.IOS:
		return migCode(reclaimNotSupported)
	}
	var header uint64
	switch {
	case (128+reclaimEntry*length)%pageSize == 0:
		header = 128
	case (112+reclaimEntry*length)%pageSize == 0:
		header = 112
	default:
		return migReply{none: true} // a length that neither header rounds to: which the library has is not known
	}
	size := align(header+reclaimEntry*most, pageSize)
	switch {
	case size > 128<<20:
		return migCode(kernNoSpace)
	case p.ring != nil:
		return migCode(reclaimResourceShortage)
	}
	addr, ok := p.alloc(size, allocation{name: "reclaim ring"})
	if !ok {
		return migCode(kernNoSpace)
	}
	p.ring = &reclaimRing{addr: addr, size: size, header: header, length: length}
	var second uint64
	if header == 128 {
		second = p.m.clock.ticks() + reclaimPeriod
	}
	return migData(le64(addr, second))
}

// reclaim takes up to so many entries from the ring, oldest first, and says how many bytes they
// were (as written, not by pages) and a code if it could not go on. An entry of no address or
// no length is passed over and counted.
func (p *Process) reclaim(want uint64) (bytes uint64, code uint32, stop bool) {
	r := p.ring
	at := r.addr + r.indices()
	for want > 0 {
		head, busy, tail := le.Uint64(p.read(at, 8)), le.Uint64(p.read(at+8, 8)), le.Uint64(p.read(at+16, 8))
		if busy < head || busy-head > reclaimChunk || tail < head {
			return bytes, 0, true // the kernel ends a process whose ring says so, or tries again: neither was measured
		}
		n := min(tail-head, want, reclaimChunk)
		if n == 0 {
			break
		}
		p.write(at+8, le64(head+n))
		for i := range n {
			entry := p.read(r.addr+r.header+reclaimEntry*((head+i)%r.length), reclaimEntry)
			addr, size, how := le.Uint64(entry), uint64(le.Uint32(entry[8:])), entry[12]
			if addr == 0 || size == 0 {
				continue
			}
			switch how {
			case 1: // to be used again: it stays as it is, as measured where nothing is short
			case 2:
				start, end := addr&^(pageSize-1), align(addr+size, pageSize)
				if !p.mappedAll(start, end) {
					return bytes, 0, true // a gap in what is to be unmapped: the kernel raises a guard exception, which was not measured
				}
				p.unmap(start, end)
				p.m.deferred = append(p.m.deferred, p.m.settleMapped) // once the engine no longer maps it
			default:
				return bytes, 0, true
			}
			bytes += size
		}
		p.write(at, le64(head+n, head+n))
		want -= n
	}
	return bytes, 0, false
}

// mappedAll says whether every page of a stretch is some region's.
func (p *Process) mappedAll(start, end uint64) bool {
	for _, r := range p.overlaps(start, end) {
		if r.start > start {
			return false
		}
		start = r.end
	}
	return start >= end
}

// migReclaimFlush is mach_vm_deferred_reclamation_buffer_flush (4823): so many entries are
// taken, and how many bytes they were is said. Of none it is nothing, and of more than there
// are it is those there are, as measured; with no ring it is KERN_INVALID_ARGUMENT (the source).
func migReclaimFlush(p *Process, body []byte) migReply {
	if p.ring == nil {
		return migCode(kernInvalidArg)
	}
	bytes, code, stop := p.reclaim(uint64(le.Uint32(body[32:])))
	switch {
	case stop:
		return migReply{none: true}
	case code != 0:
		return migCode(code)
	}
	return p.reclaimed(bytes)
}

// reclaimed is the answer of a flush or of a new length: how many bytes were taken, and with
// the header of macOS 27.2.1 a second value, ten seconds from now, as measured there of both
// (it is not the time the accounting trap answers with, which the kernel keeps apart).
func (p *Process) reclaimed(bytes uint64) migReply {
	if p.ring.header == 128 {
		return migData(le64(bytes, p.m.clock.ticks()+reclaimPeriod))
	}
	return migData(le64(bytes))
}

// migReclaimResize is mach_vm_deferred_reclamation_buffer_resize (4826): everything in the ring
// is taken, and it is then taken to have the new length, which has to fit what was mapped
// (KERN_NO_SPACE); a length of none is KERN_INVALID_ARGUMENT. As measured. With no ring it is
// KERN_INVALID_TASK (the source).
func migReclaimResize(p *Process, body []byte) migReply {
	length := uint64(le.Uint32(body[32:]))
	switch {
	case length == 0:
		return migCode(kernInvalidArg)
	case p.ring == nil:
		return migCode(kernInvalidTask)
	case align(p.ring.header+reclaimEntry*length, pageSize) > p.ring.size:
		return migCode(kernNoSpace)
	}
	bytes, _, stop := p.reclaim(^uint64(0))
	if stop {
		return migReply{none: true}
	}
	p.ring.length = length
	return p.reclaimed(bytes)
}

// migReclaimQuery is mach_vm_deferred_reclamation_buffer_query (4828): where the ring is and
// how large its mapping, or nothing of both, as measured.
func migReclaimQuery(p *Process, body []byte) migReply {
	if p.ring == nil {
		return migData(le64(0, 0))
	}
	return migData(le64(p.ring.addr, p.ring.size))
}

// machReclaimAccounting is mach_vm_reclaim_update_kernel_accounting_trap (Mach trap 63), by
// which the library tells the kernel that what the ring holds has changed: the kernel looks
// again if ten seconds have gone since it last did, noting how much is in the ring, and says
// how many bytes it took meanwhile, which here is none, memory never being short. Of a name
// that is not the task's own it is MACH_SEND_INVALID_DEST and with nowhere to answer
// KERN_INVALID_ARGUMENT, as measured; with no ring KERN_NOT_FOUND (the source).
//
// With the header of macOS 27.2.1 it has a second place to answer, for when it will next look:
// ten seconds after it last did, the same until then, as measured. With the header of the
// source of macOS 26 it has the one, as the source has it.
func machReclaimAccounting(p *Process, a []uint64) int64 {
	const kernNotFound = 56
	r := p.ring
	switch {
	case !p.isTask(uint32(a[0])):
		return sendInvalidDest
	case a[1] == 0 || r != nil && r.header == 128 && a[2] == 0:
		return kernInvalidArg
	case r == nil:
		return kernNotFound
	}
	if now := p.m.clock.ticks(); r.sampled == 0 || now >= r.sampled+reclaimPeriod {
		r.sampled = now
		counts := r.addr + r.indices() - 16 // how much is in it, and the least there was, before head, busy and tail
		p.write(counts+8, p.read(counts, 8))
	}
	p.write(a[1], le64(0))
	if r.header == 128 {
		p.write(a[2], le64(r.sampled+reclaimPeriod))
	}
	return 0
}
