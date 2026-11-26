package emulator

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
)

// The sandbox profiles a kernel has by name are in its kernelcache: the sandbox extension holds
// a collection of compiled profiles, and a daemon that asks for one by name (opendirectoryd) is
// given it by the kernel, where any other is compiled from a file of the release's. Which names
// those are is read from the release's own kernelcache, by the collection's shape and not by
// where a release has it or how long its parts are:
//
//	the number 0x8000, the number of nodes, and among the next few numbers that of profiles;
//	a record of one length for each profile, which begins with where its name is, and whose
//	numbers past the first few are each one of the nodes;
//	8 bytes for each node; and, from the next multiple of 8, the strings, each a length (with
//	its closing zero) and then the bytes, a name's place being counted in eights.
//
// On macOS 27.2.1 this gives the 14 names a Mac's kernel was measured to have, of 63,433 it was
// asked about, and no other.

const (
	sandboxExtension   = "com.apple.security.sandbox"
	collectionMagic    = 0x8000
	loadFilesetEntry   = 0x80000035 // LC_FILESET_ENTRY
	machHeader64       = 0xFEEDFACF
	machFileset        = 0xC // MH_FILESET
	maxProfiles        = 4096
	maxProfileRecord   = 8192
	maxCollectionStart = 64
	fewestProfiles     = 2
	recordHead         = 8 // of a record: its name's place and what else is not a node
)

var errNoProfiles = errors.New("no collection of sandbox profiles was found in the kernelcache")

// kernelProfiles is the names of the sandbox profiles a kernel gives by name: those of the first
// collection in its sandbox extension. The newest releases have a second after it, of profiles
// for the extension's own tests, which a Mac's kernel was measured not to give by name (macOS
// 27.2.1). A collection that can be read as one in two ways that differ is an error.
func kernelProfiles(kc []byte) ([]string, error) {
	from, to := sandboxExtensionRange(kc)
	for q := from; q+maxCollectionStart < to; q += 2 {
		if le.Uint16(kc[q:]) != collectionMagic {
			continue
		}
		readings := collectionsAt(kc[q:to])
		for _, names := range readings {
			if !slices.Equal(names, readings[0]) {
				return nil, fmt.Errorf("what looks like the kernelcache's collection of sandbox profiles reads two ways (%d names: %v; or %d: %v)", len(readings[0]), readings[0], len(names), names)
			}
		}
		if len(readings) != 0 {
			return readings[0], nil
		}
	}
	return nil, errNoProfiles
}

// sandboxExtensionRange is where in a kernelcache the sandbox extension is, if the kernelcache
// is a set of files that names its parts (as those of every Mac with Apple's chips are, and an
// iPhone's since iOS 16); otherwise it is all of it.
func sandboxExtensionRange(kc []byte) (from, to int) {
	if len(kc) < 32 || le.Uint32(kc) != machHeader64 || le.Uint32(kc[12:]) != machFileset {
		return 0, len(kc)
	}
	type entry struct {
		at   int
		name string
	}
	var entries []entry
	at := 32
	for range le.Uint32(kc[16:]) { // ncmds
		if at+8 > len(kc) {
			break
		}
		cmd, size := le.Uint32(kc[at:]), int(le.Uint32(kc[at+4:]))
		if size < 8 || at+size > len(kc) {
			break
		}
		if cmd == loadFilesetEntry && size >= 32 { // vmaddr, fileoff, the name's place, reserved
			name := kc[at+int(le.Uint32(kc[at+24:])) : at+size]
			if end := bytes.IndexByte(name, 0); end >= 0 {
				name = name[:end]
			}
			entries = append(entries, entry{int(le.Uint64(kc[at+16:])), string(name)})
		}
		at += size
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.at - b.at })
	for i, e := range entries {
		if e.name != sandboxExtension || e.at >= len(kc) {
			continue
		}
		if i+1 < len(entries) && entries[i+1].at <= len(kc) {
			return e.at, entries[i+1].at
		}
		return e.at, len(kc)
	}
	return 0, len(kc)
}

// collectionsAt is the names of the profiles of a collection that begins at the start of c, as
// each way of reading what is there as one gives them: none if it is not one. Every profile's
// record must lead to a name, and no two to the same.
func collectionsAt(c []byte) [][]string {
	var found [][]string
	nodes := int(le.Uint16(c[2:]))
	for field := 4; field < 20; field += 2 { // which of the numbers after is how many profiles
		count := int(le.Uint16(c[field:]))
		if count < fewestProfiles || count > maxProfiles {
			continue
		}
		for start := field + 2; start <= maxCollectionStart; start += 2 { // where the records begin
			for record := 4; record <= maxProfileRecord; record += 2 { // how long each is
				strings := (start + count*record + nodes*8 + 7) &^ 7
				if strings >= len(c) {
					break
				}
				if names := profileNames(c, start, record, count, nodes, strings); names != nil {
					found = append(found, names)
				}
			}
		}
	}
	return found
}

func profileNames(c []byte, start, record, count, nodes, strings int) []string {
	var names []string
	for i := range count {
		for at := start + i*record + recordHead; at < start+(i+1)*record; at += 2 {
			if int(le.Uint16(c[at:])) >= nodes {
				return nil
			}
		}
		at := strings + 8*int(le.Uint16(c[start+i*record:]))
		if at+3 > len(c) {
			return nil
		}
		length := int(le.Uint16(c[at:]))
		if length < 2 || at+2+length > len(c) || c[at+2+length-1] != 0 {
			return nil
		}
		name := c[at+2 : at+2+length-1]
		for _, b := range name {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '.' || b == '-' || b == '_') {
				return nil
			}
		}
		if slices.Contains(names, string(name)) {
			return nil
		}
		names = append(names, string(name))
	}
	return names
}

// profileDirectories are where a release keeps the sandbox profiles that are files.
var profileDirectories = []string{"/System/Library/Sandbox/Profiles", "/usr/share/sandbox"}

// kernelHasProfile is Sandbox 22's answer: whether the kernel has a sandbox profile by a name.
// It has none that is a file of the release's (a Mac's kernel was asked of all 610 that are, on
// macOS 27.2.1, and had none), so those are told apart without the kernelcache, which is read
// for the others, once. Were that not so of another release, a guest would be told its kernel has
// no such profile and compile the file, which here comes to the same: a profile confines nothing.
func (p *Process) kernelHasProfile(name string, a []uint64) bool {
	m := p.m
	if m.cfg.KernelCache == nil {
		return false
	}
	for _, dir := range profileDirectories {
		if t, ok := p.resolve(dir + "/" + name + ".sb"); ok && t.kind != dirFD {
			return false
		}
	}
	if m.profiles == nil && m.profilesErr == nil {
		m.profiles, m.profilesErr = away(m.clock, func() ([]string, error) { // the host's work, not the guest's time
			kc, err := m.cfg.KernelCache()
			if err != nil {
				return nil, err
			}
			return kernelProfiles(kc)
		})
		p.note("  the kernel's sandbox profiles: %v %v", m.profiles, m.profilesErr)
	}
	if m.profilesErr != nil {
		p.unimplemented("a sandbox profile by name, of a kernel whose profiles could not be read ("+m.profilesErr.Error()+")", a)
		return false
	}
	return slices.Contains(m.profiles, name)
}
