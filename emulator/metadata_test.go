package emulator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		p.write(act+16, le64(1)) // it_value: fire in a second
		if _, _, stopped := p.trap(number(t, "setitimer"), 0, act, 0); !strings.Contains(stopped, "setitimer that starts a timer") {
			t.Errorf("arming a timer stopped with %q", stopped)
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
