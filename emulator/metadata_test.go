package emulator

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What a guest changes about a file in its home lands on the host file, and reads back.
func TestFileMetadata(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		path, name, value, buf := uint64(dataAddr), uint64(dataAddr+0x100), uint64(dataAddr+0x200), uint64(dataAddr+0x300)
		p.write(path, []byte(GuestHome+"/file\x00"))
		p.write(name, []byte("com.example.colour\x00"))
		p.write(value, []byte("blue"))
		call := func(what string, want int64, n string, args ...uint64) {
			t.Helper()
			x0, carry, stopped := p.trap(number(t, n), args...)
			got := int64(x0)
			if carry {
				got = -got
			}
			if got != want || stopped != "" {
				t.Errorf("%s: %s = %d stopped %q, want %d", what, n, got, stopped, want)
			}
		}
		fd := p.doOpen(GuestHome+"/file", 0x202, 0o644) // O_RDWR|O_CREAT
		host := filepath.Join(p.mounts[0].root.Name(), "file")

		call("mode", 0, "chmod", path, 0o600)
		if st, err := os.Stat(host); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("after chmod 600 the host file is %v (%v)", st.Mode(), err)
		}
		call("mode by descriptor", 0, "fchmod", uint64(fd), 0o640)
		call("owner unchanged", 0, "chown", path, uint64(p.m.user.uid), ^uint64(0))
		call("owner root", -ePERM, "chown", path, 0, 0)

		p.write(buf, append(le64(1_000_000_000, 0), le64(1_234_567_890, 0)...)) // two timevals
		call("dates", 0, "utimes", path, buf)
		if st, err := os.Stat(host); err != nil || st.ModTime().Unix() != 1_234_567_890 {
			t.Errorf("after utimes the host file was modified at %v (%v)", st.ModTime(), err)
		}

		// Extended attributes are kept by the machine, keyed by the host file.
		call("set", 0, "setxattr", path, name, value, 4, 0, 0)
		call("size", 4, "getxattr", path, name, 0, 0, 0, 0)
		call("short buffer", -eRANGE, "getxattr", path, name, buf, 2, 0, 0)
		call("read", 4, "getxattr", path, name, buf, 64, 0, 0)
		if got := string(p.read(buf, 4)); got != "blue" {
			t.Errorf("getxattr read %q", got)
		}
		call("create again", -eEXIST, "setxattr", path, name, value, 4, 0, xattrCreate)
		call("by descriptor", 4, "fgetxattr", uint64(fd), name, buf, 64, 0, 0)
		call("list", int64(len("com.example.colour\x00")), "listxattr", path, buf, 256, 0)
		call("unlink", 0, "unlink", path)
		// A new file is a new file, even where the host gives it the old one's inode.
		p.doOpen(GuestHome+"/file", 0x202, 0o644)
		call("gone with the file", -eNOATTR, "getxattr", path, name, buf, 64, 0, 0)
		call("replace what is not there", -eNOATTR, "setxattr", path, name, value, 4, 0, xattrReplace)
		call("remove", 0, "setxattr", path, name, value, 4, 0, 0)
		call("remove", 0, "removexattr", path, name, 0)
		call("list after remove", 0, "listxattr", path, buf, 256, 0)

		// Root gives a file away, and what root makes is root's, of its directory's group.
		owner := func(what string, at uint64, uid, gid uint32) {
			t.Helper()
			call(what, 0, "stat64", at, buf)
			if st := p.read(buf, 24); le.Uint32(st[16:]) != uid || le.Uint32(st[20:]) != gid {
				t.Errorf("%s: owned by %d:%d, want %d:%d", what, le.Uint32(st[16:]), le.Uint32(st[20:]), uid, gid)
			}
		}
		user := p.m.user
		p.write(path, []byte(GuestHome+"/dir\x00"))
		call("a directory", 0, "mkdir", path, 0o755)
		owner("the user's directory", path, user.uid, user.gid)
		p.uid = 0
		call("root gives it to a group", 0, "chown", path, ^uint64(0), 80)
		owner("given to a group", path, user.uid, 80)
		inner := uint64(dataAddr + 0x400)
		p.write(inner, []byte(GuestHome+"/dir/inner\x00"))
		call("root makes a directory in it", 0, "mkdir", inner, 0o755)
		owner("root's directory", inner, 0, 80)
		call("root gives it away", 0, "fchownat", ^uint64(1), inner, 7, 8, 0x20)
		owner("given away", inner, 7, 8)
		p.uid = user.uid
		call("its owner may not take it back", -ePERM, "chown", inner, uint64(user.uid), ^uint64(0))
		call("the directory removed", 0, "rmdir", inner)
		call("and made again", 0, "mkdir", inner, 0o755)
		owner("a new directory, the user's", inner, user.uid, 80)

		// Files that are not the guest's cannot be changed, and have no attributes.
		p.write(path, []byte("/dev/null\x00"))
		call("a device", -ePERM, "chmod", path, 0o600)
		call("a device's attribute", -eNOATTR, "getxattr", path, name, buf, 64, 0, 0)
	})
}

