package emulator

import (
	"os"
	"slices"
	"testing"
)

// collection builds a collection of sandbox profiles in the shape kernelProfiles reads, with a
// header of the given length and records of the given length.
func collection(names []string, header, record, nodes int) []byte {
	c := make([]byte, header+len(names)*record+nodes*8)
	le.PutUint16(c, collectionMagic)
	le.PutUint16(c[2:], uint16(nodes))
	le.PutUint16(c[header-10:], uint16(len(names)))
	for len(c)%8 != 0 {
		c = append(c, 0)
	}
	pool := len(c)
	c = append(c, 2, 0, 'x', 0, 0, 0, 0, 0)       // a string that is no profile's name
	for i, name := range slices.Backward(names) { // not in the records' order
		for len(c)%8 != 0 {
			c = append(c, 0)
		}
		le.PutUint16(c[header+i*record:], uint16((len(c)-pool)/8))
		c = le.AppendUint16(c, uint16(len(name)+1))
		c = append(append(c, name...), 0)
	}
	return append(c, make([]byte, 64)...)
}

func TestKernelProfiles(t *testing.T) {
	want := []string{"com.apple.opendirectoryd", "com.openssh.sshd", "no-network", "pure-computation"}
	for _, shape := range []struct{ header, record, nodes int }{{0x14, 0x1a0, 0xf0}, {0x0e, 0x30, 3}, {0x18, 0x202, 5}} {
		kc := append(make([]byte, 4096), collection(want, shape.header, shape.record, shape.nodes)...)
		got, err := kernelProfiles(kc)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("a collection with a header of %#x and records of %#x: %v, %v", shape.header, shape.record, got, err)
		}
	}
	// The first collection is the kernel's; one after it is of other profiles.
	first, second := collection(want, 0x14, 0x40, 9), collection([]string{"tests-allow", "tests-deny", "tests-empty"}, 0x14, 0x40, 9)
	if got, err := kernelProfiles(append(first, second...)); err != nil || !slices.Equal(got, want) {
		t.Errorf("two collections: %v, %v", got, err)
	}
	// A record with a number in it that is no node's is no profile's.
	bad := collection(want, 0x14, 0x40, 9)
	le.PutUint16(bad[0x14+0x40+recordHead:], 9)
	if got, _ := kernelProfiles(bad); got != nil {
		t.Errorf("a record that names a node there is not: %v", got)
	}
	if got, err := kernelProfiles(make([]byte, 1<<16)); err == nil {
		t.Errorf("a kernelcache with no collection: %v", got)
	}
	// Two names the same are no collection, and nor is a record that leads to no name.
	if got, _ := kernelProfiles(collection([]string{"a.b", "a.b", "c.d"}, 0x14, 0x40, 1)); got != nil {
		t.Errorf("a collection that names a profile twice: %v", got)
	}

	// A Mac's own kernelcache, unpacked (ipsw kernel dec), where there is one to read: the 14
	// names its kernel was measured to have on macOS 27.2.1.
	path := os.Getenv("IPSWRUN_KERNELCACHE")
	if path == "" {
		return
	}
	kc, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := kernelProfiles(kc)
	t.Logf("%d profiles: %v (%v)", len(got), got, err)
	if os.Getenv("IPSWRUN_KERNELCACHE_BUILD") == "26B131" {
		measured := []string{"com.apple.corebrightnessd", "com.apple.dext", "com.apple.endpointsecurity.endpointsecurityd", "com.apple.oahd", "com.apple.oahd-helper", "com.apple.oahd-root-helper",
			"com.apple.opendirectoryd", "com.apple.sandboxd", "com.openssh.sshd", "no-internet", "no-network", "no-write", "no-write-except-temporary", "pure-computation"}
		slices.Sort(got)
		if !slices.Equal(got, measured) {
			t.Errorf("the profiles of macOS 27.2.1: %v", got)
		}
	}
}
