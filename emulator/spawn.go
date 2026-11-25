package emulator

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"os"
	"slices"
	"syscall"
	"time"

	"github.com/blacktop/go-macho"
)

// A process starts another with posix_spawn: a program of the build's, of the home or of a
// mount, or its own, as a process of its own on the machine, with the descriptors it is left by
// its parent and what it is told to do with them. The parent learns how it ended with wait4.
// What a Mac's kernel does was measured with examples/spawn.c. fork and execve, which make a
// process of a running one's memory or put a program in its place, are not here.

const (
	eCHILD      = 10
	eNOEXEC     = 8
	eBADARCH    = 86
	waitNoHang  = 0x1
	waitStopped = 0x2 // WUNTRACED: of children that were stopped, which none here is

	// What a child is to be started with, of its attributes' flags (POSIX_SPAWN_…).
	spawnSetPgroup      = 0x2
	spawnSetSigdef      = 0x4
	spawnSetSigmask     = 0x8
	spawnCloexecDefault = 0x4000

	spawnActionSize = 1040 // of one thing to do with a descriptor (_psfa_action_t)

	fdCloexec = 0x1 // FD_CLOEXEC
)

// waiting4 is a wait4 that waits: for which child, and where to say how it ended.
type waiting4 struct {
	pid            int32
	status, rusage uint64
}

// program is the executable at a path, and the name to run it under; or the error posix_spawn
// answers with for a path that cannot be run.
func (p *Process) program(path string) ([]byte, int64) {
	abs := p.absolute(path)
	if abs == p.path {
		return p.exe, 0
	}
	var image []byte
	switch t, ok := p.lookup(abs); {
	case ok && t.kind == dirFD:
		return nil, -eACCES
	case ok && t.kind == hostFD && t.root != nil:
		info, err := t.root.Stat(t.rel)
		switch {
		case err != nil:
			return nil, hostErrno(err)
		case info.IsDir() || info.Mode()&0o111 == 0:
			return nil, -eACCES
		}
		if image, err = t.root.ReadFile(t.rel); err != nil {
			return nil, hostErrno(err)
		}
	case ok && t.kind == hostFD:
		var err error
		if image, err = os.ReadFile(t.host); err != nil {
			return nil, hostErrno(err)
		}
	default:
		// Of the build: any of its programs can be run, whatever of it the guest may read.
		if p.m.cfg.Program == nil {
			return nil, -eNOENT
		}
		data, dir, err := away3(p.m.clock, func() ([]byte, bool, error) { return p.m.cfg.Program(abs) })
		switch {
		case err != nil:
			stopf("read %s: %v", abs, err)
		case dir:
			return nil, -eACCES
		case data == nil:
			return nil, -eNOENT
		}
		image = data
	}
	if _, err := macho.NewFatFile(bytes.NewReader(image)); err != nil {
		if _, err := macho.NewFile(bytes.NewReader(image)); err != nil {
			if bytes.HasPrefix(image, []byte("#!")) {
				p.unimplemented("posix_spawn of a script, which the kernel hands to its interpreter", nil)
			}
			return nil, -eNOEXEC
		}
	}
	if _, err := thin(image, 0, cpuSubtypeARM64E); err != nil {
		return nil, -eBADARCH
	}
	return image, 0
}

// away3 is away for a fetch of two results.
func away3[A, B any](c *clock, fetch func() (A, B, error)) (a A, b B, err error) {
	_, err = away(c, func() (struct{}, error) {
		var e error
		a, b, e = fetch()
		return struct{}{}, e
	})
	return a, b, err
}

// strings reads a list of pointers to strings that ends with none: arguments, or an environment.
func (p *Process) strings(addr uint64) []string {
	var out []string
	for ; addr != 0 && len(out) < 1<<16; addr += 8 {
		at := p.u64(addr)
		if at == 0 {
			break
		}
		out = append(out, p.cstr(at))
	}
	return out
}