// flock locks the host file, so a second descriptor is kept out until the first lets go.
func TestFlock(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		first := uint64(p.doOpen(GuestHome+"/lock", 0x202, 0o644))
		second := uint64(p.doOpen(GuestHome+"/lock", 0, 0))
		flock := number(t, "flock")
		if x0, carry, _ := p.trap(flock, first, lockExclusive); carry {
			t.Errorf("flock exclusive: errno %d", x0)
			return
		}
		if x0, carry, _ := p.trap(flock, second, lockExclusive|lockNonblock); !carry || x0 != eAGAIN {
			t.Errorf("a second exclusive lock: x0 %d carry %v, want EWOULDBLOCK", x0, carry)
		}
		p.closeFD(first)
		if x0, carry, _ := p.trap(flock, second, lockExclusive|lockNonblock); carry {
			t.Errorf("once the first is closed: errno %d", x0)
		}
		if x0, carry, _ := p.trap(flock, 1, lockExclusive); !carry || x0 != eNOTSUP {
			t.Errorf("flock of standard output: x0 %d carry %v, want ENOTSUP", x0, carry)
		}
	})
}

// What a process says about signals reads back, and a timer that would fire stops the run.
func TestSignalState(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		act, old := uint64(dataAddr), uint64(dataAddr+0x100)
		// struct __sigaction: handler, trampoline, mask, flags (SA_RESTART and libc's SA_USERTRAMP).
		p.write(act, append(le64(0x1000, 0x2000), le32(1<<11, 0x402)...))
		if _, carry, _ := p.trap(number(t, "sigaction"), 30, act, 0); carry {
			t.Error("sigaction SIGUSR1 failed")
			return
		}
		p.trap(number(t, "sigaction"), 30, 0, old)
		if b := p.read(old, 16); le.Uint64(b) != 0x1000 || le.Uint32(b[8:]) != 1<<11 || le.Uint32(b[12:]) != 2 {
			t.Errorf("sigaction read back handler %#x mask %#x flags %#x", le.Uint64(b), le.Uint32(b[8:]), le.Uint32(b[12:]))
		}
		if x0, carry, _ := p.trap(number(t, "sigaction"), sigKill, act, 0); !carry || x0 != eINVAL {
			t.Errorf("a handler for SIGKILL: x0 %d carry %v", x0, carry)
		}

		p.write(act, le32(1<<29|1<<8)) // SIGUSR1 and SIGKILL
		p.trap(number(t, "sigprocmask"), 1, act, 0)
		p.trap(number(t, "sigprocmask"), 1, 0, old)
		if got := p.u32(old); got != 1<<29 {
			t.Errorf("blocked %#x, want SIGUSR1 alone", got)
		}

		p.write(act, make([]byte, 32)) // an itimerval that stops the timer
		if _, carry, stopped := p.trap(number(t, "setitimer"), 0, act, 0); carry || stopped != "" {
			t.Errorf("disarming a timer: carry %v stopped %q", carry, stopped)
		}
		// One that is started is read back with time left on it; the two that count the
		// process's own time cannot be started.
		p.write(act+16, le64(1)) // it_value: due in a second
		if _, carry, stopped := p.trap(number(t, "setitimer"), 0, act, old); carry || stopped != "" || p.u64(old+16) != 0 {
			t.Errorf("starting a timer: carry %v stopped %q, and one was running before: %d s", carry, stopped, p.u64(old+16))
		}
		p.trap(number(t, "getitimer"), 0, old)
		if left := time.Duration(p.u64(old+16))*time.Second + time.Duration(p.u32(old+24))*time.Microsecond; left <= 0 || left > time.Second {
			t.Errorf("a timer of a second has %v left", left)
		}
		if _, _, stopped := p.trap(number(t, "setitimer"), 1, act, 0); !strings.Contains(stopped, "setitimer that starts a timer") {
			t.Errorf("starting a timer of the process's own time stopped with %q", stopped)
		}
	})
}

