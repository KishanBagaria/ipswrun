package emulator

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// A kernel says what it is in one line (kern.version, which uname(3) asks with the rest of
// what it tells): its version, when it was built, and the name of the build, which ends in the
// kind of chip it is for. The line is in the release's own kernelcache, as text, and is read
// from there: by what it begins with and how it goes on, wherever a release has it.
//
//	Darwin Kernel Version 27.2.0: Wed Oct 14 23:47:32 PDT 2026; root:xnu-13432.41.4~25/RELEASE_ARM64_T6031

const (
	// versionReading names what kernelVersion makes of a kernelcache, for keeping it between
	// runs: a change to how the line is read is a change to the number here.
	versionReading = "kernel version, 1"
	versionStart   = "Darwin Kernel Version "
	versionBuild   = "; root:xnu-"
	maxVersion     = 512
)

var errNoVersion = errors.New("no line that says the kernel's version was found in the kernelcache")

// kernelVersion is the line a kernelcache says its version in. It may be there more than once
// (after a mark of what it is, for one); two that differ are an error.
func kernelVersion(kc []byte) (string, error) {
	found := ""
	for at := 0; ; {
		i := bytes.Index(kc[at:], []byte(versionStart))
		if i < 0 {
			break
		}
		at += i
		end := bytes.IndexByte(kc[at:min(at+maxVersion, len(kc))], 0)
		line := ""
		if end > 0 {
			line = string(kc[at : at+end])
		}
		at += len(versionStart)
		if !strings.Contains(line, versionBuild) || strings.ContainsFunc(line, func(r rune) bool { return r < ' ' || r > '~' }) {
			continue // the words in some other text
		}
		if found != "" && line != found {
			return "", fmt.Errorf("the kernelcache says its version in two ways (%q and %q)", found, line)
		}
		found = line
	}
	if found == "" {
		return "", errNoVersion
	}
	return found, nil
}

// kernelVersion is kern.version: the release's kernel's own line, read from its kernelcache
// once. With no kernelcache to read, or none that says it, it is not known, and is left out.
func (m *Machine) kernelVersion() (string, bool) {
	if m.cfg.KernelReading == nil {
		return "", false
	}
	if m.version == "" && m.versionErr == nil {
		line, err := away(m.clock, func() ([]byte, error) { // the host's work, not the guest's time
			return m.cfg.KernelReading(versionReading, func(kc []byte) ([]byte, error) {
				line, err := kernelVersion(kc)
				return []byte(line), err
			})
		})
		if m.version, m.versionErr = string(line), err; err == nil && m.version == "" {
			m.versionErr = errNoVersion
		}
	}
	return m.version, m.versionErr == nil
}
