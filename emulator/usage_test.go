package emulator

import (
	"testing"
	"time"
)

func TestChildrenUsed(t *testing.T) {
	// What children used is added up as they are waited for: times and counts together, and of
	// memory the most any one had (tests/kernel/files/everyday.c prints the same natively).
	var sum used
	sum.add(used{user: 3 * time.Millisecond, system: time.Millisecond, most: 4 * pageSize, minor: 2, waits: 1})
	sum.add(used{user: time.Millisecond, system: 2 * time.Millisecond, most: 9 * pageSize, minor: 5, major: 1})
	sum.add(used{most: pageSize})
	want := used{user: 4 * time.Millisecond, system: 3 * time.Millisecond, most: 9 * pageSize, minor: 7, major: 1, waits: 1}
	if sum != want {
		t.Fatalf("children used %+v, want %+v", sum, want)
	}
	b := sum.bytes()
	if len(b) != 144 || le.Uint32(b[8:]) != 4000 || le.Uint32(b[24:]) != 3000 || le.Uint64(b[32:]) != 9*pageSize || le.Uint64(b[64:]) != 7 {
		t.Fatalf("struct rusage % x", b)
	}
}

func TestDescriptorArguments(t *testing.T) {
	// Every call whose descriptors are read as ints is one there is a handler for, and none names
	// an argument the call does not have.
	for name, bits := range descriptorArgs {
		h, ok := bsdHandlers[name]
		if !ok {
			t.Errorf("%s has descriptor arguments and no handler", name)
		} else if bits>>h.args != 0 {
			t.Errorf("%s has %d arguments, and descriptors %#b", name, h.args, bits)
		}
	}
}
