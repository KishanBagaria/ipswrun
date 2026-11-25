package emulator

import (
	"slices"
	"strings"
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
		for _, name := range []string{"kern.osproductversioncompat", "kern.iossupportversion"} {
			if _, ok := p.sysctlValue(name); ok != (c.platform == dyldcache.MacOS) {
				t.Errorf("%v: %s is there: %v", c.platform, name, ok)
			}
		}
	}
}

// An iPhone or iPad is told the family of its own chip, the one its generation has unless the
// device is one of the few with another, and has no hw.cpubrand; a device not known, and a Mac,
// keep the M1's.
func TestCPUFamilyByDevice(t *testing.T) {
	for _, c := range []struct {
		platform dyldcache.Platform
		model    string
		family   uint64
	}{
		{dyldcache.IOS, "iPhone11,2", 0x07d34b9f},
		{dyldcache.IOS, "iPhone12,8", 0x462504d2},
		{dyldcache.IOS, "iPhone17,3", 0x204526d0},
		{dyldcache.IOS, "iPhone17,1", 0x75d4acb9},
		{dyldcache.IOS, "iPad15,7", 0x8765edea},
		{dyldcache.IOS, "iPad15,3", 0xfa33415e},
		{dyldcache.IOS, "iPad16,1", 0x2876f5b5},
		{dyldcache.IOS, "iPad16,3", 0x6f5129ac},
		{dyldcache.IOS, "iPhone99,1", 0x1b588bb3},
		{dyldcache.MacOS, "Macmini9,1", 0x1b588bb3},
	} {
		p := &Process{cache: &dyldcache.Cache{Platform: c.platform}, os: map[string]string{"model": c.model}}
		if got, _ := p.sysctlValue("hw.cpufamily"); got != c.family {
			t.Errorf("%s: hw.cpufamily = %#x, want %#x", c.model, got, c.family)
		}
		if _, ok := p.sysctlValue("hw.cpubrand"); ok != (c.platform == dyldcache.MacOS) {
			t.Errorf("%s: hw.cpubrand is there: %v", c.model, ok)
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

// An iOS guest is the user mobile, with its home in /var/mobile, as on an iPhone; a macOS one emu.
func TestUserByPlatform(t *testing.T) {
	for _, c := range []struct {
		platform dyldcache.Platform
		home     string
		gid      uint32
		entry    string
	}{
		{dyldcache.MacOS, "/Users/emu", 20, "emu:*:501:20:emu:/Users/emu:/bin/zsh\n"},
		{dyldcache.IOS, "/var/mobile", 501, "mobile:*:501:501:mobile:/var/mobile:/bin/sh\n"},
	} {
		if got := HomeOf(c.platform); got != c.home {
			t.Errorf("%v: home %s, want %s", c.platform, got, c.home)
		}
		if env := BaseEnv(HomeOf(c.platform)); !slices.Contains(env, "HOME="+c.home) || !slices.Contains(env, "TMPDIR="+c.home+"/tmp/") {
			t.Errorf("%v: environment %q", c.platform, env)
		}
		if got := passwd(userOf(c.platform), false); !strings.HasSuffix(got, c.entry) {
			t.Errorf("%v: passwd %q, want it to end in %q", c.platform, got, c.entry)
		}
		if st := statBuf(fileInfo{}, userOf(c.platform)); le.Uint32(st[16:]) != 501 || le.Uint32(st[20:]) != c.gid {
			t.Errorf("%v: files owned by %d:%d", c.platform, le.Uint32(st[16:]), le.Uint32(st[20:]))
		}
		if err := CheckMounts([]Mount{{c.home, t.TempDir()}}, c.home); err == nil {
			t.Errorf("%v: a mount at the home is taken", c.platform)
		}
	}
}