// kevent registers what it is asked to and receipts it, and stops rather than wait for an event
// that would never come.
func TestKevent(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		kq, _, _ := p.trap(number(t, "kqueue"))
		change, events := uint64(dataAddr), uint64(dataAddr+0x100)
		// struct kevent: ident, filter (EVFILT_READ), flags (EV_ADD|EV_RECEIPT), fflags, data, udata.
		p.write(change, append(le64(0), le32(0xFFFF|uint32(evAdd|evReceipt)<<16, 0)...))
		p.write(change+16, make([]byte, 16))
		x0, carry, stopped := p.trap(number(t, "kevent"), kq, change, 1, events, 1, 0)
		if x0 != 1 || carry || stopped != "" {
			t.Errorf("a receipted change: x0 %d carry %v stopped %q", x0, carry, stopped)
			return
		}
		if flags := uint16(p.u32(events+8) >> 16); flags&evError == 0 || p.u64(events+16) != 0 {
			t.Errorf("receipt flags %#x data %d", flags, p.u64(events+16))
		}
		if _, _, stopped := p.trap(number(t, "kevent"), kq, 0, 0, events, 1, 0); !strings.Contains(stopped, "kevent that waits for events") {
			t.Errorf("waiting for an event stopped with %q", stopped)
		}
		if x0, carry, _ := p.trap(number(t, "kevent"), 99, 0, 0, 0, 0, 0); !carry || x0 != eBADF {
			t.Errorf("kevent on no kqueue: x0 %d carry %v", x0, carry)
		}
	})
}

// mprotect takes effect: a store to a page made read-only faults, and says why.
func TestMprotect(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		mmap, mprotect := number(t, "mmap"), number(t, "mprotect")
		addr, carry, _ := p.trap(mmap, 0, 2*pageSize, 3, 0x1002, ^uint64(0), 0) // MAP_ANON|MAP_PRIVATE
		if carry {
			t.Errorf("mmap: errno %d", addr)
			return
		}
		p.write(addr, []byte("x"))
		if x0, carry, _ := p.trap(mprotect, addr, pageSize, 1); carry {
			t.Errorf("mprotect read-only: errno %d", x0)
			return
		}
		if x0, carry, _ := p.trap(mprotect, addr+1, pageSize, 1); !carry || x0 != eINVAL {
			t.Errorf("unaligned: x0 %d carry %v", x0, carry)
		}
		if x0, carry, _ := p.trap(mprotect, nowhere, pageSize, 1); !carry || x0 != eNOMEM {
			t.Errorf("unmapped: x0 %d carry %v", x0, carry)
		}

		run := func(instruction uint32, at uint64) string {
			p.fault = ""
			p.write(codeAddr, le32(instruction))
			p.setX(0, 0)
			p.setX(1, at)
			if err := p.uc.start(codeAddr, codeAddr+4, 1000); err != nil && p.fault == "" {
				p.fault = err.Error()
			}
			return p.fault
		}
		const load, store = 0xF9400020, 0xF9000020 // ldr x0, [x1]; str x0, [x1]
		if fault := run(load, addr); fault != "" || p.x(0)&0xFF != 'x' {
			t.Errorf("reading the read-only page: fault %q x0 %#x", fault, p.x(0))
		}
		if fault := run(store, addr); !strings.Contains(fault, "write to protected memory") {
			t.Errorf("writing the read-only page: fault %q", fault)
		}
		if fault := run(store, addr+pageSize); fault != "" {
			t.Errorf("writing the page after it, still writable: fault %q", fault)
		}
		p.trap(mprotect, addr+pageSize, pageSize, 0)
		if fault := run(load, addr+pageSize); !strings.Contains(fault, "read of protected memory") {
			t.Errorf("reading a page with no access: fault %q", fault)
		}
		p.trap(mprotect, addr, 2*pageSize, 3)
		if fault := run(store, addr); fault != "" {
			t.Errorf("writing once writable again: fault %q", fault)
		}
	})
}

// result is what a system call returned as a C caller sees it: negative for an errno.
func (p *Process) result(t *testing.T, name string, args ...uint64) int64 {
	t.Helper()
	x0, carry, stopped := p.trap(number(t, name), args...)
	if stopped != "" {
		t.Errorf("%s stopped: %s", name, stopped)
	}
	if carry {
		return -int64(x0)
	}
	return int64(x0)
}

