package emulator

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

// CacheName is the file name of the arm64e dyld shared cache; subcaches add a suffix.
const CacheName = "dyld_shared_cache_arm64e"

// BaseEnv is the environment every guest gets: no preference daemon, no allocator features that
// need address space or kernel support the emulator lacks, and the emulated home.
var BaseEnv = []string{"__CFPREFERENCES_AVOID_DAEMON=1", "MallocNanoZone=0", "MallocDeferredReclaim=0", "HOME=" + GuestHome,
	"CFFIXED_USER_HOME=" + GuestHome, "TMPDIR=" + GuestHome + "/tmp/"}

// DefaultSystem is what a guest may read from the build's system volume unless told otherwise:
// the feature flags, the system version, ICU's data (locales, calendars, time zones) and the
// Objective-C runtime's block trampolines, which are not in the shared cache.
var DefaultSystem = []string{"/System/Library/FeatureFlags/", "/System/Library/CoreServices/SystemVersion.plist", "/usr/share/icu/",
	"/usr/lib/libobjc-trampolines.dylib"}

// Run describes one guest execution against a build.
type Run struct {
	Unicorn    string            // path of the verified libunicorn (see EnsureUnicorn)
	Home       string            // host directory behind GuestHome
	Mounts     map[string]string // more host directories the guest may change, by the guest directory each appears as
	Executable string            // host path of the guest Mach-O (arm64, linked against the cache)
	Args       []string          // guest arguments after argv[0]
	Env        []string          // added to BaseEnv
	System     []string          // system-volume files and directory prefixes (ending in "/") the guest may read; DefaultSystem if empty
	Timeout    time.Duration     // for the whole run
	Trace      bool              // print every system call to stderr
	Survey     bool              // answer calls nothing implements with an error instead of stopping (see Config.Survey)
	Notes      io.Writer         // told about a cache location that could not be read, unless the release has none there; os.Stderr if nil
}

var leadingDigits = regexp.MustCompile(`^\d+`)

// RunBuild starts the guest inside the userland of the build in image and runs it to completion.
// The process is returned even when the run fails, for its diagnostics; the caller closes it.
func RunBuild(image *rangefs.Image, run Run) (*Process, *dyldcache.Cache, error) {
	if err := os.MkdirAll(filepath.Join(run.Home, "tmp"), 0o777); err != nil { // BaseEnv's TMPDIR
		return nil, nil, err
	}
	allowed := run.System
	if len(allowed) == 0 {
		allowed = DefaultSystem
	}
	notes := run.Notes
	if notes == nil {
		notes = os.Stderr
	}
	var main *rangefs.File
	var volume, guestDir string
	var skipped []error
	for _, candidate := range [][2]string{{rangefs.SystemOS, cryptex + "/System/Library/dyld"}, {rangefs.FileSystem, "/System/Library/dyld"}} {
		f, err := image.Open(candidate[0], "/System/Library/dyld/"+CacheName)
		if err != nil {
			// A release from before the cryptex has nothing to say about lacking one.
			if candidate[0] != rangefs.SystemOS || !errors.Is(err, rangefs.ErrNoSystemOS) {
				fmt.Fprintf(notes, "note: %v\n", err)
			}
			skipped = append(skipped, err)
			continue
		}
		if f != nil {
			main, volume, guestDir = f, candidate[0], candidate[1]
			break
		}
	}
	if main == nil {
		return nil, nil, &StopError{Message: "no arm64e dyld shared cache readable by range in this restore image", Kind: StopNoCache,
			Cause: errors.Join(skipped...)}
	}
	cache, err := dyldcache.Open(func(suffix string) (dyldcache.File, error) {
		if suffix == "" {
			return main, nil
		}
		f, err := image.Open(volume, "/System/Library/dyld/"+CacheName+suffix)
		if err == nil && f == nil {
			err = fmt.Errorf("%s:/System/Library/dyld/%s%s is missing", volume, CacheName, suffix)
		}
		if err != nil {
			return nil, err
		}
		return f, nil
	})
	if err != nil {
		return nil, nil, err
	}
	dyldFile, err := image.Open(rangefs.FileSystem, guestDyld)
	if err == nil && dyldFile == nil {
		err = errors.New("fs:/usr/lib/dyld is missing")
	}
	if err != nil {
		return nil, nil, err
	}
	dyld, err := dyldFile.ReadAll()
	if err != nil {
		return nil, nil, err
	}

	known := map[string]SystemFile{}
	system := func(path string) (SystemFile, error) {
		wanted := false
		for _, prefix := range allowed {
			wanted = wanted || strings.HasPrefix(path, prefix) || path+"/" == prefix || strings.HasPrefix(prefix, path+"/")
		}
		if !wanted {
			return SystemFile{}, nil
		}
		if f, ok := known[path]; ok {
			return f, nil
		}
		entry, err := image.Stat(rangefs.FileSystem, path)
		if err != nil {
			return SystemFile{}, err
		}
		var f SystemFile
		switch entry.Kind {
		case rangefs.Directory:
			f = SystemFile{Dir: true}
		case rangefs.Regular:
			f = SystemFile{File: entry.File}
		}
		known[path] = f
		return f, nil
	}

	build := image.Build()
	p, err := New(Config{Unicorn: run.Unicorn, Cache: cache, CacheDir: guestDir, Home: run.Home, Mounts: run.Mounts, Executable: run.Executable, Dyld: dyld, System: system, Trace: run.Trace, Survey: run.Survey,
		OS: map[string]string{"build": build, "version": image.Version(), "release": leadingDigits.FindString(build) + ".0.0"}})
	if err != nil {
		return nil, cache, err
	}
	env := append(append([]string{}, BaseEnv...), run.Env...)
	return p, cache, p.Start(append([]string{GuestExe}, run.Args...), env, run.Timeout)
}
