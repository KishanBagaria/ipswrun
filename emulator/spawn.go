package emulator

import (
	"bytes"
	"cmp"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/blacktop/go-macho"
)

// A process starts another with posix_spawn: a program of the build's, of the home or of a
// mount, or its own, as a process of its own on the machine, with the descriptors it is left by
// its parent and what it is told to do with them. The parent learns how it ended with wait4.
// What a Mac's kernel does was measured with examples/spawn.c. fork and execve, which make a
// process of a running one's memory or put a program in its place, are not here.

const (
	eCHILD        = 10
	eNOEXEC       = 8
	eBADARCH      = 86
	waitNoHang    = 0x1
	waitStopped   = 0x2  // WUNTRACED: of children that were stopped, which none here is
	waitContinued = 0x10 // WCONTINUED: and of those that went on again

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
func (p *Process) fileToRun(path string) ([]byte, int64) {
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
	return image, 0
}

// program is what is to be run for a path and the arguments it is given: the program at the
// path, or for a script, a file that begins with #!, the program its first line names, which is
// started with the words after it on that line, the script's path, and the script's arguments
// after its name. It answers with the program, its path, and what it is started with.
func (p *Process) program(path string, argv []string) ([]byte, string, []string, int64) {
	image, errno := p.fileToRun(path)
	if errno != 0 {
		return nil, "", nil, errno
	}
	if words, ok := scriptLine(image); ok {
		if len(words) == 0 {
			return nil, "", nil, -eNOEXEC
		}
		if image, errno = p.fileToRun(words[0]); errno != 0 {
			return nil, "", nil, errno
		}
		if _, nested := scriptLine(image); nested {
			p.unimplemented("a script whose program is a script", nil)
			return nil, "", nil, -eNOEXEC
		}
		argv = append(append(slices.Clone(words), path), argv[min(1, len(argv)):]...)
		path = words[0]
	}
	if _, err := macho.NewFatFile(bytes.NewReader(image)); err != nil {
		if _, err := macho.NewFile(bytes.NewReader(image)); err != nil {
			return nil, "", nil, -eNOEXEC
		}
	}
	if _, err := thin(image, 0, cpuSubtypeARM64E); err != nil {
		return nil, "", nil, -eBADARCH
	}
	return image, p.absolute(path), argv, 0
}

// scriptLine is the words of a script's first line after its #!, and whether the file is one.
func scriptLine(file []byte) ([]string, bool) {
	line, ok := bytes.CutPrefix(file[:min(len(file), 512)], []byte("#!"))
	if !ok {
		return nil, false
	}
	line, _, _ = bytes.Cut(line, []byte("\n"))
	return strings.Fields(string(line)), true
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
	image, abs, argv, errno := p.program(path, p.strings(a[3]))
	if errno != 0 {
		return errno
	}
	// The rest is done between two stretches of emulation and not inside this one: a new
	// process is a new engine, which is not to be made, or started, from within another's.
	env, parent, t, cwd := p.strings(a[4]), p.current, p.current, p.workingDirectory()
	// The group it is to be in: its parent's, or one it is told, which with 0 is one of its own.
	sigdefault, sigmask, group, ownGroup := uint32(0), parent.sigmask, p.group(), false
	if a[2] != 0 && flags&(spawnSetPgroup|spawnSetSigdef|spawnSetSigmask) != 0 {
		attr := p.read(p.u64(a[2]+8), 16)
		if flags&spawnSetPgroup != 0 {
			// As setpgid has it, and as measured (examples/spawngroup.c): a group below zero
			// is EINVAL, and one that no process of the session is in EPERM; nothing is started.
			group = le.Uint32(attr[12:])
			switch ownGroup = group == 0; {
			case int32(group) < 0:
				return -eINVAL
			case !ownGroup && !p.groupThere(group):
				return -ePERM
			}
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
	child.parent, child.ppid, child.execed = p, p.pid, true
	child.lineage = p.m.descend(p.lineage, child.uuid)
	child.cwd = cwd // the directory of the thread that started it
	p.handOn(child, false)
	// What a new program keeps of its parent's signals: those it ignores, and those the thread
	// that started it blocks, unless it is told otherwise.
	p.keepIgnored(child, sigdefault)
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
			var err error
			if f, err = f.hostDup(); err != nil {
				return hostErrno(err)
			}
		case f.streams():
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
	if a[2]&^(waitNoHang|waitStopped|waitContinued) != 0 {
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
	its := child.used()
	if w.rusage != 0 {
		p.write(w.rusage, its.bytes())
	}
	p.childrenUsed.add(its)
	p.childrenUsed.add(child.childrenUsed)
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

// sysGetppid is the process's parent, which is launchd once the one it came of has ended.
func sysGetppid(p *Process, a []uint64) int64 { return int64(p.parentPID()) }

func (p *Process) parentPID() uint32 {
	if p.parent != nil && p.parent.exited {
		return launchdPID
	}
	return p.ppid
}

// sysExecve is execve(path, arguments, environment): the program takes the place of the one that
// called, as the same process: with its pid, its parent and children, and the descriptors it is
// to keep. It answers only if the program cannot be run.
func sysExecve(p *Process, a []uint64) int64 {
	image, abs, argv, errno := p.program(p.cstr(a[0]), p.strings(a[1]))
	if errno != 0 {
		return errno
	}
	if len(p.threads) > 1 && slices.ContainsFunc(p.threads, func(t *thread) bool { return t != p.current && t.state != dead }) {
		// A Mac's kernel refuses a process with other threads, in some cases, and ends them
		// in others.
		p.unimplemented("execve by a process with more than one thread", a)
		return -eNOTSUP
	}
	env, t := p.strings(a[2]), p.current
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
	next.pid, next.parent, next.ppid, next.began = p.pid, p.parent, p.ppid, p.began
	next.lineage = lineage{uuid: next.uuid, unique: p.unique, parentUnique: p.parentUnique, version: m.moveOn(), parentVersion: p.parentVersion} // the same process, of a later version
	next.cwd, next.usage, next.early, next.pgroup, next.childrenUsed = p.cwd, p.usage, p.early, p.pgroup, p.childrenUsed
	next.charged = [2]time.Duration{p.usage.user, p.usage.system} // all of it by threads that are gone
	p.handOn(next, false)
	p.keepIgnored(next, 0)
	next.current.sigmask, next.itimer, next.execed = p.current.sigmask, p.itimer, true
	for _, s := range p.pending { // a signal that waits goes on waiting, for the one thread there now is
		if s.forThread(p.current) {
			s.to = nil
			next.pending = append(next.pending, s)
		}
	}
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

// handOn gives a process what every process that comes of p has of it, whether a copy (fork), a
// program p starts (posix_spawn) or the one p becomes (execve): who it is, where its output
// goes, the mask for the files it makes, its limits, its priority and its I/O policies, as a
// Mac's kernel was measured to hand them on (examples/inherit.c). The policies of single
// threads go with them only to a copy, which has the thread.
func (p *Process) handOn(to *Process, threads bool) {
	to.uid, to.gid, to.stdio, to.session = p.uid, p.gid, p.stdio, p.sessionOf()
	to.cmask, to.cmaskSet, to.nice = p.cmask, p.cmaskSet, p.nice
	to.rlimits = maps.Clone(p.rlimits)
	to.iopolicies = maps.Clone(p.iopolicies)
	if !threads {
		maps.DeleteFunc(to.iopolicies, func(key [2]uint32, _ uint32) bool { return key[0]>>16 != 0 })
	}
}

// keepIgnored has a new program ignore the signals the one that starts or becomes it ignores,
// but for those it is told to take as they come.
func (p *Process) keepIgnored(to *Process, sigdefault uint32) {
	for sig := 1; sig < numSignals; sig++ {
		if p.sigactions[sig].handler == sigIgnore && sigdefault>>(sig-1)&1 == 0 {
			to.sigactions[sig].handler = sigIgnore
		}
	}
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

// sessionOf is the session p is in: its own, as one launchd started is (setsid tells it so),
// or that of the process it came of.
func (p *Process) sessionOf() uint32 {
	if p.session != 0 {
		return p.session
	}
	return p.pid
}

// sysSetsid is setsid(): the process leaves for a session of its own, and a group of its own in
// it, unless there is a group of its pid already: its own, as one launchd started is in, or one
// it was the first of and left with others in it.
func sysSetsid(p *Process, a []uint64) int64 {
	if len(p.m.inGroup(p.pid)) != 0 {
		return -ePERM
	}
	p.session, p.pgroup = 0, 0
	return int64(p.pid)
}

// sysSetpgid is setpgid(pid, group): the caller (0) or a copy of it that is still the program
// it was goes into a group of its own (0, or its pid) or into another of its session. A child
// that has become or was started as another program is EACCES, and any other process ESRCH. One
// that leads its session stays in its group (EPERM), and a group there is not in the session is
// EPERM too.
func sysSetpgid(p *Process, a []uint64) int64 {
	pid, group := uint32(a[0]), int32(a[1])
	of := p
	if pid != 0 && pid != p.pid {
		if of = p.m.process(pid); of == nil || of.parent != p {
			return -eSRCH
		}
		if of.execed {
			return -eACCES
		}
	}
	to := uint32(group)
	if to == 0 {
		to = of.pid
	}
	switch {
	case group < 0:
		return -eINVAL
	case of.sessionOf() == of.pid || of.sessionOf() != p.sessionOf():
		return -ePERM
	case to != of.pid && !of.groupThere(to):
		return -ePERM
	}
	if of.pgroup = to; to == of.pid {
		of.pgroup = 0
	}
	return 0
}

// groupThere is whether a group is one a process may go into: one a process of its session is in.
func (p *Process) groupThere(group uint32) bool {
	return slices.ContainsFunc(p.m.live(), func(q *Process) bool { return q.group() == group && q.sessionOf() == p.sessionOf() })
}

// sysGetsid is the session of a process, by its pid, or with 0 of the caller. launchd's is its
// own.
func sysGetsid(p *Process, a []uint64) int64 {
	pid := uint32(a[0])
	if pid == 0 {
		return int64(p.sessionOf())
	}
	if pid == launchdPID {
		return launchdPID
	}
	if q := p.m.process(pid); q != nil {
		return int64(q.sessionOf())
	}
	return -eSRCH
}

// sysGetpgid is the group of a process, by its pid, or with 0 of the caller.
func sysGetpgid(p *Process, a []uint64) int64 {
	if a[0] == 0 {
		return int64(p.group())
	}
	if uint32(a[0]) == launchdPID { // its own, as its record says
		return launchdPID
	}
	if q := p.m.process(uint32(a[0])); q != nil {
		return int64(q.group())
	}
	return -eSRCH
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

// sysFork is fork(): a copy of the process, which goes on from the same place as the thread that
// called, with memory of its own that starts as the parent's, the parent's descriptors and what
// it has said of signals, and no other of its threads, ports, timers or locks. The parent is
// answered with the copy's pid, and the copy is told that it is the copy.
func sysFork(p *Process, a []uint64) int64 {
	t := p.current
	p.m.deferred = append(p.m.deferred, func() {
		child := p.fork()
		p.setX(1, 0)
		t.answer = &result{x0: uint64(child.pid)}
		t.wake()
	})
	p.wait(waitSpawn, 0, time.Time{}, result{})
	return 0
}

// fork makes the copy. It is made between two stretches of emulation, with the thread that
// called still on the parent's engine, where its registers are read from.
func (p *Process) fork() *Process {
	m := p.m
	child, err := m.newProcess(p.exePath, p.exe, p.path)
	if err != nil {
		stopf("fork: %v", err)
	}
	child.parent, child.ppid, child.pgroup = p, p.pid, p.group()
	child.lineage = m.descend(p.lineage, p.uuid)
	child.cwd, child.brk = p.workingDirectory(), p.brk
	child.identity, child.entitlements, child.entitlementsDER = p.identity, p.entitlements, p.entitlementsDER
	child.started, child.argc = p.started, p.argc // a copy has its parent's stack
	p.handOn(child, true)
	child.sigactions = p.sigactions
	child.threadStart, child.wqThread, child.pthreadSize, child.tsdOffset, child.portOffset = p.threadStart, p.wqThread, p.pthreadSize, p.tsdOffset, p.portOffset
	child.inodes = maps.Clone(p.inodes)
	child.slide, child.slideSet, child.imageInfo, child.dit = p.slide, p.slideSet, p.imageInfo, p.dit

	// Its memory: what a new process has that is the kernel's (the commpage, the tables that
	// say where memory is) stays, and the rest is the parent's, a copy of what the parent has
	// touched and, of what it has not, the same to be read on first touch.
	kernels := func(r *region) bool { return r.name == "commpage" || r.name == "pagetables" }
	for _, r := range slices.Clone(child.regions) {
		if !kernels(r) {
			child.unmap(r.start, r.end)
		}
	}
	for _, r := range p.regions {
		if !kernels(r) {
			copied := *r
			child.addRegion(&copied)
		}
	}
	// A run of pages that follow one another in one region is copied at once: the engine takes
	// long over each mapping it is given, and a process has touched thousands of pages.
	pages := slices.Sorted(maps.Keys(p.mapped))
	for i := 0; i < len(pages); {
		start := pages[i] * pageSize
		r := p.findRegion(start)
		if r == nil {
			stopf("fork: the page at %#x is in no region", start)
		}
		j := i + 1
		for j < len(pages) && pages[j] == pages[j-1]+1 && pages[j]*pageSize < r.end {
			j++
		}
		run, size := pages[i:j], uint64(j-i)*pageSize
		i = j
		if kernels(r) || r.kind == sharedRegion {
			continue // a shared one is the same memory in both, mapped when the copy first touches it
		}
		for _, page := range run {
			child.mapped[page] = struct{}{}
		}
		data, err := p.uc.memRead(start, int(size))
		if err != nil {
			stopf("fork: reading the pages at %#x: %v", start, err)
		}
		child.uc.memMap(start, size, r.prot())
		child.uc.memWrite(start, data)
	}

	// Its one thread: the registers of the one that called, the vector ones too, and what the
	// kernel keeps of a thread.
	from, to := p.current, child.current
	for i := uint32(0); i < 31; i++ {
		child.setXR(i, p.xr(i))
	}
	for _, id := range []int{regSP, regNZCV, regFPCR, regFPSR, regTPIDRRO} {
		child.uc.setReg(id, p.uc.reg(id))
	}
	vector := make([]byte, 16)
	for i := 0; i < 32; i++ {
		p.uc.regBytes(regV0+i, vector)
		child.uc.setRegBytes(regV0+i, vector)
	}
	to.pc, to.tsd, to.pthread = from.pc, from.tsd, from.pthread // its port is its own: its parent's name for the thread is nothing to it
	to.sigmask, to.altstack, to.onstack, to.cwd = from.sigmask, from.altstack, from.onstack, from.cwd
	child.uc.setReg(regPC, to.pc)
	child.setX(0, uint64(p.pid))
	child.setX(1, 1) // it is the copy
	child.carry(false)

	// Its descriptors: the parent's, each still marked as it is. A kqueue is not a copy's.
	child.fds = map[uint64]*fd{}
	for n, f := range p.fds {
		switch {
		case f.kq != nil:
			continue
		case f.kind == hostFD:
			var err error
			if f, err = f.hostDup(); err != nil {
				stopf("fork: %v", err)
			}
		case f.streams():
			f.refs++
		case f.shared != nil:
			f.shared.retain()
		}
		child.fds[n] = f
		if p.cloexec[n] {
			child.cloexec[n] = true
		}
	}
	p.note("  forked, as pid %d", child.pid)
	return child
}