// sysPosixSpawn is posix_spawn(&pid, path, description, arguments, environment).
func sysPosixSpawn(p *Process, a []uint64) int64 {
	path := p.cstr(a[1])
	var flags uint16
	var actions []byte
	if a[2] != 0 {
		desc := p.read(a[2], 32)
		if size, attr := le.Uint64(desc), le.Uint64(desc[8:]); attr != 0 && size >= 2 {
			flags = uint16(p.u32(attr))
		}
		if size, at := le.Uint64(desc[16:]), le.Uint64(desc[24:]); at != 0 {
			actions = p.read(at, size)
			if size < 8 || size != 8+uint64(le.Uint32(actions[4:]))*spawnActionSize {
				p.unimplemented("posix_spawn with file actions laid out another way", a)
				return -eINVAL
			}
		}
	}
	if other := flags &^ (spawnSetPgroup | spawnSetSigdef | spawnSetSigmask | spawnCloexecDefault); other != 0 {
		p.unimplemented(fmt.Sprintf("posix_spawn with the flags %#x", other), a)
		return -eINVAL
	}
	image, errno := p.program(path)
	if errno != 0 {
		return errno
	}
	// The rest is done between two stretches of emulation and not inside this one: a new
	// process is a new engine, which is not to be made, or started, from within another's.
	abs, argv, env, parent, t, cwd := p.absolute(path), p.strings(a[3]), p.strings(a[4]), p.current, p.current, p.workingDirectory()
	// The group it is to be in: its parent's, or one it is told, which with 0 is one of its own.
	sigdefault, sigmask, group, ownGroup := uint32(0), parent.sigmask, p.group(), false
	if a[2] != 0 && flags&(spawnSetPgroup|spawnSetSigdef|spawnSetSigmask) != 0 {
		attr := p.read(p.u64(a[2]+8), 16)
		if flags&spawnSetPgroup != 0 {
			group = le.Uint32(attr[12:])
			ownGroup = group == 0
		}
		if flags&spawnSetSigdef != 0 {
			sigdefault = le.Uint32(attr[4:])
		}
		if flags&spawnSetSigmask != 0 {
			sigmask = le.Uint32(attr[8:])
		}
	}
	p.m.deferred = append(p.m.deferred, func() {
		child, errno := p.spawn(abs, cwd, image, argv, env, actions, flags&spawnCloexecDefault != 0, sigdefault, sigmask)
		var pid uint32
		if errno == 0 {
			pid = child.pid
			if child.pgroup = group; ownGroup {
				child.pgroup = 0
			}
		}
		if errno == 0 && a[0] != 0 {
			p.write(a[0], le32(pid))
		}
		t.answer = &result{x0: uint64(-errno), carry: errno != 0}
		t.wake()
	})
	p.wait(waitSpawn, 0, time.Time{}, result{})
	return 0
}

// spawn makes the process, or returns the error it could not be started for.
func (p *Process) spawn(abs, cwd string, image []byte, argv, env []string, actions []byte, closeAll bool, sigdefault, sigmask uint32) (*Process, int64) {
	child, err := p.m.newProcess(abs, image, abs)
	if err != nil {
		stopf("posix_spawn of %s: %v", abs, err)
	}
	child.parent, child.ppid, child.stdio = p, p.pid, p.stdio
	child.uid, child.gid, child.cwd = p.uid, p.gid, cwd // the directory of the thread that started it
	// What a new program keeps of its parent's signals: those it ignores, and those the thread
	// that started it blocks, unless it is told otherwise.
	for sig := 1; sig < numSignals; sig++ {
		if p.sigactions[sig].handler == sigIgnore && sigdefault>>(sig-1)&1 == 0 {
			child.sigactions[sig].handler = sigIgnore
		}
	}
	child.current.sigmask = sigmask
	if errno := child.inherit(p, actions, closeAll); errno != 0 {
		p.m.discard(child)
		return nil, errno
	}
	if err := child.load(argv, env); err != nil {
		p.m.discard(child)
		return nil, -eNOEXEC
	}
	p.note("  started %s as pid %d", abs, child.pid)
	return child, 0
}

// inherit gives a new process its parent's descriptors, does with them what it was told to, and
// closes those it is not to have.
func (p *Process) inherit(parent *Process, actions []byte, closeAll bool) int64 {
	p.fds = map[uint64]*fd{}
	for n, f := range parent.fds {
		switch {
		case f.kq != nil:
			continue // a kqueue is its process's alone
		case f.kind == hostFD:
			h, err := syscall.Dup(f.host)
			if err != nil {
				return hostErrno(err)
			}
			syscall.CloseOnExec(h)
			f = &fd{kind: hostFD, host: h, path: f.path}
		case f.kind == pipeFD:
			f.refs++
		case f.shared != nil:
			f.shared.retain()
		}
		p.fds[n] = f
		if closeAll || parent.cloexec[n] {
			p.cloexec[n] = true
		}
	}
	const open, closing, dup2, inheriting, chdir = 0, 1, 2, 3, 5
	for at := 8; at < len(actions); at += spawnActionSize {
		action := actions[at : at+spawnActionSize]
		kind, n := le.Uint32(action), uint64(le.Uint32(action[4:]))
		var errno int64
		switch kind {
		case open:
			flags, mode := uint64(le.Uint32(action[8:])), uint64(le.Uint16(action[12:]))
			path, _, _ := bytes.Cut(action[14:], []byte{0})
			opened := p.doOpen(string(path), flags, mode)
			if errno = min(opened, 0); errno == 0 && uint64(opened) != n {
				errno = min(p.dupFD(uint64(opened), n, true), 0)
				p.closeFD(uint64(opened))
			}
			delete(p.cloexec, n)
		case closing:
			errno = p.closeFD(n)
		case dup2:
			to := uint64(le.Uint32(action[8:]))
			if errno = min(p.dupFD(n, to, true), 0); errno == 0 {
				delete(p.cloexec, to)
			}
		case inheriting:
			if p.fds[n] == nil {
				errno = -eBADF
			}
			delete(p.cloexec, n)
		case chdir:
			path, _, _ := bytes.Cut(action[8:], []byte{0})
			errno = p.changeDirectory(string(path))
		default:
			parent.unimplemented(fmt.Sprintf("posix_spawn file action %d", kind), nil)
			errno = -eINVAL
		}
		if errno != 0 {
			return errno
		}
	}
	for _, n := range slices.Sorted(maps.Keys(p.cloexec)) {
		p.closeFD(n)
	}
	return 0
}

