package emulator

import (
	"fmt"
	"maps"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// A pseudo-terminal: two ends with the kernel's handling of a line between them. The first is
// the one a terminal window, script or ssh holds (the name /dev/ptmx gives a new one each time
// it is opened); the second, named /dev/ttysNNN, is the terminal of what is run in it. What is
// written to the first is typed: it is echoed back to the first, edited as a line, and read at
// the second a line at a time, or as it comes once the second is told to leave bytes alone.
// What the second writes is read at the first, a newline made a return and a newline. As the
// kernel's source has it (bsd/kern/tty.c, tty_pty.c, tty_ptmx.c) and as measured on macOS
// 27.2.1 (examples/pty.c).
//
// Not here yet, and stopping the run when asked for: a terminal as a session's own (signals
// from the keys that interrupt, quit and suspend, the group in front, hanging up), flow
// control, the keys that discard output and ask for the status, tabs made spaces or rubbed
// out, a wait for fewer or more bytes than one or for a time, more typed than a terminal
// holds, and what was not measured of the rest.

const (
	// Input.
	termMarkParity, termStrip, termNewlineReturn, termNoReturn, termReturnNewline, termFlow, termUTF8 = 0x8, 0x20, 0x40, 0x80, 0x100, 0x200, 0x4000
	// Output.
	termProcess, termNewlineBoth, termTabs = 0x1, 0x2, 0x4
	// Local.
	termEchoKillLine, termEchoErase, termEchoKill, termEcho, termEchoNewline = 0x1, 0x2, 0x4, 0x8, 0x10
	termEchoPrint, termEchoControl, termAltWordErase                         = 0x20, 0x40, 0x200
	termSignals, termLines, termExtended                                     = 0x80, 0x100, 0x400

	// The characters that do something, by where they are kept.
	ccEnd, ccEndLine, ccEndLine2, ccErase, ccWordErase, ccKill, ccReprint = 0, 1, 2, 3, 4, 5, 6
	ccInterrupt, ccQuit, ccSuspend, ccDelayedSuspend, ccStart, ccStop     = 8, 9, 10, 11, 12, 13
	ccLiteral, ccDiscard, ccLeast, ccTime, ccStatus                       = 14, 15, 16, 17, 18
	ccNone                                                                = 0xFF // _POSIX_VDISABLE

	termiosSize = 72   // four flags of eight bytes, twenty characters and four spare, two speeds
	inputMost   = 1022 // typed and not read: short of what the kernel holds (1024, MAX_INPUT), past which it was not measured
	closeWait   = 600 * time.Millisecond
	// What the second end has written and the first not read: 1024 bytes at most, and the
	// second can be written, to poll, select and a kqueue, while no more than 256 wait. Both
	// as measured.
	outputMost      = 1024
	outputWritable  = 256
	ioctlDeviceType = 0x4004667a // FIODTYPE
	ioctlGetTerm    = 0x40487413 // TIOCGETA
	ioctlSetTerm    = 0x80487414 // TIOCSETA; the next two are TIOCSETAW and TIOCSETAF
	ioctlGetSize    = 0x40087468 // TIOCGWINSZ
	ioctlSetSize    = 0x80087467 // TIOCSWINSZ
	ioctlGrant      = 0x20007454 // TIOCPTYGRANT
	ioctlName       = 0x40807453 // TIOCPTYGNAME
	ioctlUnlock     = 0x20007452 // TIOCPTYUNLK
)

// terminalDefaults are what a terminal is set to when its second end is first opened, from the
// source (TTYDEF_IFLAG and the rest, and ttydefchars) and as measured of the flags, the speeds
// and eight of the characters.
var terminalDefaults = struct {
	input, output, control, local, speed uint64
	characters                           [20]byte
}{0x2b02, 0x3, 0x4b00, 0x5cb, 9600,
	[20]byte{4, ccNone, ccNone, 0x7f, 0x17, 0x15, 0x12, ccNone, 3, 0x1c, 0x1a, 0x19, 0x11, 0x13, 0x16, 0x0f, 1, 0, 0x14, ccNone}}

// typedLine is a line that waits to be read: how many bytes of it are to be read, and whether
// the byte that ended it is the end of input, which waits with them and is not read.
type typedLine struct {
	size int
	end  bool
}

type terminal struct {
	id     uint64 // among the pipes': what a thread that waits on either end waits on
	number int    // of its name
	clock  *clock
	// Its settings, and its size in rows, columns and dots each way.
	input, output, control, local uint64
	characters                    [20]byte
	inSpeed, outSpeed             uint64
	size                          [4]uint16
	// What was typed: the line not yet ended, with how many of its last letters were echoed
	// since the second end last wrote (the source's t_rocount: rubbing out goes back over
	// those, and past them shows the line again), and whether the next byte is to be taken
	// as it is; and what the second end can read, which with lines on is the lines that
	// have ended.
	line    []byte
	echoed  int
	literal bool
	typed   []byte
	lines   []typedLine
	shown   []byte // what the first end can read: the echo, and what the second wrote
	firsts  int    // descriptors open of the first end: one, or none once it has closed
	// The second end's: how many are open, that it has been opened at some time, whose it
	// was made by grantpt and that it may be opened; and until when what it wrote waits to
	// be read after its last close.
	seconds  int
	was      bool
	granted  bool
	owner    uint32
	unlocked bool
	until    time.Time
	changes  uint64
}

// name is the path of a terminal's second end.
func (t *terminal) name() string { return fmt.Sprintf("/dev/ttys%03d", t.number) }

func (t *terminal) is(bit uint64) bool { return t.local&bit != 0 }

// reset sets a terminal as one is when its second end is opened with no other open: the
// settings from the source and as measured, the size none, as measured.
func (t *terminal) reset() {
	d := terminalDefaults
	t.input, t.output, t.control, t.local, t.characters, t.inSpeed, t.outSpeed = d.input, d.output, d.control, d.local, d.characters, d.speed, d.speed
	t.size = [4]uint16{}
}

// settle lets go of what the second end wrote and closed on, once the first has read it or the
// wait for that is over.
func (t *terminal) settle() {
	if !t.until.IsZero() && (len(t.shown) == 0 || !t.clock.now().Before(t.until)) {
		t.shown, t.until = nil, time.Time{}
	}
}

// terminalNamed is the terminal whose second end a path names, if it names one.
func (p *Process) terminalNamed(path string) *terminal {
	digits, ok := strings.CutPrefix(p.absolute(path), "/dev/ttys")
	n, err := strconv.Atoi(digits)
	if !ok || err != nil || n >= len(p.m.terminals) || len(digits) != 3 {
		return nil
	}
	return p.m.terminals[n]
}

// terminalNames are the names in /dev of the terminals there are.
func (p *Process) terminalNames() []string {
	var names []string
	for _, t := range p.m.terminals {
		if t != nil {
			names = append(names, pathpkg.Base(t.name()))
		}
	}
	return names
}

// openTerminal opens /dev/ptmx or the second end of a terminal by its name; false if the path
// names neither. The second end is root's until grantpt, and nobody else's to open (EACCES);
// until unlockpt it is EAGAIN; it may be opened to read only or to write only. As measured.
func (p *Process) openTerminal(path string, flags uint64) (int64, bool) {
	const nonblock, readWrite = 0x4, 2
	first := p.absolute(path) == "/dev/ptmx"
	t := p.terminalNamed(path)
	if !first && t == nil {
		return 0, false
	}
	if t != nil {
		t.settle()
	}
	switch {
	case flags&3 == 3:
		return -eINVAL, true
	case first && flags&3 != readWrite:
		p.unimplemented("the first end of a terminal opened to read only or to write only", []uint64{flags})
		return -eNOSYS, true
	case first:
		p.m.pipes++
		// The lowest number no terminal has (the source's ptmx_get_ioctl).
		number := slices.Index(p.m.terminals, nil)
		if number < 0 {
			number = len(p.m.terminals)
			p.m.terminals = append(p.m.terminals, nil)
		}
		t = &terminal{id: p.m.pipes, number: number, clock: p.m.clock, firsts: 1}
		t.reset()
		p.m.terminals[number] = t
	case !t.granted && p.uid != 0:
		return -eACCES, true
	case !t.unlocked:
		return -eAGAIN, true
	case t.firsts == 0 && flags&nonblock == 0:
		// With nobody at the first end an open waits for one, which never comes: measured
		// not to return.
		p.unimplemented("the second end of a terminal opened, waiting, after the first has closed", nil)
		return -eNOSYS, true
	case !t.until.IsZero():
		p.unimplemented("the second end of a terminal opened again while what it wrote before its last close waits to be read", nil)
		return -eNOSYS, true
	default:
		if t.seconds == 0 {
			t.reset()
		}
		t.seconds++
		t.was = true
		p.terminalChanged(t)
	}
	if flags&0x20000 == 0 { // O_NOCTTY
		// Whether it becomes the session's own terminal is the kernel's to decide for a
		// process that leads a session and has none.
		if p.sessionOf() == p.pid {
			p.unimplemented("a terminal opened by the leader of a session without O_NOCTTY: it would become the session's own", nil)
			return -eNOSYS, true
		}
	}
	n := p.newFD(&fd{kind: ttyFD, tty: t, first: first, flags: uint32(flags) & (3 | flagNonblock), refs: 1})
	if flags&0x1000000 != 0 { // O_CLOEXEC
		p.cloexec[n] = true
	}
	return int64(n), true
}

func (p *Process) terminalChanged(t *terminal) {
	t.changes++
	p.m.streamChanged(t.id)
}

// terminalClosed is the last descriptor of an end closing. Either takes with it what was typed
// and not read. What the second end wrote and the first has not read waits for it a while, 0.6
// seconds as measured, and then goes too; a close waits that long (see closeFD), and a process
// that ends does not, though under the kernel its ending does. All as measured, but that what
// the second wrote goes at once with the first end, which is from the source.
func (p *Process) terminalClosed(f *fd) {
	if f.refs--; f.refs > 0 {
		return
	}
	t := f.tty
	switch {
	case f.first:
		t.firsts = 0
		t.line, t.typed, t.lines, t.shown, t.until, t.echoed, t.literal = nil, nil, nil, nil, time.Time{}, 0, false
	case t.seconds > 1:
		t.seconds--
	default:
		t.seconds = 0
		t.line, t.typed, t.lines, t.echoed, t.literal = nil, nil, nil, 0, false
		if len(t.shown) > 0 && t.firsts > 0 {
			t.until = t.clock.now().Add(closeWait)
		}
	}
	if t.firsts+t.seconds == 0 { // its number and its name are free for another (the source's ptmx_free_ioctl)
		p.m.terminals[t.number] = nil
	}
	p.terminalChanged(t)
}

// leavesTerminal closes, for a process that is ending, the descriptors of a terminal's second
// end that are the last of it, where what was written there has not been read, and gives one
// of them: the process's end waits for that as a close does. One that ends another way (a
// signal, its last thread) does not wait here, though under the kernel it does.
func (p *Process) leavesTerminal() *fd {
	held := map[*fd]int{}
	for _, f := range p.fds {
		if f.kind == ttyFD && !f.first {
			held[f]++
		}
	}
	for f, n := range held {
		if t := f.tty; n != f.refs || t.seconds != 1 || len(t.shown) == 0 || t.firsts == 0 {
			delete(held, f)
		}
	}
	var left *fd
	for _, n := range slices.Sorted(maps.Keys(p.fds)) {
		f := p.fds[n]
		if held[f] == 0 {
			continue
		}
		p.closeFD(n)
		if !f.tty.until.IsZero() {
			left = f
		}
	}
	return left
}

// terminalPut sends a byte the second end wrote, or an echo, on its way to the first, and says
// whether there was room. A newline is a return and a newline, unless output is left alone;
// with room for the return alone, the return goes and the newline does not, so that the newline
// written again brings a second return, as measured.
func (p *Process) terminalPut(t *terminal, c byte) bool {
	switch {
	case t.output&termProcess == 0:
	case t.output&^(termProcess|termNewlineBoth|termTabs) != 0:
		p.unimplemented("output to a terminal with settings that were not measured", []uint64{t.output})
	case c == '\t' && t.output&termTabs != 0:
		p.unimplemented("a tab written to a terminal set to make tabs spaces", nil)
	case c == '\n' && t.output&termNewlineBoth != 0:
		if len(t.shown) == outputMost {
			return false
		}
		t.shown = append(t.shown, '\r')
	}
	if len(t.shown) == outputMost {
		return false
	}
	t.shown = append(t.shown, c)
	return true
}

// terminalShow is an echo, on its way out as anything is. There is no waiting for room for
// one: the kernel lets it go, which was not measured, and here it stops.
func (p *Process) terminalShow(t *terminal, echo ...byte) {
	for _, c := range echo {
		if !p.terminalPut(t, c) {
			p.unimplemented("typing echoed at a terminal whose first end has not read what waits for it", nil)
			return
		}
	}
}

// control says whether a byte is one the terminal shows as ^ and a letter.
func control(c byte) bool { return c < 0x20 && c != '\t' && c != '\n' || c == 0x7f }

// terminalEcho shows the first end a byte that was typed, if the terminal is set to (the
// source's ttyecho).
func (p *Process) terminalEcho(t *terminal, c byte) {
	switch {
	case !t.is(termEcho) && !(t.is(termEchoNewline) && c == '\n'):
	case control(c) && t.is(termEchoControl):
		p.terminalShow(t, '^', c^0x40)
	default:
		p.terminalShow(t, c)
	}
}

// terminalRetype shows the line again under what is there: the key that asks for it, a newline,
// and all that was typed and not read (the source's ttyretype; measured of a line not ended).
func (p *Process) terminalRetype(t *terminal) {
	if c := t.characters[ccReprint]; c != ccNone {
		p.terminalEcho(t, c)
	}
	p.terminalShow(t, '\n')
	for _, c := range t.typed {
		p.terminalEcho(t, c)
	}
	for _, c := range t.line {
		p.terminalEcho(t, c)
	}
	t.echoed = len(t.line)
}

// terminalRub takes the last byte of the line and takes it off the first end's screen (the
// source's ttyrub): going back over it, or, where the second end has written since it was
// echoed, showing the line again.
func (p *Process) terminalRub(t *terminal) {
	c := t.line[len(t.line)-1]
	t.line = t.line[:len(t.line)-1]
	switch {
	case !t.is(termEcho):
		return
	case t.is(termEchoPrint) || c >= 0x80 || c == '\t' || c == '\n':
		p.unimplemented("a byte rubbed out at a terminal that was not measured: a tab, a newline, one above 127, or with the erased shown between marks", []uint64{uint64(c)})
		return
	case !t.is(termEchoErase):
		p.terminalEcho(t, t.characters[ccErase])
	case t.echoed == 0:
		p.terminalRetype(t)
		return
	case !control(c):
		p.terminalShow(t, '\b', ' ', '\b')
	case t.is(termEchoControl):
		p.terminalShow(t, '\b', ' ', '\b', '\b', ' ', '\b')
	}
	t.echoed--
}

// terminalType is the first end written to: each byte typed, in the order the source's
// ttyinput has it.
func (p *Process) terminalType(t *terminal, data []byte) {
	cc := t.characters
	stop := func(what string, c byte) { p.unimplemented(what, []uint64{uint64(c)}) }
	blank := func(c byte) bool { return c == ' ' || c == '\t' }
	for _, c := range data {
		if t.input&termStrip != 0 {
			c &= 0x7f
		}
		// A byte after the key that says to take the next as it is, is no key at all.
		literal := t.literal
		t.literal = false
		key := func(which int) bool { return !literal && cc[which] != ccNone && c == cc[which] }
		extended, lines := t.is(termExtended), t.is(termLines)
		switch {
		case extended && key(ccLiteral):
			switch {
			case !t.is(termEcho):
			case t.is(termEchoErase):
				p.terminalShow(t, '^', '\b')
			default:
				p.terminalEcho(t, c)
			}
			t.literal = true
			continue
		case extended && key(ccDiscard):
			stop("the key that discards output typed at a terminal", c)
			return
		case t.is(termSignals) && (key(ccInterrupt) || key(ccQuit) || key(ccSuspend) || extended && key(ccDelayedSuspend)):
			stop("a key that interrupts, quits or suspends typed at a terminal: the signals a terminal sends are not here yet", c)
			return
		case t.input&termFlow != 0 && (key(ccStart) || key(ccStop)):
			stop("a key that stops or starts output typed at a terminal", c)
			return
		case literal:
		case c == '\r' && t.input&termNoReturn != 0:
			continue
		case c == '\r' && t.input&termReturnNewline != 0:
			c = '\n'
		case c == '\n' && t.input&termNewlineReturn != 0:
			c = '\r'
		}
		switch {
		case !lines:
		case key(ccErase):
			if len(t.line) > 0 {
				if t.input&termUTF8 != 0 && t.line[len(t.line)-1] >= 0x80 {
					stop("a letter of more bytes than one rubbed out at a terminal", c)
					return
				}
				p.terminalRub(t)
			}
			continue
		case key(ccKill):
			if t.is(termEchoKillLine) && len(t.line) == t.echoed && !t.is(termEchoPrint) {
				for len(t.line) > 0 {
					p.terminalRub(t)
				}
			} else { // the key and a newline, where the line cannot be gone back over
				p.terminalEcho(t, c)
				if t.is(termEchoKill) || t.is(termEchoKillLine) {
					p.terminalEcho(t, '\n')
				}
				t.line, t.echoed = nil, 0
			}
			continue
		case key(ccWordErase) && extended:
			if t.is(termAltWordErase) {
				stop("a word rubbed out at a terminal set to the other meaning of a word", c)
				return
			}
			for len(t.line) > 0 && blank(t.line[len(t.line)-1]) {
				p.terminalRub(t)
			}
			for len(t.line) > 0 && !blank(t.line[len(t.line)-1]) {
				p.terminalRub(t)
			}
			continue
		case key(ccReprint) && extended:
			p.terminalRetype(t)
			continue
		case key(ccStatus) && extended:
			stop("the key that asks for the status typed at a terminal", c)
			return
		}
		switch {
		case len(t.line)+len(t.typed) >= inputMost:
			stop("more typed at a terminal than it holds unread", c)
			return
		case c == 0xff && t.input&termMarkParity != 0:
			stop("a byte of all ones typed at a terminal that marks errors of parity", c)
			return
		case !lines:
			t.typed = append(t.typed, c)
			p.terminalEcho(t, c)
			continue
		}
		t.line = append(t.line, c)
		end := key(ccEnd)
		if end || !literal && c == '\n' || key(ccEndLine) || key(ccEndLine2) && extended {
			t.typed = append(t.typed, t.line...)
			t.lines = append(t.lines, typedLine{len(t.line) - int(truth(end)), end})
			t.line, t.echoed = nil, 0
		} else {
			t.echoed++
		}
		p.terminalEcho(t, c)
		if end && t.is(termEcho) && control(c) && t.is(termEchoControl) { // the end of input is shown and gone back over
			p.terminalShow(t, '\b', '\b')
		}
	}
}

// terminalGone says that nobody is at the other end, where somebody has been: the first end's
// descriptor closed, or the second's all closed after one was opened and what it wrote read
// or let go. Before the second is first opened the first end waits for it.
func (f *fd) terminalGone() bool {
	t := f.tty
	t.settle()
	return f.first && t.seconds == 0 && t.was && t.until.IsZero() || !f.first && t.firsts == 0
}

// terminalWrite is write to either end. With nobody at the other end it is EIO. What is typed
// at the first before the second is opened waits for it; what the second writes waits for room,
// the first end reading. A descriptor that does not wait takes what there is room for, or is
// EAGAIN. All as measured.
func (p *Process) terminalWrite(f *fd, data []byte) int64 {
	if f.flags&3 == 0 {
		return -eBADF
	}
	op := &pipeOp{f: f, data: data, sending: true}
	if r, ok := p.tryTerminal(op); ok {
		return r
	}
	if f.flags&flagNonblock != 0 {
		if op.done > 0 {
			return op.done
		}
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.tty.id, time.Time{}, result{})
	return 0
}

func (p *Process) tryTerminalWrite(op *pipeOp) (int64, bool) {
	f, t := op.f, op.f.tty
	switch {
	case f.terminalGone() && op.done > 0:
		return op.done, true
	case f.terminalGone():
		return -eIO, true
	case f.first && t.seconds == 0 && t.was:
		p.unimplemented("typing at a terminal whose second end has closed and whose last writing waits to be read", nil)
		return -eNOSYS, true
	case f.first && t.seconds == 0:
		return 0, false
	case f.first:
		p.terminalType(t, op.data)
		op.done, op.data = int64(len(op.data)), nil
		return op.done, true
	}
	if len(op.data) > 0 {
		t.echoed = 0 // what was echoed of the line is no longer what is on the screen's last line
	}
	for len(op.data) > 0 && p.terminalPut(t, op.data[0]) {
		op.data = op.data[1:]
		op.done++
	}
	return op.done, len(op.data) == 0
}

// waiting is how much of what was typed the second end can read now: the lines that have
// ended, each with the end of input that ended it, or with lines off all of it. FIONREAD
// answers it at either end, and a kqueue.
func (t *terminal) waiting() int {
	if !t.is(termLines) {
		return len(t.typed)
	}
	n := 0
	for _, line := range t.lines {
		n += line.size + int(truth(line.end))
	}
	return n
}

// terminalReady says whether an end can be read, or written, without waiting, as select has
// it: either once nobody is at the other end, and neither at the first end before the second
// is first opened. The second can be written while little waits for the first to read.
func (f *fd) terminalReady(writing bool) bool {
	t := f.tty
	switch {
	case f.terminalGone():
		return true
	case f.first && !t.was:
		return false
	case f.first:
		return writing || len(t.shown) > 0
	case writing:
		return len(t.shown) <= outputWritable
	}
	return t.waiting() > 0
}

// terminalRead is read of either end: at the first what is there to see; at the second a line,
// or with lines off what was typed. With nothing there it waits, or for a descriptor that does
// not wait is EAGAIN; with nobody at the other end it is the end, as measured.
func (p *Process) terminalRead(f *fd, into []stretch) int64 {
	if f.flags&3 == 1 {
		return -eBADF
	}
	op := &pipeOp{f: f, into: into}
	for _, s := range into {
		op.room += s.size
	}
	if t := f.tty; !f.first && !t.is(termLines) && (t.characters[ccLeast] != 1 || t.characters[ccTime] != 0) {
		p.unimplemented("a read of a terminal set to wait for other than one byte and no time", []uint64{uint64(t.characters[ccLeast]), uint64(t.characters[ccTime])})
		return -eNOSYS
	}
	if op.room == 0 {
		return 0
	}
	if r, ok := p.tryTerminal(op); ok {
		return r
	}
	if f.flags&flagNonblock != 0 {
		return -eAGAIN
	}
	p.current.pipeOp = op
	p.wait(waitPipe, f.tty.id, time.Time{}, result{})
	return 0
}

// tryTerminal gives a read, a write or a setting that waits what it can have now, and says
// whether that is the end of it. What it changes wakes whoever waits on the terminal, which
// comes back here for this one too, if it waited: that finds it being served.
func (p *Process) tryTerminal(op *pipeOp) (int64, bool) {
	if op.trying {
		return 0, false
	}
	op.trying = true
	t := op.f.tty
	defer func(was uint64, shown, typed, lines int) {
		op.trying = false
		if len(t.shown) != shown || len(t.typed) != typed || len(t.lines) != lines || t.changes != was {
			p.terminalChanged(t)
		}
	}(t.changes, len(t.shown), len(t.typed), len(t.lines))
	switch {
	case op.closing: // a close that waits for what was written to be read, or for long enough
		t.settle()
		return 0, t.until.IsZero()
	case op.setting != nil:
		return p.trySetTerminal(op)
	case op.sending:
		return p.tryTerminalWrite(op)
	}
	return p.tryTerminalRead(op)
}

// tryTerminalRead: a line is read to its end, or as much of it as there is room for; the end
// of input that ended a line goes with a read that had room for more than the line, and is
// otherwise the next read's, of nothing. As measured.
func (p *Process) tryTerminalRead(op *pipeOp) (int64, bool) {
	f, t := op.f, op.f.tty
	take := func(from *[]byte, n uint64) uint64 {
		n = min(n, op.room)
		p.put(op, (*from)[:n])
		*from = (*from)[n:]
		return n
	}
	gone := f.terminalGone()
	switch {
	case f.first && len(t.shown) > 0:
		return int64(take(&t.shown, uint64(len(t.shown)))), true
	case f.first:
		return 0, gone
	case t.is(termLines) && len(t.lines) > 0:
		line := &t.lines[0]
		n := take(&t.typed, uint64(line.size))
		line.size -= int(n)
		switch {
		case line.size > 0:
		case line.end && op.room > n:
			t.typed, t.lines = t.typed[1:], t.lines[1:]
		case !line.end:
			t.lines = t.lines[1:]
		}
		return int64(n), true
	case !t.is(termLines) && len(t.typed) > 0:
		return int64(take(&t.typed, uint64(len(t.typed)))), true
	}
	return 0, gone
}

// terminalPolled is what poll answers of an end: readable with something to read, writable
// (the first end even before the second is opened), and once nobody is at the other end hung
// up, with a read that will not wait, and not writable. As measured.
func (f *fd) terminalPolled(events uint16) uint16 {
	var answer uint16
	if events&pollReading != 0 && f.terminalReady(false) {
		answer |= events & pollReading
	}
	switch {
	case f.terminalGone():
		answer |= pollHangup
	case f.terminalReady(true), f.first:
		answer |= events & pollWriting
	}
	return answer
}

// terminalFiltered is what a kqueue says of an end, to read or to write: whether there is an
// event, its count, and whether it says the end has come. The second's counts are what waits
// to be read and the room there is to write; the first's what waits to be read, and to write
// none, with an event all the same, even before the second end is opened. As measured.
func (f *fd) terminalFiltered(filter int16) (data int64, ended, ready bool) {
	t, reading := f.tty, filter == filterRead
	switch {
	case f.terminalGone():
		return 0, true, true
	case f.first && reading:
		return int64(len(t.shown)), false, len(t.shown) > 0
	case f.first:
		return 0, false, true
	case reading:
		return int64(t.waiting()), false, t.waiting() > 0
	}
	return int64(outputMost - len(t.shown)), false, len(t.shown) <= outputWritable
}

// terminalIoctl is what is asked of a terminal by ioctl, at either end: its settings read and
// set, its size, how much waits to be read, and at the first end the three of grantpt,
// unlockpt and ptsname. Before the second end is first opened the first is no terminal to the
// requests of one (ENOTTY, as measured of four of them). Any other stops.
func (p *Process) terminalIoctl(f *fd, a []uint64) int64 {
	t := f.tty
	t.settle()
	request := uint32(a[1])
	switch request {
	case ioctlGetTerm, ioctlSetTerm, ioctlSetTerm + 1, ioctlSetTerm + 2, ioctlGetSize, ioctlSetSize, ioctlAvailable:
		if f.first && !t.was {
			return -eNOTTY
		}
	}
	switch request {
	case ioctlGetTerm:
		b := le64(t.input, t.output, t.control, t.local)
		b = append(append(b, t.characters[:]...), 0, 0, 0, 0)
		p.write(a[2], append(b, le64(t.inSpeed, t.outSpeed)...))
	case ioctlSetTerm, ioctlSetTerm + 1, ioctlSetTerm + 2:
		op := &pipeOp{f: f, setting: p.read(a[2], termiosSize), flags: uint64(request)}
		if f.first { // set at the first end, what waits there to be read is let go, as measured
			t.shown = nil
		}
		if r, ok := p.tryTerminal(op); ok {
			return r
		}
		p.current.pipeOp = op
		p.wait(waitPipe, t.id, time.Time{}, result{})
	case ioctlGetSize:
		p.write(a[2], le16(t.size[:]...))
	case ioctlSetSize:
		b := p.read(a[2], 8)
		if size := [4]uint16{le.Uint16(b), le.Uint16(b[2:]), le.Uint16(b[4:]), le.Uint16(b[6:])}; size != t.size {
			t.size = size // whoever has it for its own would be told (SIGWINCH), and nobody has yet
		}
	case ioctlAvailable:
		p.write(a[2], le32(uint32(t.waiting())))
	case ioctlDeviceType: // what isatty asks: both ends are of the kind D_TTY (the source's two device switches)
		p.write(a[2], le32(3))
	case ioctlNonblock:
		if f.flags &^= flagNonblock; p.u32(a[2]) != 0 {
			f.flags |= flagNonblock
		}
	case ioctlGrant, ioctlUnlock, ioctlName:
		if !f.first {
			return -eNOTTY
		}
		switch request {
		case ioctlGrant:
			t.granted, t.owner = true, p.uid
		case ioctlUnlock:
			t.unlocked = true
		case ioctlName:
			p.write(a[2], fixed(t.name(), 128))
		}
	default:
		p.unimplemented("ioctl of a terminal that was not measured", a)
		return -eNOSYS
	}
	return 0
}

// trySetTerminal sets a terminal's settings (tcsetattr). The two kinds that wait (TCSADRAIN,
// TCSAFLUSH) wait for the first end to have read what the second wrote, by the source; the
// second of them then lets go what was typed and not read. Lines turned off, what was typed of
// a line can be read with the lines that had ended, as measured.
func (p *Process) trySetTerminal(op *pipeOp) (int64, bool) {
	t, b, request := op.f.tty, op.setting, uint32(op.flags)
	if request != ioctlSetTerm && len(t.shown) > 0 && t.firsts > 0 {
		return 0, false
	}
	lines := t.is(termLines)
	local := le.Uint64(b[24:]) &^ 0x800 // EXTPROC is not set this way, by the source
	switch {
	case request == ioctlSetTerm+2:
		t.line, t.typed, t.lines, t.echoed, t.literal = nil, nil, nil, 0, false
	case !lines && local&termLines != 0 && len(t.typed) > 0:
		p.unimplemented("a terminal told to take lines with something typed and not read", nil)
		return -eNOSYS, true
	case lines && local&termLines == 0:
		t.typed, t.line, t.lines, t.echoed = append(t.typed, t.line...), nil, nil, 0
	}
	t.input, t.output, t.control, t.local = le.Uint64(b), le.Uint64(b[8:]), le.Uint64(b[16:]), local
	copy(t.characters[:], b[32:52])
	t.inSpeed, t.outSpeed = le.Uint64(b[56:]), le.Uint64(b[64:])
	if t.inSpeed == 0 { // by the source
		t.inSpeed = t.outSpeed
	}
	t.changes++
	return 0, true
}

// terminalStat is the struct stat64 of an end: a device of characters of the group tty, the
// first end root's and anybody's to read and write, the second root's until grantpt makes it
// the user's, to be written to by the group. The device's number is the terminal's with, above
// it, which kind of device: 16 for the second ends and 15 for the first, as measured on macOS
// 27.2.1 (the kernel gives them out as it starts, and they were not measured on iOS). ttyname
// finds a terminal's name by it. The times and the rest are not known and are left out.
func (p *Process) terminalStat(t *terminal, first bool) []byte {
	b := make([]byte, 144)
	mode, kind := uint16(syscall.S_IFCHR|0o666), uint32(15)
	if !first {
		mode, kind = syscall.S_IFCHR|0o620, 16
		le.PutUint32(b[16:], t.owner)
	}
	le.PutUint16(b[4:], mode)
	le.PutUint16(b[6:], 1)
	le.PutUint32(b[20:], 4)
	le.PutUint32(b[24:], kind<<24|uint32(t.number))
	return b
}