// Limits start as launchd gives them, change as the kernel lets a process change them, and the
// descriptors' is kept to.
func TestResourceLimits(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		limit := uint64(dataAddr)
		get := func(resource uint64) (uint64, uint64) {
			p.result(t, "getrlimit", resource, limit)
			return p.u64(limit), p.u64(limit + 8)
		}
		if soft, hard := get(rlimitNofile); soft != 256 || hard != rlimInfinity {
			t.Errorf("RLIMIT_NOFILE starts at %d, %#x", soft, hard)
		}
		if soft, hard := get(4); soft != 0 || hard != rlimInfinity {
			t.Errorf("RLIMIT_CORE starts at %d, %#x", soft, hard)
		}
		set := func(resource, soft, hard uint64) int64 {
			p.write(limit, le64(soft, hard))
			return p.result(t, "setrlimit", resource, limit)
		}
		if errno := set(rlimitNofile, 300, 100); errno != -eINVAL {
			t.Errorf("soft above hard: %d", errno)
		}
		if errno := set(99, 1, 1); errno != -eINVAL {
			t.Errorf("no such resource: %d", errno)
		}
		if errno := set(rlimitNofile, 5, 200); errno != 0 {
			t.Errorf("lowering: %d", errno)
		}
		if soft, hard := get(rlimitNofile); soft != 5 || hard != 200 {
			t.Errorf("read back %d, %d", soft, hard)
		}
		if errno := set(rlimitNofile, 5, 300); errno != -ePERM {
			t.Errorf("a user raising its hard limit: %d", errno)
		}
		// 0 to 2 are open: two more, and then no room.
		p.write(dataAddr+0x100, []byte(GuestHome+"/f\x00"))
		for i, want := range []int64{3, 4, -eMFILE} {
			if got := p.result(t, "open", dataAddr+0x100, 0x202, 0o644); got != want {
				t.Errorf("open %d with a limit of 5: %d, want %d", i, got, want)
			}
		}
		if got := p.result(t, "dup", 0); got != -eMFILE {
			t.Errorf("dup at the limit: %d", got)
		}
		if got := p.result(t, "dup2", 0, 9); got != -eBADF {
			t.Errorf("dup2 beyond the limit: %d", got)
		}
	})
}

// A file a guest creates has the mode it asks for less its umask, not the host's.
func TestUmask(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		if old := p.result(t, "umask", 0o077); old != 0o22 {
			t.Errorf("umask starts at %o", old)
		}
		if old := p.result(t, "umask", 0o002); old != 0o077 {
			t.Errorf("umask read back %o", old)
		}
		p.write(dataAddr, []byte(GuestHome+"/f\x00"))
		p.write(dataAddr+0x100, []byte(GuestHome+"/d\x00"))
		p.result(t, "open", dataAddr, 0x202, 0o666)
		p.result(t, "mkdir", dataAddr+0x100, 0o777)
		for name, want := range map[string]os.FileMode{"f": 0o664, "d": 0o775} {
			if st, err := os.Stat(filepath.Join(p.mounts[0].root.Name(), name)); err != nil || st.Mode().Perm() != want {
				t.Errorf("%s created as %v (%v), want %v", name, st.Mode().Perm(), err, want)
			}
		}
		p.result(t, "umask", 0o777)
		p.result(t, "open", dataAddr, 0x202, 0o666) // there already: its mode stays
		if st, _ := os.Stat(filepath.Join(p.mounts[0].root.Name(), "f")); st.Mode().Perm() != 0o664 {
			t.Errorf("opening an existing file changed its mode to %v", st.Mode().Perm())
		}
	})
}

// fsync and sync are the host's; what the guest cannot write has nothing to write.
func TestFsync(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		p.write(dataAddr, []byte(GuestHome+"/f\x00"))
		fd := p.result(t, "open", dataAddr, 0x202, 0o644)
		if got := p.result(t, "fsync", uint64(fd)); got != 0 {
			t.Errorf("fsync of a home file: %d", got)
		}
		if got := p.result(t, "fsync", 99); got != -eBADF {
			t.Errorf("fsync of no descriptor: %d", got)
		}
		if got := p.result(t, "sync"); got != 0 {
			t.Errorf("sync: %d", got)
		}
		kq, _, _ := p.trap(number(t, "kqueue"))
		if _, _, stopped := p.trap(number(t, "fsync"), kq); !strings.Contains(stopped, "fsync of shared memory or a kqueue") {
			t.Errorf("fsync of a kqueue stopped with %q", stopped)
		}
	})
}

