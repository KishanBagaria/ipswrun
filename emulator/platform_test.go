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
		product, board        string
		machine, model, type_ string
		variant               uint64
	}{
		{dyldcache.MacOS, "Macmini9,1", "J274AP", "arm64", "Macmini9,1", "J274", osVariantMac},
		{dyldcache.IOS, "iPhone11,2", "D321AP", "iPhone11,2", "D321AP", "D321", osVariantIOS},
	} {
		p := &Process{cache: &dyldcache.Cache{Platform: c.platform}, os: map[string]string{"model": c.product, "board": c.board}}
		for name, want := range map[string]any{"hw.machine": c.machine, "hw.model": c.model, "hw.targettype": c.type_, "kern.osvariant_status": c.variant} {
			if got, _ := p.sysctlValue(name); got != want {
				t.Errorf("%v: %s = %v, want %v", c.platform, name, got, want)
			}
		}
		// Values that differ by release or by chip, and are not known for sure, are left out;
		// hw.cpubrand is no kernel's.
		for _, name := range []string{"kern.osproductversioncompat", "kern.iossupportversion", "hw.cpubrand", "kern.version",
			"kern.maxfilesperproc", "hw.cpusubfamily", "hw.perflevel0.l2cachesize"} {
			if _, ok := p.sysctlValue(name); ok {
				t.Errorf("%v: %s is there", c.platform, name)
			}
		}
		_, caches := p.sysctlValue("hw.l2cachesize")
		if caches != (c.platform == dyldcache.MacOS) {
			t.Errorf("%v: hw.l2cachesize is there: %v; only an M1's are known", c.platform, caches)
		}
	}
}

// A device is told the family of its own chip, the one its generation has unless the device is
// one of the few with another; a device not known has none.
func TestCPUFamilyByDevice(t *testing.T) {
	for model, family := range map[string]any{
		"iPhone11,2": uint64(0x07d34b9f), "iPhone12,8": uint64(0x462504d2), "iPhone17,3": uint64(0x204526d0), "iPhone17,1": uint64(0x75d4acb9),
		"iPad15,7": uint64(0x8765edea), "iPad15,3": uint64(0xfa33415e), "iPad16,1": uint64(0x2876f5b5), "iPad16,3": uint64(0x6f5129ac),
		"iPhone99,1": nil, "Macmini9,1": uint64(cpuFamilyFirestorm), "Mac13,2": uint64(cpuFamilyFirestorm), "Mac14,3": nil,
	} {
		platform := dyldcache.IOS
		if !strings.HasPrefix(model, "iP") {
			platform = dyldcache.MacOS
		}
		p := &Process{cache: &dyldcache.Cache{Platform: platform}, os: map[string]string{"model": model}}
		if got, _ := p.sysctlValue("hw.cpufamily"); got != family {
			t.Errorf("%s: hw.cpufamily = %#v, want %#v", model, got, family)
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
		if err := CheckMounts([]Mount{{Guest: c.home, Host: t.TempDir()}}, c.home); err == nil {
			t.Errorf("%v: a mount at the home is taken", c.platform)
		}
	}
}
