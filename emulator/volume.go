package emulator

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// The home and the paths of the data volume (--data) are directories of the host's each, and
// one volume to a guest, as they are on a Mac: a file is moved, linked and cloned between them.
// That holds for those the host keeps on one file system of its own, which is where it can be
// done at all; one the host keeps elsewhere is a volume of its own, as any other mount is, and
// as the system's is. (What statfs says of a file's volume is volumeAt's: the home and those paths
// are a Mac's data volume there whatever the host keeps them on.)

// sameVolume says whether two files under the home or a mount are on one volume.
func sameVolume(t1, t2 target) bool {
	return t1.root != nil && t2.root != nil && (t1.root == t2.root || t1.dataOn != 0 && t1.dataOn == t2.dataOn)
}

// between does to a name under one directory of the host's and a name under another what the
// host does between two directories (renameat, linkat). Each directory is found inside its own
// root, so neither name leads out of it.
func (p *Process) between(t1, t2 target, op func(fromDir int, from string, toDir int, to string) error) int64 {
	if t1.rel == "." || t2.rel == "." {
		p.unimplemented("a move or a link of a directory the data volume is made of", nil)
		return -eNOSYS
	}
	from, name1, err := t1.parent()
	if err != nil {
		return hostErrno(err)
	}
	defer from.Close()
	to, name2, err := t2.parent()
	if err != nil {
		return hostErrno(err)
	}
	defer to.Close()
	err = op(int(from.Fd()), name1, int(to.Fd()), name2)
	return hostCall(err)
}

// parent is the directory of the host's that a file under the home or a mount is in, found
// inside its root, and the file's name in it: what the host's calls that take a directory and a
// name are given, so that a last name that is a link is not followed out of the root.
func (t target) parent() (*os.File, string, error) {
	dir, err := t.root.Open(filepath.Dir(t.rel))
	return dir, filepath.Base(t.rel), err
}

// renameHost moves a file to another name of its volume.
func (p *Process) renameHost(t1, t2 target) int64 {
	if t1.root == t2.root {
		return hostCall(t1.root.Rename(t1.rel, t2.rel))
	}
	return p.between(t1, t2, unix.Renameat)
}

// linkHost gives a file another name on its volume.
func (p *Process) linkHost(t1, t2 target) int64 {
	if t1.root == t2.root {
		return hostCall(t1.root.Link(t1.rel, t2.rel))
	}
	return p.between(t1, t2, func(fromDir int, from string, toDir int, to string) error {
		return unix.Linkat(fromDir, from, toDir, to, 0)
	})
}
