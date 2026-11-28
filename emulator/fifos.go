package emulator

import "time"

// A named pipe (mkfifo) opened: a pipe between whoever opens the name, as the kernel's source
// has it (bsd/miscfs/fifofs/fifo_vnops.c) and as measured on macOS 27.2.1 (tests/kernel/io/fifo.c). An
// open to read waits for one to write and an open to write for one to read, unless it is told
// not to wait: then reading opens at once, and writing with nobody reading is ENXIO. An open
// to do both waits for nobody. What is written is read by any reader; with no writer left a
// read is the end, and with no reader a write is EPIPE; with nobody at either end what was in
// it is gone. The name is a file of the host's that the machine knows to be a pipe (see
// sysMkfifo), and can be removed while the pipe is open.

const (
	fifoSize = 8192 // what one holds: from the source (PIPSIZ), not measured
	eNXIO    = 6
)

// openFifo opens the named pipe a file names, for reading, writing or both.
func (p *Process) openFifo(id hostFile, mode uint16, flags uint64, path string) int64 {
	const writeOnly, readWrite, nonblock = 1, 2, 0x4
	if p.full() {
		return -eMFILE
	}
	switch {
	case flags&0xA00 == 0xA00: // to be made, and only if it is not there, as measured
		return -eEXIST
	case flags&0x100000 != 0: // O_DIRECTORY: from the source
		return -eNOTDIR
	}
	q := p.m.fifoPipes[id]
	if q == nil {
		p.m.pipes++
		now := p.m.clock.now()
		q = &pipe{id: p.m.pipes, size: fifoSize, made: now, written: now, fifo: true, named: id, mode: mode, path: path}
		p.m.fifoPipes[id] = q
	}
	f := &fd{kind: pipeFD, pipe: q, refs: 1, flags: uint32(flags) & flagNonblock}
	switch flags & 3 {
	case writeOnly:
		if q.readers == 0 && flags&nonblock != 0 {
			if q.writers == 0 {
				delete(p.m.fifoPipes, id)
			}
			return -eNXIO
		}
		f.flags |= flagWriteOnly
		q.writers++
	case readWrite:
		f.both = true
		q.readers++
		q.writers++
	default:
		q.readers++
	}
	if f.writes() {
		q.ended = false
	}
	n := p.newFD(f)
	if flags&0x1000000 != 0 { // O_CLOEXEC
		p.cloexec[n] = true
	}
	p.pipeChanged(q) // whoever waited to open the other end has someone now
	op := &pipeOp{f: f, opening: true, number: n}
	if _, ok := p.tryOpenFifo(op); ok || flags&nonblock != 0 {
		return int64(n)
	}
	p.current.pipeOp = op
	p.wait(waitPipe, q.id, time.Time{}, result{})
	return 0
}

// tryOpenFifo says whether an open that waits for the other end has it: the descriptor then.
func (p *Process) tryOpenFifo(op *pipeOp) (int64, bool) {
	q, f := op.f.pipe, op.f
	if f.both || f.writing() && q.readers > 0 || !f.writing() && q.writers > 0 {
		return int64(op.number), true
	}
	return 0, false
}
