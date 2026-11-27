package emulator

import (
	"path/filepath"
	"syscall"
)

// A file's access control list: the entries that say who may and may not do what beyond its
// mode. One is kept for a file under the home or a mount, as chmod_extended and fchmod_extended
// set it, said back by the stat calls that tell of a file's security, and handed down to what
// is made in a directory that has one, as the kernel's source has it (kauth_acl_inherit) and as
// measured on macOS 27.2.1. Nobody is refused by one: who may do what is the host's to say. A
// list is kept in memory, so not for the next run of a data volume that is kept.
const (
	filesecMagic  = 0x012CC16D // what a file's security record starts with
	filesecNoACL  = 0xFFFFFFFF // the count of a record that has no list
	filesecHeader = 44         // the mark, two identifiers of 16 bytes, the count and the list's flags
	aceSize       = 24         // an entry: whom it names (16 bytes), its flags, and what it is of
	aclMost       = 128        // entries a list may have

	aceInherited    = 1 << 4 // it was handed down
	aceFileInherit  = 1 << 5 // handed down to files
	aceDirInherit   = 1 << 6 // and to directories
	aceLimitInherit = 1 << 7 // but no further
	aceOnlyInherit  = 1 << 8 // and is not of the directory itself
	aceInheritFlags = aceFileInherit | aceDirInherit | aceLimitInherit | aceOnlyInherit
)

// acl is a list's flags and its entries, each as the kernel has it.
type acl struct {
	flags   uint32
	entries [][]byte
}

// bytes is the list as it is in a security record, after the identifiers.
func (l *acl) bytes() []byte {
	out := le32(uint32(len(l.entries)), l.flags)
	for _, e := range l.entries {
		out = append(out, e...)
	}
	return out
}

// readFilesec reads the security a call gives for a file: nothing (0), that its list is to be
// taken off (1), or a record with a list. A record that does not start as one does, or has more
// entries than a list may, is EINVAL. A record that says it has no list is taken, as measured
// of a file that has none; what it does to a file that has one was not measured (none says
// whether the list stays), so it is told apart for the caller to stop there.
func (p *Process) readFilesec(addr uint64) (list *acl, remove, noList bool, errno int64) {
	switch addr {
	case 0:
		return nil, false, false, 0
	case 1:
		return nil, true, false, 0
	}
	head := p.read(addr, filesecHeader)
	count := le.Uint32(head[36:])
	switch {
	case le.Uint32(head) != filesecMagic:
		return nil, false, false, -eINVAL
	case count == filesecNoACL:
		return nil, false, true, 0
	case count > aclMost:
		return nil, false, false, -eINVAL
	}
	list = &acl{flags: le.Uint32(head[40:])}
	entries := p.read(addr+filesecHeader, uint64(count)*aceSize)
	for i := uint32(0); i < count; i++ {
		list.entries = append(list.entries, entries[i*aceSize:(i+1)*aceSize])
	}
	return list, false, false, 0
}

// setACL gives a host file a list, or takes its list off: a list of no entries is none, as a
// Mac says of a file that was given one.
func (m *Machine) setACL(id hostFile, list *acl) {
	if list == nil || len(list.entries) == 0 {
		delete(m.acls, id)
	} else {
		m.acls[id] = list
	}
}

// filesec is the security record of a file: with its list if it has one, and saying that it has
// none if not. The identifiers of its owner and group are none, as the source has them for a
// file system that keeps none.
func (m *Machine) filesec(id hostFile, known bool) []byte {
	head := append(le32(filesecMagic), make([]byte, 32)...)
	if list := m.acls[id]; known && list != nil {
		return append(head, list.bytes()...)
	}
	return append(head, le32(filesecNoACL, 0)...)
}

// inheritACL gives what was just made in a directory the entries of the directory's list that are
// handed down to its kind: marked as handed down, no longer for handing down alone, and not to
// be handed down again if they are a file's or limited to one step.
func (p *Process) inheritACL(t target, st *syscall.Stat_t) {
	var dir syscall.Stat_t
	if st.Mode&syscall.S_IFMT == syscall.S_IFLNK { // from the source, not measured: a link is made without what is handed down
		return
	}
	if statInRoot(t.root, filepath.Dir(t.rel), true, &dir) != nil {
		return
	}
	from := p.m.acls[hostFileOf(&dir)]
	if from == nil {
		return
	}
	isDir := st.Mode&syscall.S_IFMT == syscall.S_IFDIR
	want := uint32(aceFileInherit)
	if isDir {
		want = aceDirInherit
	}
	list := &acl{}
	for _, e := range from.entries {
		flags := le.Uint32(e[16:])
		if flags&want == 0 {
			continue
		}
		flags = flags&^aceOnlyInherit | aceInherited
		if flags&aceLimitInherit != 0 || !isDir {
			flags &^= aceInheritFlags
		}
		entry := append([]byte(nil), e...)
		le.PutUint32(entry[16:], flags)
		list.entries = append(list.entries, entry)
	}
	p.m.setACL(hostFileOf(st), list)
}
