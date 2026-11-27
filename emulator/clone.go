package emulator

import (
	"io"
	"os"
	"slices"
	"syscall"
	"time"
)

// A copy of a file made by the file system, as ditto and Finder make one. On a Mac's the copy
// shares the first's blocks until one of them is written to, which nothing here can tell from a
// copy of its bytes, so that is what is made: a file of its own with what the first held, and its
// mode, its date of modification, its attributes and its owner, as measured (examples/clone.c).

const (
	cloneNoFollow    = 0x1  // CLONE_NOFOLLOW: of a symbolic link, the link
	cloneNoOwnerCopy = 0x2  // CLONE_NOOWNERCOPY: the copy is whose makes it
	cloneACL         = 0x4  // CLONE_ACL: with the access control list, which no file here has
	cloneNoFollowAny = 0x8  // CLONE_NOFOLLOW_ANY: following no link anywhere in the path
	cloneBeneath     = 0x10 // CLONE_RESOLVE_BENEATH: staying beneath the directory
)

// clone makes to as a copy of from.
func (p *Process) clone(from, to string, flags uint64, a []uint64) int64 {
	switch {
	case flags&^(cloneNoFollow|cloneNoOwnerCopy|cloneACL|cloneNoFollowAny|cloneBeneath) != 0:
		return -eINVAL
	case flags&(cloneNoFollowAny|cloneBeneath) != 0:
		p.unimplemented("clonefile told to follow no link in the path, or to keep beneath a directory", a)
		return -eNOSYS
	}
	t1, ok1 := p.resolve(from)
	t2, ok2 := p.writableTarget(to)
	switch {
	case !ok1:
		return -eNOENT
	case !ok2:
		return -eACCES
	case !sameVolume(t1, t2): // another volume, which the system's is of the home's too
		return -eXDEV
	}
	var st syscall.Stat_t
	if err := statInRoot(t1.root, t1.rel, flags&cloneNoFollow == 0, &st); err != nil {
		return hostErrno(err)
	}
	id := hostFileOf(&st)
	switch {
	case st.Mode&syscall.S_IFMT == syscall.S_IFLNK:
		text, err := t1.root.Readlink(t1.rel)
		if err != nil {
			return hostErrno(err)
		}
		return p.doSymlink(text, to)
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG || p.m.fifos[id]:
		p.unimplemented("clonefile of what is not a file or a symbolic link", a)
		return -eNOSYS
	}
	in, err := t1.root.Open(t1.rel)
	if err != nil {
		return hostErrno(err)
	}
	defer in.Close()
	out, err := t2.root.OpenFile(t2.rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return hostErrno(err)
	}
	// The copying is the host's work, which a Mac's file system does not have: not the guest's time.
	made, err := away(p.m.clock, func() (syscall.Stat_t, error) { return copyFile(in, out, &st) })
	if err == nil {
		err = t2.root.Chtimes(t2.rel, time.Time{}, time.Unix(mtimeSeconds(&st), 0))
	}
	if err != nil {
		t2.root.Remove(t2.rel)
		return hostErrno(err)
	}
	copied := hostFileOf(&made)
	if p.m.acls[id] != nil {
		// The copy's list is the original's entries and then what its directory hands down, by
		// the kernel's source; it was not measured.
		p.unimplemented("a copy of a file that has an access control list (clonefile)", a)
	}
	if attrs := p.m.xattrs[id]; len(attrs) != 0 {
		p.m.xattrs[copied] = map[string][]byte{}
		for name, value := range attrs {
			p.m.xattrs[copied][name] = slices.Clone(value)
		}
	}
	p.created(t2)
	if flags&cloneNoOwnerCopy == 0 {
		p.m.own(copied, p.m.owned(id))
	}
	return 0
}

// copyFile fills a new file with what another holds, gives it the other's mode and closes it,
// and says what file of the host's it is.
func copyFile(in, out *os.File, st *syscall.Stat_t) (made syscall.Stat_t, err error) {
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return made, err
	}
	if err = out.Chmod(fileMode(uint64(st.Mode))); err != nil {
		return made, err
	}
	return made, syscall.Fstat(int(out.Fd()), &made)
}

// sysClonefileat is clonefileat(directory, path, directory, path, flags), which clonefile is too.
func sysClonefileat(p *Process, a []uint64) int64 {
	return p.atPath(a[0], a[1], func(from string) int64 {
		return p.atPath(a[2], a[3], func(to string) int64 { return p.clone(from, to, a[4], a) })
	})
}

// sysFclonefileat is fclonefileat(open file, directory, path, flags).
func sysFclonefileat(p *Process, a []uint64) int64 {
	f := p.fds[a[0]]
	if f == nil {
		return -eBADF
	}
	if f.kind != hostFD || f.path == "" {
		p.unimplemented("fclonefileat of what is not a file of the home's or a mount's", a)
		return -eNOSYS
	}
	return p.atPath(a[1], a[2], func(to string) int64 { return p.clone(f.path, to, a[3]&^cloneNoFollow, a) })
}
