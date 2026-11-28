package emulator

import (
	"bufio"
	"cmp"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// A device keeps what changes on a volume of its own, beside the sealed one the system is on:
// the data volume. A macOS release says which paths are that volume's (/usr/share/firmlinks:
// /Library, /private, /Users and more) and what a newly installed Mac's has in it
// (/System/Library/Templates/Data). A run that is given a directory for one (Run.Data) has
// those paths as mounts into it, each read through to the template: a directory of the volume
// is given what the template has in it, once, the first time a path leads into it. From then on
// it is the host's directory, and what a guest makes, changes or removes there stays so, for the
// next run too.

// TemplateEntry is what the template of a data volume has at a path.
type TemplateEntry struct {
	Dir   bool
	Names []string // of a directory: what is in it
	Link  string   // of a symbolic link: its text
	// Of a file: its size and what it holds.
	Size int64
	Read func() ([]byte, error)
	// Inode is who owns it and what they may do, as the template has it.
	Inode *Inode
}

// maxTemplateFiles is how much the files of one directory of a template may come to. They are
// copied when the directory is first entered, whether or not a guest reads them.
const maxTemplateFiles = 64 << 20

// populate gives the directories on the way to a path under a mount, and the path itself if it
// is one, what the template has in them, where that has not been done before.
func (m *Machine) populate(mt *mounted, rel string) {
	if mt.template == nil {
		return
	}
	dir := "."
	var rest []string
	if rel != "." {
		rest = strings.Split(rel, "/")
	}
	for {
		if !mt.populated[dir] {
			m.fill(mt, dir)
		}
		if len(rest) == 0 {
			return
		}
		dir, rest = pathpkg.Join(dir, rest[0]), rest[1:]
	}
}

// fill copies into a directory of a mount what the template has in it and it has not.
func (m *Machine) fill(mt *mounted, dir string) {
	if st, err := mt.root.Lstat(dir); err != nil || !st.IsDir() {
		return // nothing to fill, or not yet
	}
	failed := func(what string, err error) {
		stopf("the data volume: %s %s: %v", what, pathpkg.Join(mt.guest, dir), err)
	}
	entry, err := away(m.clock, func() (TemplateEntry, error) { return mt.template(dir) })
	if err != nil {
		failed("reading the template of", err)
	}
	if dir == "." {
		m.inherit(mt, dir, entry.Inode) // the path of the volume itself, which was made for the mount
	}
	var files int64
	for _, name := range entry.Names {
		child := pathpkg.Join(dir, name)
		if _, err := mt.root.Lstat(child); err == nil {
			continue
		}
		e, err := away(m.clock, func() (TemplateEntry, error) { return mt.template(child) })
		switch {
		case err != nil:
		case e.Dir:
			if err = mt.root.Mkdir(child, 0o755); err == nil {
				m.inherit(mt, child, e.Inode)
			}
		case e.Link != "":
			if err = mt.root.Symlink(e.Link, child); err == nil {
				m.inherit(mt, child, e.Inode)
			}
		case e.Read != nil:
			if files += e.Size; files > maxTemplateFiles {
				stopf("the data volume: the files the release's template has in %s come to more than %d MB, and a directory's are copied when it is first entered", pathpkg.Join(mt.guest, dir), maxTemplateFiles>>20)
			}
			var data []byte
			if data, err = away(m.clock, e.Read); err == nil {
				err = mt.root.WriteFile(child, data, 0o600)
			}
			if err == nil {
				m.inherit(mt, child, e.Inode)
			}
		}
		if err != nil {
			failed("filling", err)
		}
	}
	mt.populated[dir] = true
	if mt.record != nil {
		if _, err := mt.record.WriteString(mt.guest + "\t" + dir + "\n"); err != nil {
			failed("recording", err)
		}
	}
}

// kept is what the template says of a file of the data volume that the host's file cannot say
// for it: whom it belongs to and, where they differ from the host file's, its permissions (mode
// is then set), with the host's number for the file it was said of, as a file a guest has since
// put in its place is its own.
type kept struct {
	Owner
	mode uint16
	said bool
	ino  uint64
}

// ownerLine starts a line of a mount's record that says what kept does of a path; the others
// name a directory that was filled.
const ownerLine = "owner"

// inherit gives what was just made from the template the owner and permissions the template
// has for it. They are said to whoever asks and refuse nobody: the host's file is left for the
// host's user to read and write (and a directory to fill), with the template's other permission
// bits where it is not a link, and what the template says beyond that is kept beside it, and in
// the record for the next run.
func (m *Machine) inherit(mt *mounted, rel string, inode *Inode) {
	if inode == nil {
		return
	}
	failed := func(what string, err error) {
		stopf("the data volume: %s %s: %v", what, pathpkg.Join(mt.guest, rel), err)
	}
	fi, err := mt.root.Lstat(rel)
	if err != nil {
		failed("looking at", err)
	}
	k := kept{Owner: Owner{inode.UID, inode.GID}, mode: inode.Mode & 0o7777}
	if fi.Mode()&os.ModeSymlink == 0 {
		host := os.FileMode(k.mode&0o777) | 0o600
		if fi.IsDir() {
			host |= 0o700
		}
		if err := mt.root.Chmod(rel, host); err != nil {
			failed("setting the mode of", err)
		}
		k.said = uint16(host) != k.mode
	}
	var st syscall.Stat_t
	if err := statInRoot(mt.root, rel, false, &st); err != nil {
		failed("looking at", err)
	}
	k.ino = uint64(st.Ino)
	m.keep(hostFileOf(&st), k)
	if mt.record != nil && (k.said || k.Owner != (Owner{m.user.uid, m.user.gid})) {
		said := "-"
		if k.said {
			said = strconv.FormatUint(uint64(k.mode), 8)
		}
		line := strings.Join([]string{ownerLine, mt.guest, strconv.FormatUint(uint64(k.UID), 10), strconv.FormatUint(uint64(k.GID), 10), said, strconv.FormatUint(k.ino, 10), rel}, "\t")
		if _, err := mt.record.WriteString(line + "\n"); err != nil {
			failed("recording the owner of", err)
		}
	}
}

// keep has a host file be said to be whose and as the template has it.
func (m *Machine) keep(id hostFile, k kept) {
	m.own(id, k.Owner)
	if k.said {
		m.said[id] = k.mode
		m.changed()
	}
}

// claim says again what the template says of the files of a volume kept from an earlier run. A
// file is known by the host's number for it, which a copy of the volume does not keep: what was
// copied is then the user's, as every file of the host's is that nothing more is known of.
func (m *Machine) claim(mt *mounted) {
	for rel, k := range mt.kept {
		var st syscall.Stat_t
		if statInRoot(mt.root, rel, false, &st) == nil && uint64(st.Ino) == k.ino {
			m.keep(hostFileOf(&st), k)
		}
	}
}

// openRecord reads which directories of a mount were filled in earlier runs and whom what they
// were filled with belongs to, from the file that is kept of that, and opens it to add to.
func (mt *mounted) openRecord(path string) error {
	mt.populated = map[string]bool{}
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	mt.kept = map[string]kept{}
	for lines := bufio.NewScanner(f); lines.Scan(); {
		// What an owner's line says comes before the path, which may have anything in it.
		if fields := strings.SplitN(lines.Text(), "\t", 7); len(fields) == 7 && fields[0] == ownerLine {
			uid, err1 := strconv.ParseUint(fields[2], 10, 32)
			gid, err2 := strconv.ParseUint(fields[3], 10, 32)
			ino, err3 := strconv.ParseUint(fields[5], 10, 64)
			k := kept{Owner: Owner{uint32(uid), uint32(gid)}, ino: ino}
			if fields[4] != "-" {
				mode, err := strconv.ParseUint(fields[4], 8, 16)
				k.mode, k.said, err1 = uint16(mode), true, cmp.Or(err1, err)
			}
			if fields[1] == mt.guest && err1 == nil && err2 == nil && err3 == nil {
				mt.kept[fields[6]] = k
			}
		} else if guest, dir, ok := strings.Cut(lines.Text(), "\t"); ok && guest == mt.guest {
			mt.populated[dir] = true
		}
	}
	mt.record = f
	return nil
}

// rootFolders are the per-user folders of root, which every Mac that has started has (measured
// on macOS 27.2.1 and on 27.2 in a virtual machine, where they were made in the first second of
// the first start, and are root's and wheel's): the folder itself, named as Libc names it from
// the user's number and identifier, which for root is the same on every Mac, and in it what is
// kept for the user, its caches, its temporary files and one more, each marked as dirhelper marks
// a folder it makes (see sandboxStorageClass). A release's template has
// /private/var/folders empty, as a Mac's is before it has first started.
var rootFolders = []struct {
	rel  string
	mode uint16
}{
	{"var/folders/zz", 0o755}, {"var/folders/zz/zyxvpxvq6csfxvn_n0000000000000", 0o755},
	{"var/folders/zz/zyxvpxvq6csfxvn_n0000000000000/0", 0o755}, {"var/folders/zz/zyxvpxvq6csfxvn_n0000000000000/C", 0o700},
	{"var/folders/zz/zyxvpxvq6csfxvn_n0000000000000/T", 0o700}, {"var/folders/zz/zyxvpxvq6csfxvn_n0000000000000/X", 0o755},
}

// boot gives a data volume what a Mac that has started has and its template has not: root's
// per-user folders. launchd is no program here to have made them, and a daemon that finds them
// missing asks dirhelper for them, which asks opendirectoryd who root is: opendirectoryd, asking
// for its own, would wait on itself.
func (m *Machine) boot(mt *mounted) {
	if mt.template == nil || mt.guest != "/private" {
		return
	}
	m.populate(mt, "var/folders")
	if st, err := mt.root.Lstat("var/folders"); err != nil || !st.IsDir() {
		return // a release whose template has no such directory
	}
	for _, f := range rootFolders {
		if _, err := mt.root.Lstat(f.rel); err == nil {
			continue
		}
		if err := mt.root.Mkdir(f.rel, 0o755); err != nil {
			stopf("the data volume: making %s: %v", pathpkg.Join(mt.guest, f.rel), err)
		}
		m.inherit(mt, f.rel, &Inode{Mode: f.mode})
		var st syscall.Stat_t
		if statInRoot(mt.root, f.rel, false, &st) == nil { // marked as dirhelper marks what it makes
			m.mark(hostFileOf(&st), "folders")
			m.flags[hostFileOf(&st)] |= flagNoUnlink
		}
	}
}
