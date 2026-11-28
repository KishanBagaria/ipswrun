package emulator

import "testing"

func TestKernelVersion(t *testing.T) {
	line := "Darwin Kernel Version 27.2.0: Wed Oct 14 23:46:57 PDT 2026; root:xnu-13432.41.4~25/RELEASE_ARM64_T8103"
	for name, c := range map[string]struct {
		kc   string
		want string
		fail bool
	}{
		"once":            {"junk\x00" + line + "\x00more", line, false},
		"twice the same":  {"@(#)VERSION: " + line + "\x00pad\x00" + line + "\x00", line, false},
		"in other words":  {"the Darwin Kernel Version is said elsewhere\x00" + line + "\x00", line, false},
		"two that differ": {line + "\x00" + line + "x\x00", "", true},
		"none":            {"nothing here\x00", "", true},
		"not ended":       {line, "", true},
	} {
		got, err := kernelVersion([]byte(c.kc))
		if got != c.want || (err != nil) != c.fail {
			t.Errorf("%s: %q, %v; want %q, an error: %v", name, got, err, c.want, c.fail)
		}
	}
}
