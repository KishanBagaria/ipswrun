package emulator

import (
	"cmp"
	"maps"
	"slices"
	"syscall"
	"time"
)

// A pipe is bytes in the kernel between two descriptors of one process: what is written to one
// end waits to be read from the other. A reader with nothing to read waits for a writer, and a
// writer with no room waits for a reader, each a thread off the engine until the other end has
// done its part or has gone. What a Mac's kernel does was measured with examples/pipes.c.

const (
	pipeSize    = 16384 // what a pipe holds to begin with (PIPE_SIZE), and
	pipeSizeBig = 65536 // once more than that has been asked of it
	pipeBuf     = 512   // PIPE_BUF: a write no larger goes in whole, or not at all

	modePipe = syscall.S_IFIFO | 0o660

	sigPipe   = 13
	sigIgnore = 1 // SIG_IGN, as a handler

	// File status flags, as fcntl reads and sets them.
	flagWriteOnly = 0x1
	flagNonblock  = 0x4
	flagAppend    = 0x8
	flagAsync     = 0x40
	flagWritten   = 0x10000 // it has been written through

	ioctlAvailable = 0x4004667F // FIONREAD
	ioctlNonblock  = 0x8004667E // FIONBIO
)

type pipe struct {
	id               uint64
	data             []byte
	size             int
	readers, writers int       // descriptors open at each end, a duplicate counting once
	made, written    time.Time // when; the first until it is written to
	read             time.Time // when it was last read; zero until then
	changes          uint64    // how often something has happened to it: written, read, or an end closed
}

// pipeOp is what a thread that waits on a pipe is in the middle of.
type pipeOp struct {
	f    *fd
	into []stretch // of a read: where it goes, and
	room uint64    // how much there is room for in all
	data []byte    // of a write: what is still to go in, and
	done int64     // how much has
	// Of a socket's (see sockets.go): that it sends, and how it was asked to.
	sending bool
	flags   uint64
}

// stretch is a piece of the guest's memory.
type stretch struct{ addr, size uint64 }

// put writes what was read to where a read wants it, after what it has been given already.
func (p *Process) put(op *pipeOp, data []byte) {
	skip := uint64(op.done)
	for _, s := range op.into {
		if skip >= s.size {
			skip -= s.size
			continue
		}
		n := min(s.size-skip, uint64(len(data)))
		p.write(s.addr+skip, data[:n])
		data, skip = data[n:], 0
		if len(data) == 0 {
			return
		}
	}
}

// streams says whether a descriptor is an end of a pipe or a socket: what is read and written
// in order, and waited for.
func (f *fd) streams() bool { return f.kind == pipeFD || f.kind == socketFD }

// selecting is a select that waits: the descriptors it asked about, and where to say which are
// ready.
type selecting struct {
	sets  [3]uint64 // to read, to write, and with something out of the ordinary; zero for none
	asked [3][]byte
}

// sysPipe makes a pipe, and answers with its two descriptors in the two result registers.
func sysPipe(p *Process, a []uint64) int64 {
	if p.allocFD(p.allocFD(0)+1) >= p.fdLimit() {
		return -eMFILE
	}
	p.m.pipes++
	now := p.m.clock.now()
	q := &pipe{id: p.m.pipes, size: pipeSize, readers: 1, writers: 1, made: now, written: now}
	r := p.newFD(&fd{kind: pipeFD, pipe: q, refs: 1})
	w := p.newFD(&fd{kind: pipeFD, pipe: q, flags: flagWriteOnly, refs: 1})
	p.setX(1, w)
	return int64(r)
}

func (f *fd) writing() bool { return f.flags&flagWriteOnly != 0 }

// pipeClosed is the last descriptor of one end of a pipe closing, which those who wait on the
// other are told.
func (p *Process) pipeClosed(f *fd) {
	if f.refs--; f.refs > 0 {
		return // a duplicate is still open, or another process's
	}
	if f.writing() {
		f.pipe.writers--
	} else {
		f.pipe.readers--
	}
	p.pipeChanged(f.pipe)
}

// pipeRead reads from a pipe: what is there, up to what was asked for. With nothing there it is
// the end if no writer is left, and otherwise a wait, or for a descriptor that does not wait an
// error.
func (p *Process) pipeRead(f *fd, into []stretch) int64 {
	if f.writing() {
		return -eBADF
	}
	op := &pipeOp{f: f, into: into}
	for _, s := range into {
		op.room += s.size
	}
	if op.room == 0 {
		return 0
	}
	if r, ok := p.tryRead(op); ok {
		return r
	}
	if f.flags&flagNonblock != 0 {
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.pipe.id, time.Time{}, result{})
	return 0
}