// discard takes back a process that was never started.
func (m *Machine) discard(p *Process) {
	for _, n := range slices.Sorted(maps.Keys(p.fds)) {
		p.closeFD(n)
	}
	m.procs = slices.DeleteFunc(m.procs, func(q *Process) bool { return q == p })
	m.replaced = append(m.replaced, p)
}

// children are the processes p started that it has not yet been told the end of.
func (p *Process) children() []*Process {
	var out []*Process
	for _, q := range p.m.procs {
		if q.parent == p && !q.reaped {
			out = append(out, q)
		}
	}
	return out
}

// sysWait4 is wait4(pid, &status, options, &usage): how a child ended, once one has. It waits
// for one to, unless told not to.
func sysWait4(p *Process, a []uint64) int64 {
	pid := int32(a[0])
	if pid < -1 {
		p.unimplemented("wait4 for a process group", a)
		return -eCHILD
	}
	if a[2]&^(waitNoHang|waitStopped) != 0 {
		p.unimplemented(fmt.Sprintf("wait4 with the options %#x", a[2]), a)
		return -eINVAL
	}
	w := &waiting4{pid: pid, status: a[1], rusage: a[3]}
	found := false
	for _, child := range p.children() {
		if !w.wants(child) {
			continue
		}
		if found = true; child.exited {
			return p.reapChild(child, w)
		}
	}
	switch {
	case !found:
		return -eCHILD
	case a[2]&waitNoHang != 0:
		return 0
	}
	p.current.waiting4 = w
	p.wait(waitChild, 0, time.Time{}, result{})
	return 0
}

func (w *waiting4) wants(child *Process) bool { return w.pid <= 0 || uint32(w.pid) == child.pid }

// reapChild tells a parent how a child ended, which is the last that is heard of it.
func (p *Process) reapChild(child *Process, w *waiting4) int64 {
	if w.status != 0 {
		p.write(w.status, le32(child.status()))
	}
	if w.rusage != 0 {
		p.write(w.rusage, child.usageBytes())
	}
	child.reaped = true
	return int64(child.pid)
}

// childExited gives the threads of a parent that wait for a child the one that has just ended.
func (m *Machine) childExited(child *Process) {
	parent := child.parent
	if parent == nil || parent.exited {
		return
	}
	for _, t := range parent.waiters(waitChild, 0) {
		if w := t.waiting4; w != nil && w.wants(child) && !child.reaped {
			t.waiting4 = nil
			t.answer = &result{x0: uint64(parent.reapChild(child, w))}
			t.wake()
		}
	}
}

func sysGetppid(p *Process, a []uint64) int64 { return int64(p.ppid) }

// sysExecve is execve(path, arguments, environment): the program takes the place of the one that
// called, as the same process: with its pid, its parent and children, and the descriptors it is
// to keep. It answers only if the program cannot be run.
func sysExecve(p *Process, a []uint64) int64 {
	image, errno := p.program(p.cstr(a[0]))
	if errno != 0 {
		return errno
	}
	if len(p.threads) > 1 && slices.ContainsFunc(p.threads, func(t *thread) bool { return t != p.current && t.state != dead }) {
		// A Mac's kernel refuses a process with other threads, in some cases, and ends them
		// in others.
		p.unimplemented("execve by a process with more than one thread", a)
		return -eNOTSUP
	}
	abs, argv, env, t := p.absolute(p.cstr(a[0])), p.strings(a[1]), p.strings(a[2]), p.current
	p.m.deferred = append(p.m.deferred, func() {
		if errno := p.exec(abs, image, argv, env); errno != 0 {
			t.answer = &result{x0: uint64(-errno), carry: true}
			t.wake()
		}
	})
	p.wait(waitSpawn, 0, time.Time{}, result{})
	return 0
}

