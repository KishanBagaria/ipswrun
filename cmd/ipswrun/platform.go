package main

import (
	"debug/macho"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/KishanBagaria/ipswrun/dyldcache"
)

// The Mach-O load commands that say which system an executable was built for.
const (
	lcVersionMinMacOS  = 0x24
	lcVersionMinIPhone = 0x25
	lcBuildVersion     = 0x32
)

// platformOf is the platform of the system an OS of osOf's runs.
func platformOf(system string) dyldcache.Platform {
	if system == iOS {
		return dyldcache.IOS
	}
	return dyldcache.MacOS
}

// builtFor is the platform the arm64 code of the Mach-O at path was built for, or 0 when it
// says none, or path is not a Mach-O this can read (a source file, a script, one of the
// release's own): dyld is left to judge those.
func builtFor(path string) dyldcache.Platform {
	f, err := macho.Open(path)
	if err == nil {
		defer f.Close()
		return loadsPlatform(f)
	}
	fat, err := macho.OpenFat(path)
	if err != nil {
		return 0
	}
	defer fat.Close()
	for _, arch := range fat.Arches {
		if arch.Cpu == macho.CpuArm64 {
			return loadsPlatform(arch.File)
		}
	}
	return 0
}

func loadsPlatform(f *macho.File) dyldcache.Platform {
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) < 12 {
			continue
		}
		switch f.ByteOrder.Uint32(raw) {
		case lcBuildVersion:
			return dyldcache.Platform(f.ByteOrder.Uint32(raw[8:]))
		case lcVersionMinMacOS:
			return dyldcache.MacOS
		case lcVersionMinIPhone:
			return dyldcache.IOS
		}
	}
	return 0
}

// checkBuiltFor refuses an executable built for the other system than the --device runs, before
// anything is fetched for it: the release's dyld would refuse it only once its image is open. A
// Mac Catalyst one is a Mac's. Source files are built for the right one (see compile).
func checkBuiltFor(path, system string) error {
	if compiler(path, system) != nil {
		return nil
	}
	want := platformOf(system)
	fix := "build it for macOS, or pass an iPhone or iPad --device (iPhone11,2) to run it on iOS"
	if want == dyldcache.IOS {
		fix = "build it for iOS (as make -C examples PLATFORM=ios does), or leave out --device to run it on macOS"
	}
	switch got := builtFor(path); {
	case got == 0, got == want, got == dyldcache.Catalyst && want == dyldcache.MacOS:
		return nil
	case got == dyldcache.MacOS || got == dyldcache.IOS:
		return fmt.Errorf("%s is built for %v, and this --device runs %v; %s", path, got, want, fix)
	default:
		return fmt.Errorf("%s is built for %v (see LC_BUILD_VERSION), not %v", path, got, want)
	}
}

// errOldChip is in the error for a --device whose iOS has no arm64e shared cache, which is the
// only kind ipswrun runs: every iPhone, iPad and iPod touch with a chip before the A12.
var errOldChip = errors.New("has a chip before the A12, and its iOS only an arm64 shared cache, where ipswrun runs the arm64e one")

// checkDevice refuses an iOS --device whose releases ipswrun cannot run, before anything is looked
// up for it. A12 devices are iPhone11,* and iPad8,* (the A12X) on; no iPod touch has one.
func checkDevice(device string) error {
	product, rest, _ := strings.Cut(device, ",")
	var name string
	var first int
	switch {
	case strings.HasPrefix(product, "iPhone"):
		name, first = "iPhone", 11
	case strings.HasPrefix(product, "iPad"):
		name, first = "iPad", 8
	case strings.HasPrefix(product, "iPod"):
		name, first = "iPod", 1<<30
	}
	major, err := strconv.Atoi(strings.TrimPrefix(product, name))
	switch {
	case name == "" || err != nil || rest == "":
		return fmt.Errorf("--device %s is not an iPhone, iPad or iPod touch product type like iPhone11,2", device)
	case major < first:
		return fmt.Errorf("--device %s %w; fix: pass an iPhone from iPhone11,2 (iPhone XS) on, or an iPad from iPad8,1 on", device, errOldChip)
	}
	return nil
}
