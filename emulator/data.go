package emulator

import (
	"bufio"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
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
	// Of a file: its size, what it holds, and its permission bits.
	Size int64
	Read func() ([]byte, error)
	Mode uint32
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
			err = mt.root.Mkdir(child, 0o755)
		case e.Link != "":
			err = mt.root.Symlink(e.Link, child)
		case e.Read != nil:
			if files += e.Size; files > maxTemplateFiles {
				stopf("the data volume: the files the release's template has in %s come to more than %d MB, and a directory's are copied when it is first entered", pathpkg.Join(mt.guest, dir), maxTemplateFiles>>20)
			}
			var data []byte
			if data, err = away(m.clock, e.Read); err == nil {
				err = mt.root.WriteFile(child, data, os.FileMode(e.Mode&0o777))
			}
			if err == nil { // whatever the host's own mask made of it
				err = mt.root.Chmod(child, os.FileMode(e.Mode&0o777))
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

// openRecord reads which directories of a mount were filled in earlier runs, from the file that
// is kept of that, and opens it to add to.
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
	for lines := bufio.NewScanner(f); lines.Scan(); {
		if guest, dir, ok := strings.Cut(lines.Text(), "\t"); ok && guest == mt.guest {
			mt.populated[dir] = true
		}
	}
	mt.record = f
	return nil
}