// Nice values read back; a user may raise its own but not lower it again.
func TestPriority(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		if got := p.result(t, "getpriority", 0, 0); got != 0 {
			t.Errorf("nice starts at %d", got)
		}
		if got := p.result(t, "setpriority", 0, 0, 10); got != 0 {
			t.Errorf("raising nice: %d", got)
		}
		if got := p.result(t, "getpriority", 0, uint64(p.pid)); got != 10 {
			t.Errorf("nice read back %d", got)
		}
		if got := p.result(t, "setpriority", 0, 0, 5); got != -eACCES {
			t.Errorf("a user lowering nice: %d", got)
		}
		if got := p.result(t, "getpriority", 0, 12345); got != -eSRCH {
			t.Errorf("another process: %d", got)
		}
		if _, _, stopped := p.trap(number(t, "setpriority"), 3, 0, 0x1000); !strings.Contains(stopped, "Darwin kind") {
			t.Errorf("PRIO_DARWIN_THREAD stopped with %q", stopped)
		}
	})
}

// I/O policies read back by scope and type, and those that would change what files show stop.
func TestIOPolicy(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		param := uint64(dataAddr)
		p.write(param, le32(0, 0, 3)) // process scope, IOPOL_TYPE_DISK, IOPOL_THROTTLE
		if got := p.result(t, "iopolicysys", 2, param); got != 0 {
			t.Errorf("set: %d", got)
		}
		p.write(param, le32(0, 0, 0))
		p.result(t, "iopolicysys", 1, param)
		if got := p.u32(param + 8); got != 3 {
			t.Errorf("read back policy %d", got)
		}
		p.write(param, le32(0, 3, 0)) // IOPOL_TYPE_VFS_MATERIALIZE_DATALESS_FILES, never set
		p.result(t, "iopolicysys", 1, param)
		if got := p.u32(param + 8); got != 0 {
			t.Errorf("an unset policy reads %d", got)
		}
		p.write(param, le32(0, 8, 1)) // IOPOL_TYPE_VFS_SKIP_MTIME_UPDATE
		if _, _, stopped := p.trap(number(t, "iopolicysys"), 2, param); !strings.Contains(stopped, "changes what files show") {
			t.Errorf("skipping mtime updates stopped with %q", stopped)
		}
		p.result(t, "memorystatus_get_level", param)
		if got := p.u32(param); got != 50 {
			t.Errorf("memorystatus_get_level wrote %d", got)
		}
		if got := p.result(t, "setsid"); got != -ePERM {
			t.Errorf("setsid of a group leader: %d", got)
		}
	})
}