func (p *Process) tryRead(op *pipeOp) (int64, bool) {
	q := op.f.pipe
	n := min(uint64(len(q.data)), op.room)
	switch {
	case n > 0:
		p.put(op, q.data[:n])
		q.data = q.data[n:]
		q.read = p.m.clock.now()
		p.pipeChanged(q)
		return int64(n), true
	case q.writers == 0:
		return 0, true
	}
	return 0, false
}

// pipeWrite writes to a pipe: all of it, waiting for room as often as it takes, or for a
// descriptor that does not wait as much as there is room for. With no reader left it is an
// error, and a signal.
func (p *Process) pipeWrite(f *fd, data []byte) int64 {
	if !f.writing() {
		return -eBADF
	}
	op := &pipeOp{f: f, data: data}
	if r, ok := p.tryWrite(op); ok {
		return r
	}
	if f.flags&flagNonblock != 0 {
		if op.done > 0 {
			return op.done
		}
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.pipe.id, time.Time{}, result{})
	return 0
}

// tryWrite puts in what there is room for, and says whether that is the end of the write.
func (p *Process) tryWrite(op *pipeOp) (int64, bool) {
	q := op.f.pipe
	if q.readers == 0 {
		return p.brokenPipe(op.f, false), true
	}
	if len(op.data) == 0 {
		return op.done, true
	}
	if len(q.data)+len(op.data) > q.size {
		q.size = pipeSizeBig
	}
	n := min(q.size-len(q.data), len(op.data))
	if n == 0 || len(op.data) <= pipeBuf && n < len(op.data) {
		return 0, false
	}
	q.data = append(q.data, op.data[:n]...)
	op.data, op.done = op.data[n:], op.done+int64(n)
	op.f.flags |= flagWritten
	q.written = p.m.clock.now()
	p.pipeChanged(q)
	return op.done, len(op.data) == 0
}

// brokenPipe is a write to a pipe nobody reads: an error, and before it SIGPIPE, unless the
// descriptor asked for none or the process ignores it. The signal ends a process that has not
// said what to do with it. One that has a handler for it, which nothing here would run, is not
// implemented.
func (p *Process) brokenPipe(f *fd, quiet bool) int64 {
	switch handler := p.sigactions[sigPipe].handler; {
	case f.noSigpipe || quiet || handler == sigIgnore:
	default:
		p.sendSignal(pendingSignal{sig: sigPipe, status: sigPipe, pid: p.pid, uid: p.uid, to: p.current})
	}
	return -ePIPE
}

// pipeChanged gives the threads that wait on a pipe what they waited for, if it is there now,
// and the threads in select their answer: of every process, as a pipe's ends may be in two.
func (p *Process) pipeChanged(q *pipe) {
	q.changes++
	p.m.streamChanged(q.id)
}

// streamChanged does that for the pipe or the pair of sockets with a number.
func (m *Machine) streamChanged(id uint64) {
	for _, owner := range m.procs {
		if owner.exited {
			continue
		}
		for _, t := range owner.waiters(waitPipe, id) {
			op := t.pipeOp
			if t.state != waiting || op == nil { // served meanwhile, by what an earlier one's turn set off
				continue
			}
			try := owner.tryRead
			switch {
			case op.f.kind == socketFD && op.sending:
				try = owner.trySend
			case op.f.kind == socketFD:
				try = owner.tryReceive
			case op.f.writing():
				try = owner.tryWrite
			}
			if r, ok := try(op); ok {
				t.pipeOp = nil
				t.answer = &result{x0: uint64(max(r, -r)), carry: r < 0}
				t.wake()
			}
		}
		for _, t := range owner.waiters(waitSelect, 0) {
			if t.state == waiting && t.polling != nil {
				if n, _ := owner.pollAll(t.polling, false); n > 0 {
					t.polling = nil
					t.answer = &result{x0: uint64(n)}
					t.wake()
				}
				continue
			}
			if t.state != waiting || t.selecting == nil {
				continue
			}
			if n := owner.selected(t.selecting); n > 0 {
				t.selecting = nil
				t.answer = &result{x0: uint64(n)}
				t.wake()
			}
		}
	}
}

