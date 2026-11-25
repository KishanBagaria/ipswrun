package emulator

import (
	"testing"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// A process of an iOS build is told what an iPhone says of itself, and one of a macOS build what
// a Mac does.
func TestSysctlsByPlatform(t *testing.T) {
	for _, c := range []struct {
		platform              dyldcache.Platform
		machine, model, board string
		variant               uint64
	}{
		{dyldcache.MacOS, "arm64", "Macmini9,1", "J274", osVariantMac},
		{dyldcache.IOS, "iPhone11,2", "D321AP", "D321", osVariantIOS},
	} {
		p := &Process{cache: &dyldcache.Cache{Platform: c.platform}, os: map[string]string{"model": map[dyldcache.Platform]string{dyldcache.MacOS: "Macmini9,1", dyldcache.IOS: "iPhone11,2"}[c.platform], "board": "D321AP"}}
		for name, want := range map[string]any{"hw.machine": c.machine, "hw.model": c.model, "hw.targettype": c.board, "kern.osvariant_status": c.variant} {
			if got, _ := p.sysctlValue(name); got != want {
				t.Errorf("%v: %s = %v, want %v", c.platform, name, got, want)
			}
		}
	}
}

// iOS's Objective-C runtime keeps a class's data pointer in 39 bits: what it allocates must be
// below 512 GB, and above the largest shared region (at most 0x180000000 from 0x180000000).
func TestHeapByPlatform(t *testing.T) {
	if got := heapStart(dyldcache.MacOS); got != heapBase {
		t.Errorf("macOS heap at %#x", got)
	}
	if got := heapStart(dyldcache.IOS); got >= 1<<39 || got < 0x300000000 {
		t.Errorf("iOS heap at %#x", got)
	}
}