// __mac_syscall answers as examples/macsyscall.c measured a Mac answering a process that is not
// sandboxed, writes nothing it did not write, and stops on everything else.
func TestMacSyscall(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		strs, arg, out := uint64(dataAddr), uint64(dataAddr+0x100), uint64(dataAddr+0x200)
		p.write(strs, []byte("AMFI\x00Sandbox\x00syscall-unix\x00file-write-data\x00Quarantine\x00"))
		amfi, sandbox, op, write, quarantine := strs, strs+5, strs+13, strs+26, strs+42
		mac := func(policy uint64, call uint64, words ...uint64) int64 {
			p.write(arg, le64(append(words, make([]uint64, 8-len(words))...)...))
			p.write(out, le64(0xaaaaaaaaaaaaaaaa))
			return p.result(t, "__mac_syscall", policy, call, arg)
		}
		if got := mac(amfi, 90, 0, out); got != 0 || p.u64(out) != 0x1df {
			t.Errorf("AMFI 90: %d, flags %#x", got, p.u64(out))
		}
		pid := uint64(p.pid)
		plist := uint64(dataAddr + 0x300)
		p.write(plist, []byte(GuestHome+"/a.plist\x00"))
		fd := uint64(p.result(t, "open", plist, 0x202, 0o644))
		for _, c := range []struct {
			words  []uint64
			want   int64
			result uint64
		}{
			{[]uint64{out, pid}, 0, 0},
			{[]uint64{out, 1234}, 0, 0}, // even of a process that is not there
			{[]uint64{out, pid, op, 0x41, 0x226, 1}, 0, 0},
			{[]uint64{out, pid, 0, 0, 0, 0x40000001, 1}, 0, 0},
			{[]uint64{out, pid, write, 1, plist, 0x40000011, 1}, 0, 0}, // a path that is there
			{[]uint64{out, pid, write, 1, write, 1}, 0, 2},             // and one that is not
			{[]uint64{out, 1, write, 0xf0, 2, 0x20000005}, 0, 0},       // stderr is open
			{[]uint64{out, 1, write, 0xf0, 99, 0x20000005}, -eBADF, 0xaaaaaaaaaaaaaaaa},
		} {
			if got := mac(sandbox, 2, c.words...); got != c.want || p.u64(out) != c.result {
				t.Errorf("Sandbox 2 %#x: %d, result %#x", c.words[1:], got, p.u64(out))
			}
		}
		if got := mac(sandbox, 4, pid, 0, out, 8, 0, 1); got != -eNOTSUP || p.u64(out) != 0xaaaaaaaaaaaaaaaa {
			t.Errorf("Sandbox 4, the container: %d", got)
		}
		if got := mac(sandbox, 4, pid, 1, out, 8, 1); got != -eSRCH {
			t.Errorf("Sandbox 4, kind 1: %d", got)
		}
		for _, c := range []struct {
			fd, flags uint64
			want      int64
		}{{fd, 0x102, 0}, {fd, 0, 0}, {fd, 1, -eACCES}, {99, 0x102, -eBADF}} {
			if got := mac(sandbox, 12, c.fd, c.flags); got != c.want {
				t.Errorf("Sandbox 12 of descriptor %d, flags %#x: %d", c.fd, c.flags, got)
			}
		}
		if got := mac(sandbox, 22, op, out); got != -eNOENT {
			t.Errorf("Sandbox 22: %d", got)
		}
		if got := mac(sandbox, 0, out, 8, 0, 0); got != 0 {
			t.Errorf("Sandbox 0, a profile applied: %d", got)
		}
		if got := mac(quarantine, 87, 0, op, 0, out, out, 0, 0, 0); got != 0 || p.u64(out) != 0xaaaaaaaaaaaaaaaa {
			t.Errorf("Quarantine 87: %d", got)
		}
		if got := mac(sandbox, 67, out, out+8); got != 0 || p.u64(out) != 0 || p.u64(out+8) != 0 {
			t.Errorf("Sandbox 67: %d, %#x %#x", got, p.u64(out), p.u64(out+8))
		}
	})
	for _, c := range []struct {
		policy string
		call   uint64
		words  []uint64
		stop   string
	}{
		{"Sandbox", 1, nil, "__mac_syscall Sandbox 1"},
		{"Sandbox", 12, []uint64{1, 0x102}, "sandbox passthrough access of descriptor 1, flags 0x102"},
		{"Quarantine", 88, nil, "__mac_syscall Quarantine 88"},
		{"AMFI", 91, nil, "__mac_syscall AMFI 91"},
		{"Sandbox", 2, []uint64{0, 1, 0, 3}, "filter type 0x3"},
		{"Sandbox", 4, []uint64{1, 0}, "sandbox query 0 of pid 1"},
	} {
		p := newProcess(t)
		onMain(func() {
			p.write(dataAddr, []byte(c.policy+"\x00"))
			p.write(dataAddr+0x100, le64(append(c.words, make([]uint64, 8-len(c.words))...)...))
			if _, _, stopped := p.trap(number(t, "__mac_syscall"), dataAddr, c.call, dataAddr+0x100); !strings.Contains(stopped, c.stop) {
				t.Errorf("%s %d %#x stopped with %q", c.policy, c.call, c.words, stopped)
			}
		})
	}
}