// pipeReady says whether a descriptor of a pipe can be read, or written, without waiting, as
// select has it. Each end is asked about itself: the writing end has nothing to read until its
// reader has gone, and the reading end can always be written, though a write to it is refused.
func (f *fd) pipeReady(writing bool) bool {
	if f.kind == socketFD {
		return f.sock.ready(writing)
	}
	q := f.pipe
	switch {
	case !writing && !f.writing():
		return len(q.data) > 0 || q.writers == 0
	case !writing:
		return q.readers == 0
	case !f.writing():
		return true
	}
	return q.readers == 0 || q.size-len(q.data) >= pipeBuf
}

// pipeStat is the struct stat64 of either end of a pipe.
func (p *Process) pipeStat(f *fd) []byte {
	q := f.pipe
	b := make([]byte, 144)
	le.PutUint16(b[4:], modePipe)
	// Its number is one the kernel makes up for each end, differently every time.
	le.PutUint64(b[8:], 0x9E3779B97F4A7C15*(2*q.id+uint64(f.flags&flagWriteOnly)))
	le.PutUint32(b[16:], p.uid)
	le.PutUint32(b[20:], p.gid)
	accessed := q.made
	if !f.writing() && !q.read.IsZero() {
		accessed = q.read
	}
	for at, when := range map[int]time.Time{32: accessed, 48: q.written, 64: q.written} {
		le.PutUint64(b[at:], uint64(when.Unix()))
		le.PutUint64(b[at+8:], uint64(when.Nanosecond()))
	}
	le.PutUint64(b[96:], uint64(len(q.data)))
	le.PutUint64(b[104:], uint64((len(q.data)+q.size-1)/q.size))
	le.PutUint32(b[112:], uint32(q.size))
	return b
}

// pipeFcntl is fcntl of a pipe's descriptor, for the commands that are a pipe's own; false for
// any other.
func (p *Process) pipeFcntl(f *fd, cmd, arg uint64) (int64, bool) {
	const getFlags, setFlags, setNoSigpipe, getNoSigpipe = 3, 4, 73, 74
	switch cmd {
	case getFlags:
		return int64(f.flags), true
	case setFlags:
		if arg&flagAsync != 0 {
			p.unimplemented("a pipe that signals when it can be read or written (O_ASYNC)", nil)
			return -eINVAL, true
		}
		const settable = flagNonblock | flagAppend
		f.flags = f.flags&^settable | uint32(arg)&settable
		return 0, true
	case setNoSigpipe:
		f.noSigpipe = arg != 0
		return 0, true
	case getNoSigpipe:
		return int64(truth(f.noSigpipe)), true
	}
	return 0, false
}

// sysIoctl answers what is asked of a pipe: how much there is to read, and whether to wait.
// Nothing else here is a device that takes requests.
func sysIoctl(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if !f.streams() {
		return -eNOTTY
	}
	switch uint32(a[1]) {
	case ioctlAvailable: // of its own end: nothing waits to be read from the end that is written
		var n uint32
		if f.kind == socketFD {
			n = uint32(len(f.sock.data))
		} else if !f.writing() {
			n = uint32(len(f.pipe.data))
		}
		p.write(a[2], le32(n))
		return 0
	case ioctlNonblock:
		f.flags &^= flagNonblock
		if p.u32(a[2]) != 0 {
			f.flags |= flagNonblock
		}
		return 0
	}
	return -eNOTTY
}

