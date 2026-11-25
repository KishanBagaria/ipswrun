package main

import (
	"debug/macho"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/KishanBagaria/ipswrun/emulator"
)

// The Mach-O load commands that say which system an executable was built for.
const (
	lcVersionMinMacOS  = 0x24
	lcVersionMinIPhone = 0x25
	lcBuildVersion     = 0x32
)

// Platforms of LC_BUILD_VERSION, as dyldcache.Platform has the first two.
const (
	platformMacOS    = 1
	platformIOS      = 2
	platformCatalyst = 6
)

// builtFor is the platform the arm64 code of the Mach-O at path was built for, or 0 when it
// says none, or path is not a Mach-O this can read (a source file, a script): dyld is left to
// judge those.
func builtFor(path string) uint32 {
	f, err := macho.Open(path)
	if err != nil {
		fat, fatErr := macho.OpenFat(path)
		if fatErr != nil {
			return 0
		}
		defer fat.Close()
		for _, arch := range fat.Arches {
			if arch.Cpu == macho.CpuArm64 {
				return platformOf(arch.File)
			}
		}
		return 0
	}
	defer f.Close()
	return platformOf(f)
}

func platformOf(f *macho.File) uint32 {
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) < 12 {
			continue
		}
		switch f.ByteOrder.Uint32(raw) {
		case lcBuildVersion:
			return f.ByteOrder.Uint32(raw[8:])
		case lcVersionMinMacOS:
			return platformMacOS
		case lcVersionMinIPhone:
			return platformIOS
		}
	}
	return 0
}

// checkBuiltFor refuses an executable built for the other system than the --device runs, before
// anything is fetched for it: the release's dyld would refuse it only once its image is open. A
// Mac Catalyst one is a Mac's. Source files are built for the right one (see compile), and one of
// the release's own is the release's.
func checkBuiltFor(path, system string) error {
	if strings.HasPrefix(path, emulator.SystemPrefix) || compiler(path, system) != nil {
		return nil
	}
	want, other, fix := "macOS", "iOS", "build it for macOS, or pass an iPhone or iPad --device (iPhone11,2) to run it on iOS"
	if system == iOS {
		want, other, fix = "iOS", "macOS", "build it for iOS (as make -C examples PLATFORM=ios does), or leave out --device to run it on macOS"
	}
	switch platform := builtFor(path); {
	case platform == 0:
		return nil
	case system == iOS && platform == platformIOS, system != iOS && (platform == platformMacOS || platform == platformCatalyst):
		return nil
	case platform == platformIOS || platform == platformMacOS:
		return fmt.Errorf("%s is built for %s, and this --device runs %s; %s", path, other, want, fix)
	default:
		return fmt.Errorf("%s is built for platform %d (see LC_BUILD_VERSION), not %s", path, platform, want)
	}
}

// errOldChip is in the error for a --device whose iOS has no arm64e shared cache, which is the
// only kind ipswrun runs: every iPhone, iPad and iPod touch with a chip before the A12.
var errOldChip = errors.New("has a chip before the A12, and its iOS only an arm64 shared cache, where ipswrun runs the arm64e one")

// checkDevice refuses an iOS --device whose releases ipswrun cannot run, before anything is looked
// up for it. A12 devices are iPhone11,* and iPad8,* (the A12X) on; no iPod touch has one.
func checkDevice(device string) error {
	product, rest, ok := strings.Cut(device, ",")
	first := map[string]int{"iPhone": 11, "iPad": 8, "iPod": 1 << 30}
	for prefix, from := range first {
		major, err := strconv.Atoi(strings.TrimPrefix(product, prefix))
		if !strings.HasPrefix(product, prefix) || err != nil {
			continue
		}
		if !ok || rest == "" {
			break
		}
		if major < from {
			return fmt.Errorf("--device %s %w; fix: pass an iPhone from iPhone11,2 (iPhone XS) on, or an iPad from iPad8,1 on", device, errOldChip)
		}
		return nil
	}
	return fmt.Errorf("--device %s is not an iPhone, iPad or iPod touch product type like iPhone11,2", device)
}