// Who a process is logged in as, how many descriptors it may have, and becoming a user or a
// group, as macOS 27.2.1 answers a process that is not root.
func TestIdentityCalls(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const buf = dataAddr + 0x100
		p.write(buf, bytes.Repeat([]byte{'x'}, 300))
		if got, name := p.result(t, "getlogin", buf, 6), p.read(buf, 8); got != 0 || string(name) != "emu\x00\x00\x00xx" {
			t.Errorf("getlogin into 6 bytes: %d %q", got, name)
		}
		if got, name := p.result(t, "getlogin", buf, 300), p.read(buf, 300); got != 0 || name[254] != 0 || name[255] != 'x' {
			t.Errorf("getlogin into 300 bytes: %d, and byte 254 is %q and 255 %q", got, name[254], name[255])
		}
		if got := p.result(t, "getdtablesize"); got != 256 {
			t.Errorf("getdtablesize: %d", got)
		}
		for _, call := range []string{"setuid", "seteuid"} {
			if own, root, other := p.result(t, call, uint64(p.uid)), p.result(t, call, 0), p.result(t, call, uint64(p.uid+1)); own != 0 || root != -ePERM || other != -ePERM {
				t.Errorf("%s to itself %d, to root %d, to another %d", call, own, root, other)
			}
		}
		for _, call := range []string{"setgid", "setegid"} {
			if own, wheel, other := p.result(t, call, uint64(p.gid)), p.result(t, call, 0), p.result(t, call, uint64(p.gid+1000)); own != 0 || wheel != -ePERM || other != -ePERM {
				t.Errorf("%s to itself %d, to wheel %d, to another %d", call, own, wheel, other)
			}
		}
		// Root becoming another user would have two, which a process here has not.
		p.uid = 0
		if _, _, stopped := p.trap(number(t, "seteuid"), 501); stopped == "" {
			t.Errorf("root became another user")
		}
	})
}

// What the first guest writes is passed on as it writes it, if there is somewhere for it to go,
// and is otherwise kept, up to a ceiling.
func TestGuestOutput(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const buf = dataAddr + 0x100
		p.write(buf, []byte("ab\xffcd"))
		if got := p.result(t, "write", 1, buf, 5); got != 5 || p.Stdout() != "ab�cd" {
			t.Errorf("a write that is kept: %d, %q", got, p.Stdout())
		}
		var out, err bytes.Buffer
		p.m.cfg.Stdout, p.m.cfg.Stderr = &out, &err
		if one, two := p.result(t, "write", 1, buf, 5), p.result(t, "write", 2, buf, 2); one != 5 || two != 2 || out.String() != "ab\xffcd" || err.String() != "ab" || len(p.Stdout()) != 7 || p.Stderr() != "" {
			t.Errorf("writes that are passed on: %d and %d, %q and %q, with %q and %q kept", one, two, out.String(), err.String(), p.Stdout(), p.Stderr())
		}
		p.m.cfg.Stdout = nil
		p.stdout.WriteString(strings.Repeat("x", maxKeptOutput-8))
		if _, _, stopped := p.trap(number(t, "write"), 1, buf, 5); stopped == "" {
			t.Errorf("a guest wrote more than is kept, and went on")
		}
	})
}

// A pipe's two ends, in the calls of one thread; examples/pipes.c has the rest, against a Mac.
func TestPipe(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		const buf = dataAddr + 0x100
		r := uint64(p.result(t, "pipe"))
		w := p.x(1)
		if w != r+1 || p.fds[r] == nil || p.fds[w] == nil {
			t.Fatalf("pipe: %d and %d", r, w)
		}
		p.write(buf, []byte("hello"))
		if wrote, read := p.result(t, "write", w, buf, 5), p.result(t, "read", r, buf+16, 3); wrote != 5 || read != 3 || string(p.read(buf+16, 3)) != "hel" {
			t.Errorf("wrote %d and read %d: %q", wrote, read, p.read(buf+16, 3))
		}
		if wrong, seek := p.result(t, "read", w, buf, 1), p.result(t, "lseek", r, 0, 0); wrong != -eBADF || seek != -eSPIPE {
			t.Errorf("reading the writing end %d, seeking %d", wrong, seek)
		}
		if tty, closed := p.result(t, "ioctl", r, 0x40087468, buf), p.result(t, "ioctl", 99, ioctlAvailable, buf); tty != -eNOTTY || closed != -eBADF {
			t.Errorf("a terminal's request of a pipe %d, and any of no descriptor %d", tty, closed)
		}
		if p.result(t, "ioctl", r, ioctlAvailable, buf); p.u32(buf) != 2 {
			t.Errorf("%d to read", p.u32(buf))
		}
		// The writing end twice: the pipe ends for its reader when both are closed.
		again := uint64(p.result(t, "dup", w))
		p.result(t, "close", w)
		if p.fds[r].pipe.writers != 1 {
			t.Errorf("writers with a duplicate open: %d", p.fds[r].pipe.writers)
		}
		p.result(t, "close", again)
		if left, end := p.result(t, "read", r, buf, 16), p.result(t, "read", r, buf, 16); left != 2 || end != 0 {
			t.Errorf("what was left %d, then %d", left, end)
		}
	})
}