// exec puts a new process in the place of p in the machine.
func (p *Process) exec(abs string, image []byte, argv, env []string) int64 {
	m := p.m
	at := slices.Index(m.procs, p)
	next, err := m.newProcess(abs, image, abs)
	if err != nil {
		stopf("execve of %s: %v", abs, err)
	}
	m.procs = m.procs[:len(m.procs)-1] // where newProcess put it
	next.pid, next.parent, next.ppid, next.stdio = p.pid, p.parent, p.ppid, p.stdio
	next.uid, next.gid, next.cwd, next.usage, next.early, next.pgroup = p.uid, p.gid, p.cwd, p.usage, p.early, p.pgroup
	for sig := 1; sig < numSignals; sig++ {
		if p.sigactions[sig].handler == sigIgnore {
			next.sigactions[sig].handler = sigIgnore
		}
	}
	next.current.sigmask = p.current.sigmask
	if err := next.load(argv, env); err != nil {
		m.replaced = append(m.replaced, next)
		return -eNOEXEC
	}
	// From here it is the new program's: the descriptors that are to be kept, the children,
	// the services. The old one's ports end, as a task's do, and its memory with its engine.
	for _, n := range slices.Sorted(maps.Keys(p.cloexec)) {
		p.closeFD(n)
	}
	next.fds, p.fds = p.fds, map[uint64]*fd{}
	for _, q := range m.procs {
		if q.parent == p {
			q.parent = next
		}
	}
	for _, s := range m.services {
		if s.provider == p {
			s.provider = next
		}
	}
	p.note("is now %s", abs)
	p.exited = true
	p.reap()
	m.procs[at] = next
	if m.running == p {
		m.running = next
	}
	// Its engine is closed with the machine's others, at the end: closing one while others run
	// takes memory of theirs with it.
	m.replaced = append(m.replaced, p)
	return 0
}

// group is the process group p is in: its own, as one launchd started is, unless it was started
// into another.
func (p *Process) group() uint32 {
	if p.pgroup != 0 {
		return p.pgroup
	}
	return p.pid
}

func sysGetpgrp(p *Process, a []uint64) int64 { return int64(p.group()) }

// sysGetpgid is the group of a process, by its pid, or with 0 of the caller.
func sysGetpgid(p *Process, a []uint64) int64 {
	if a[0] == 0 {
		return int64(p.group())
	}
	if q := p.m.process(uint32(a[0])); q != nil {
		return int64(q.group())
	}
	return -eSRCH
}

// sysSigreturn is sigreturn(context, style, token), which is how a handler returns to what its
// signal interrupted. No handler is ever run here, so the one call there can be is the one that
// only says the thread is no longer on its alternate signal stack (longjmp makes it): it never
// was.
func sysSigreturn(p *Process, a []uint64) int64 {
	const resetAltStack = 0x80000000 // UC_RESET_ALT_STACK
	if a[0] != 0 || uint32(a[1]) != resetAltStack {
		p.unimplemented("sigreturn from a signal handler", a)
		return -eINVAL
	}
	return 0
}

const (
	noteExit       = 0x80000000 // of a process filter: it ended, and
	noteExitStatus = 0x04000000 // how, as wait4 says it
)

// changeProc takes a change to a kqueue's interest in a process ending. A process that has
// ended, or that there never was, cannot be watched: the change is refused with ESRCH, as a
// Mac's kernel refuses it even of a child that has not been waited for.
func (p *Process) changeProc(q *kqueue, c kevent) {
	key := watchKey{c.ident, c.udata}
	switch {
	case c.flags&evDelete != 0:
		delete(q.procs, key)
	case c.flags&evAdd == 0:
	case c.fflags&^(noteExit|noteExitStatus) != 0 || c.fflags&noteExit == 0:
		p.unimplemented(fmt.Sprintf("a kqueue watching a process for more than its end (%#x)", c.fflags), nil)
	case p.m.process(uint32(c.ident)) == nil:
		refused := c
		refused.flags, refused.data = c.flags|evError, eSRCH // as it was asked, with the error
		q.errors = append(q.errors, refused)
	default:
		q.procs[key] = c
	}
}

// exitWatched tells every kqueue that watches a process that it has ended.
func (m *Machine) exitWatched(ended *Process) {
	for _, p := range m.procs {
		if p.exited {
			continue
		}
		for _, id := range slices.Sorted(maps.Keys(p.kqueues)) {
			q := p.kqueues[id]
			for _, key := range slices.SortedFunc(maps.Keys(q.procs), func(a, b watchKey) int { return cmp.Compare(a.udata, b.udata) }) {
				if w := q.procs[key]; key.ident == uint64(ended.pid) {
					event := w
					event.flags = w.flags&^evEnable | evEOF | evOneshot | evClear
					event.fflags, event.data = noteExit, 0
					if w.fflags&noteExitStatus != 0 {
						event.fflags |= noteExitStatus
						event.data = int64(ended.status())
					}
					q.ready = append(q.ready, event)
					delete(q.procs, key)
				}
			}
		}
	}
}
