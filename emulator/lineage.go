package emulator

import (
	"bytes"
	"slices"

	"github.com/blacktop/go-macho"
)

// lineage is what tells a process from every other the machine has had, as a Mac's kernel was
// measured to keep it (tests/kernel/unique.c): the UUID of its program, a number of its own that no
// later process is given, and a version that moves on with every new process and every program
// one becomes, with its parent's number and the version its parent had when it was made.
type lineage struct {
	uuid                   [16]byte
	unique, parentUnique   uint64
	version, parentVersion uint32
}

// launchd's are the first of each, and it has no parent.
var launchdLineage = lineage{unique: 1, version: 1}

// descend is what a new process of a program has, as the child of one.
func (m *Machine) descend(of lineage, uuid [16]byte) lineage {
	m.uniques++
	return lineage{uuid: uuid, unique: launchdLineage.unique + m.uniques, version: m.moveOn(), parentUnique: of.unique, parentVersion: of.version}
}

// moveOn is the next version, for a new process or for the program one becomes.
func (m *Machine) moveOn() uint32 {
	m.versions++
	return launchdLineage.version + m.versions
}

// uniqueInfo is a struct proc_uniqidentifierinfo.
func (l lineage) uniqueInfo() []byte {
	return slices.Concat(l.uuid[:], le64(l.unique, l.parentUnique), le32(l.version, l.parentVersion), make([]byte, 16))
}

// uuidOf is the UUID a program's file has for itself, of the part of it that runs here; zeros
// for one that has none.
func uuidOf(program []byte) (uuid [16]byte, err error) {
	exe, err := thin(program, 0, cpuSubtypeARM64E)
	if err != nil {
		return uuid, err
	}
	f, err := macho.NewFile(bytes.NewReader(exe))
	if err != nil {
		return uuid, err
	}
	if u := f.UUID(); u != nil {
		uuid = u.UUID
	}
	return uuid, nil
}