// What a process has used: its time on the CPU by where it spent it, and its faults.
func TestUsage(t *testing.T) {
	c := newClock()
	var u usage
	u.enter(c, true) // nobody has the CPU: nothing to account
	if u.user != 0 || u.system != 0 {
		t.Errorf("time with no thread on the CPU: %v, %v", u.user, u.system)
	}
	u.start(c)
	time.Sleep(2 * time.Millisecond)
	u.enter(c, true)
	time.Sleep(2 * time.Millisecond)
	u.enter(c, false)
	u.stop(c)
	user, system := u.user, u.system
	if user < 2*time.Millisecond || system < 2*time.Millisecond {
		t.Errorf("2 ms as the user and 2 ms in the kernel: %v, %v", user, system)
	}
	time.Sleep(2 * time.Millisecond)
	u.start(c)
	u.stop(c)
	if u.user-user > time.Millisecond || u.system != system {
		t.Errorf("time off the CPU was counted: %v, %v", u.user-user, u.system-system)
	}
	u.fault(false, 3)
	u.fault(true, 9)
	u.fault(false, 5)
	if u.minor != 2 || u.major != 1 || u.peak != 9 {
		t.Errorf("faults %d and %d, peak %d", u.minor, u.major, u.peak)
	}

	p := newProcess(t)
	onMain(func() {
		const buf = dataAddr + 0x100
		p.write(buf, bytes.Repeat([]byte{0xAA}, 144))
		if got := p.result(t, "getrusage", 0, buf); got != 0 || p.u64(buf+32) == 0 || p.u64(buf+32)%pageSize != 0 || p.u64(buf+40) != 0 || p.u64(buf+136) != 0 {
			t.Errorf("of itself: %d, % x", got, p.read(buf, 144))
		}
		if got := p.result(t, "getrusage", uint64(1<<32-1), buf); got != 0 || !bytes.Equal(p.read(buf, 144), make([]byte, 144)) {
			t.Errorf("of its children: %d, % x", got, p.read(buf, 144))
		}
		if other, nowhere := p.result(t, "getrusage", 1, buf), p.result(t, "getrusage", 0, nowhere); other != -eINVAL || nowhere != -eFAULT {
			t.Errorf("of something else %d, to nowhere %d", other, nowhere)
		}
	})
}

// The engine's numbers for the registers a signal handler's context holds beside the general
// ones: each is written here and read back by an instruction, and the other way round.
func TestVectorRegisters(t *testing.T) {
	p := newProcess(t)
	onMain(func() {
		run := func(instruction uint32) uint64 {
			p.write(codeAddr, le32(instruction))
			if err := p.uc.start(codeAddr, codeAddr+4, 10_000_000); err != nil {
				t.Fatal(err)
			}
			return p.x(0)
		}
		const fmovX0D0, umovX0V3High, mrsFPCR, mrsFPSR, msrFPCR, mrsNZCV = 0x9e660000, 0x4e183c60, 0xd53b4400, 0xd53b4420, 0xd51b4400, 0xd53b4200
		p.uc.setRegBytes(regV0, append(le64(0x1122334455667788), le64(0x99aabbccddeeff00)...))
		p.uc.setRegBytes(regV0+3, append(le64(1), le64(0x0123456789abcdef)...))
		if low, high := run(fmovX0D0), run(umovX0V3High); low != 0x1122334455667788 || high != 0x0123456789abcdef {
			t.Errorf("v0's low half %#x, v3's high half %#x", low, high)
		}
		p.uc.setReg(regFPCR, 0x00800000) // rounding towards minus infinity
		p.uc.setReg(regFPSR, 0x08000000) // a saturation happened
		if control, status := run(mrsFPCR), run(mrsFPSR); control != 0x00800000 || status != 0x08000000 {
			t.Errorf("fpcr %#x, fpsr %#x", control, status)
		}
		p.setX(0, 0x00400000)
		run(msrFPCR)
		back := make([]byte, 16)
		p.uc.regBytes(regV0, back)
		if got := p.uc.reg(regFPCR); got != 0x00400000 || le.Uint64(back[8:]) != 0x99aabbccddeeff00 {
			t.Errorf("fpcr set by the guest reads %#x; v0's high half %#x", got, le.Uint64(back[8:]))
		}
		p.uc.setReg(regNZCV, 0x60000000)
		if flags := run(mrsNZCV); flags != 0x60000000 {
			t.Errorf("flags %#x", flags)
		}
	})
}