// selectPipes is select where it is asked about a pipe or a file: it answers for the pipes,
// which are ready or become ready, and the files, which always are, and waits as long as it is
// told to. Of the other descriptors it is asked about it says what select says here of them all,
// that they are not ready. False if none is a pipe or a file.
func (p *Process) selectPipes(a []uint64, nanoseconds uint64) (int64, bool) {
	s := &selecting{sets: [3]uint64{a[1], a[2], a[3]}}
	size, found := align(a[0], 32)/8, false
	for i, set := range s.sets {
		if set == 0 {
			continue
		}
		s.asked[i] = p.read(set, size)
		for n := range a[0] {
			if f := p.fds[n]; s.asked[i][n/8]>>(n%8)&1 != 0 && f != nil && (f.streams() || f.isFile()) {
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	if n := p.selected(s); n > 0 {
		return n, true
	}
	for _, set := range s.sets { // none, unless one becomes ready
		if set != 0 {
			p.write(set, make([]byte, size))
		}
	}
	var deadline time.Time
	if a[4] != 0 {
		if deadline = p.after(relative(p.u64(a[4]), p.u64(a[4]+8)*nanoseconds)); deadline.IsZero() {
			return 0, true // not to wait at all
		}
	}
	p.current.selecting = s
	p.wait(waitSelect, 0, deadline, result{})
	return 0, true
}

// selected writes which of the descriptors a select asked about are ready, if any is, and
// returns how many.
func (p *Process) selected(s *selecting) int64 {
	var out [3][]byte
	count := int64(0)
	for i, asked := range s.asked {
		out[i] = make([]byte, len(asked))
		for n := range uint64(len(asked) * 8) {
			f := p.fds[n]
			if asked[n/8]>>(n%8)&1 == 0 || f == nil || i == 2 || !(f.streams() && f.pipeReady(i == 1) || f.isFile()) {
				continue
			}
			out[i][n/8] |= 1 << (n % 8)
			count++
		}
	}
	if count > 0 {
		for i, set := range s.sets {
			if set != 0 {
				p.write(set, out[i])
			}
		}
	}
	return count
}

// pipeWatch names a kqueue's interest in reading or in writing an end of a pipe, which is kept
// as the event that asked for it.
type pipeWatch struct {
	filter       int16
	ident, udata uint64
}

// changePipe takes a change to a kqueue's interest in a descriptor; false if it is no pipe's.
func (p *Process) changePipe(q *kqueue, c kevent) bool {
	key := pipeWatch{c.filter, c.ident, c.udata}
	w, ok := q.pipes[key]
	f := p.fds[c.ident]
	switch {
	case c.flags&evDelete != 0 && ok:
		delete(q.pipes, key)
	case ok && c.flags&evAdd == 0 && c.flags&evEnable != 0:
		w.flags &^= evDisable
		q.pipes[key] = w
	case ok && c.flags&evAdd == 0 && c.flags&evDisable != 0:
		w.flags |= evDisable
		q.pipes[key] = w
	case f == nil || !f.streams():
		return ok
	case c.flags&evAdd != 0:
		q.pipes[key] = c
	}
	return true
}

// watchedPipes makes an event of every end of a pipe a kqueue watches that can be read, or
// written: with how much there is to read, or room to write, and whether the other end has gone.
func (p *Process) watchedPipes(q *kqueue) {
	keys := slices.SortedFunc(maps.Keys(q.pipes), func(a, b pipeWatch) int {
		return cmp.Or(cmp.Compare(a.ident, b.ident), cmp.Compare(b.filter, a.filter), cmp.Compare(a.udata, b.udata))
	})
	for _, key := range keys {
		w := q.pipes[key]
		f := p.fds[key.ident]
		if f == nil || !f.streams() { // closed since: what watched it went with it
			delete(q.pipes, key)
			continue
		}
		if w.flags&evDisable != 0 || slices.ContainsFunc(q.ready, func(e kevent) bool { return e.filter == key.filter && e.ident == key.ident && e.udata == key.udata }) {
			continue
		}
		event := w
		event.flags &^= evAdd | evEnable | evVanished // what was asked for, not what happened
		event.fflags = 0
		data, ended, least, applies := f.filtered(key.filter, w)
		if !applies {
			// The end that is not read, or not written: what a Mac's kernel says of it to a
			// kqueue was not measured.
			p.unimplemented("a kqueue watching the wrong end of a pipe", nil)
			delete(q.pipes, key)
			continue
		}
		if event.data = data; ended {
			event.flags |= evEOF
		} else if data < least {
			continue
		}
		q.ready = append(q.ready, event)
		switch {
		case w.flags&evOneshot != 0:
			delete(q.pipes, key)
		case w.flags&evDispatch != 0:
			w.flags |= evDisable
			q.pipes[key] = w
		}
	}
}

// What poll asks of a descriptor, and answers.
const (
	pollIn      = 0x1
	pollPri     = 0x2
	pollOut     = 0x4 // POLLWRNORM is the same
	pollHangup  = 0x10
	pollInvalid = 0x20
	pollRdNorm  = 0x40
	pollRdBand  = 0x80
	pollWrBand  = 0x100

	pollReading = pollIn | pollPri | pollRdNorm | pollRdBand
	pollWriting = pollOut | pollWrBand
)

// isFile says whether a descriptor is of a file, which can always be read and written without
// waiting: one of the host's that is no directory, or one of the build's.
func (f *fd) isFile() bool {
	switch f.kind {
	case hostFD:
		var st syscall.Stat_t
		return syscall.Fstat(f.host, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFREG
	case cacheFD:
		return true
	case dataFD:
		return f.kq == nil
	}
	return false
}

// isDirectory says whether a descriptor is of a directory.
func (f *fd) isDirectory() bool {
	if f.kind == hostFD {
		var st syscall.Stat_t
		return syscall.Fstat(f.host, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR
	}
	return f.kind == dirFD
}

// filtered is what a kqueue's filter for reading or for writing has to say of an end of a pipe
// or of a socket: how much there is to read, or room to write, whether the other end has gone,
// and the least there has to be for that to be an event while it has not. Not applies is the
// end of a pipe that is not the one read, or written.
func (f *fd) filtered(filter int16, asked kevent) (data int64, ended bool, least int64, applies bool) {
	least = 1
	if filter == filterRead && asked.fflags&noteLowWater != 0 {
		least = max(asked.data, 1)
	}
	switch {
	case f.kind == socketFD && filter == filterRead:
		return int64(len(f.sock.data)), f.sock.cantReceive, least, true
	case f.kind == socketFD:
		return int64(f.sock.space()), f.sock.cantSend, socketLeast, true
	case filter == filterRead && !f.writing():
		return int64(len(f.pipe.data)), f.pipe.writers == 0, least, true
	case filter == filterWrite && f.writing():
		return int64(f.pipe.size - len(f.pipe.data)), f.pipe.readers == 0, pipeBuf, true
	}
	return 0, false, 0, false
}

// changes is how often something has happened to the pipe or the pair of sockets a descriptor
// is of.
func (f *fd) changes() uint64 {
	if f.kind == socketFD {
		return f.sock.pair.changes
	}
	return f.pipe.changes
}

// polled is what poll answers of one descriptor, for the events it was asked about; false if
// nothing here says when the descriptor can be read or written (a terminal, a kqueue).
func (p *Process) polled(n int32, events uint16) (uint16, bool) {
	f := p.fds[uint64(n)]
	switch {
	case n < 0:
		return 0, true
	case f == nil, f.isDirectory():
		return pollInvalid, true
	case f.isFile():
		return events & (pollReading | pollWriting), true
	case f.kind == socketFD:
		return f.sock.polled(events), true
	case f.kind != pipeFD:
		return 0, false
	}
	// Each end of a pipe answers for what it is for, and says that the other has gone only to
	// one who asked about that.
	q := f.pipe
	switch {
	case !f.writing() && events&pollReading != 0:
		var answer uint16
		if len(q.data) > 0 || q.writers == 0 {
			answer = events & pollReading
		}
		if q.writers == 0 {
			answer |= pollHangup
		}
		return answer, true
	case f.writing() && events&pollWriting != 0:
		switch {
		case q.readers == 0:
			return pollHangup, true
		case q.size-len(q.data) >= pipeBuf:
			return events & pollWriting, true
		}
	}
	return 0, true
}

// polling is a poll that waits: for which descriptors, and where to answer.
type polling struct{ fds, count uint64 }

// pollAll answers a poll's descriptors, and returns how many have something to say; false if
// one of them is of a kind nothing here answers for.
func (p *Process) pollAll(w *polling, write bool) (count int64, known bool) {
	known = true
	out := p.read(w.fds, w.count*8)
	for i := uint64(0); i < w.count; i++ {
		answer, ok := p.polled(int32(le.Uint32(out[8*i:])), le.Uint16(out[8*i+4:]))
		known = known && ok
		le.PutUint16(out[8*i+6:], answer)
		if answer != 0 {
			count++
		}
	}
	if write || count > 0 {
		p.write(w.fds, out)
	}
	return count, known
}

// sysPoll is poll(descriptors, how many, milliseconds to wait: none, or for good if negative).
func sysPoll(p *Process, a []uint64) int64 {
	w := &polling{fds: a[0], count: a[1]}
	if w.count > maxFD {
		return -eINVAL
	}
	count, known := p.pollAll(w, true)
	timeout := int32(a[2])
	if count > 0 || timeout == 0 {
		return count
	}
	if !known {
		p.unimplemented("poll that waits for a descriptor that is no pipe's and no file's", a)
		return -eNOSYS
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = p.m.clock.now().Add(time.Duration(timeout) * time.Millisecond)
	}
	p.current.polling = w
	p.wait(waitSelect, 0, deadline, result{})
	return 0
}
