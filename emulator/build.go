package emulator

import (
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

// CacheName is the file name of the arm64e dyld shared cache; subcaches add a suffix.
const CacheName = "dyld_shared_cache_arm64e"

// BaseEnv is the environment every guest gets: no allocator features that need address space or
// kernel support the emulator lacks, and the emulated home.
var BaseEnv = []string{"MallocNanoZone=0", "MallocDeferredReclaim=0", "HOME=" + GuestHome, "TMPDIR=" + GuestHome + "/tmp/"}

// noPreferencesEnv is added for a run without the daemon that keeps preferences, cfprefsd: it
// has CoreFoundation read and write them in the process itself. The fixed home does that too,
// which is why a run with the daemon has to do without it.
var noPreferencesEnv = []string{"__CFPREFERENCES_AVOID_DAEMON=1", "CFFIXED_USER_HOME=" + GuestHome}

// preferencesAgent is the service of cfprefsd that keeps a user's preferences.
const preferencesAgent = "com.apple.cfprefsd.agent"

// DefaultSystem is what a guest may read from the build's system volume unless told otherwise:
// the feature flags, the system version, ICU's data (locales, calendars, time zones) and the
// Objective-C runtime's block trampolines, which are not in the shared cache.
var DefaultSystem = []string{"/System/Library/FeatureFlags/", "/System/Library/CoreServices/SystemVersion.plist", "/usr/share/icu/",
	"/usr/lib/libobjc-trampolines.dylib"}

// SystemPrefix starts the name of an executable that is taken from the build's system volume
// and not from the host: system:/usr/sbin/cfprefsd.
const SystemPrefix = "system:"

// Daemon is a guest that runs beside the main one, to provide Mach services to it.
type Daemon struct {
	Services   []string // the names it may check in with launchd
	Executable string   // host path of its Mach-O, or with SystemPrefix its path in the build
	Args       []string // after argv[0]
	Root       bool     // it runs as root, as a launchd daemon does, and not as the guest's user, as an agent
}

// Run describes one guest execution against a build.
type Run struct {
	Unicorn    string        // path of the verified libunicorn (see EnsureUnicorn)
	Home       string        // host directory behind GuestHome
	Mounts     []Mount       // more host directories the guests may change (see CheckMounts)
	Services   []string      // the Mach services the guest provides: the names it may check in with launchd
	Executable string        // host path of the guest Mach-O (arm64, linked against the cache), or with SystemPrefix its path in the build
	Args       []string      // guest arguments after argv[0]
	Daemons    []Daemon      // more guests, started with it, that provide services
	Env        []string      // added to BaseEnv, for every guest
	System     []string      // system-volume files and directory prefixes (ending in "/") the guest may read; DefaultSystem if empty
	Timeout    time.Duration // for the whole run
	Trace      bool          // print every system call to stderr
	Survey     bool          // answer calls nothing implements with an error instead of stopping (see Config.Survey)
	Notes      io.Writer     // told about a cache location that could not be read, unless the release has none there; os.Stderr if nil
}

var leadingDigits = regexp.MustCompile(`^\d+`)

// nestedFramework matches the directory of a framework that is inside another.
var nestedFramework = regexp.MustCompile(`^/System/Library/(Private)?Frameworks/.+\.framework/.+\.framework$`)

// realDirectory is the path of a directory of the system volume with every symbolic link on the
// way to it followed; empty if there is no such directory.
func realDirectory(image *rangefs.Image, path string) (string, error) {
	real := ""
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for links := 0; len(parts) > 0; {
		next := real + "/" + parts[0]
		entry, err := image.Stat(rangefs.FileSystem, next)
		if err != nil {
			return "", err
		}
		switch entry.Kind {
		case rangefs.Directory:
			real, parts = next, parts[1:]
		case rangefs.Symlink:
			if links++; links > 16 {
				return "", nil
			}
			target := entry.Target
			if !strings.HasPrefix(target, "/") {
				target = real + "/" + target
			}
			parts = append(strings.Split(strings.TrimPrefix(pathpkg.Clean(target), "/"), "/"), parts[1:]...)
			real = ""
		default:
			return "", nil
		}
	}
	return real, nil
}

// RunBuild starts the guest, and the daemons beside it, inside the userland of the build in image
// and runs them until the guest exits. The machine is returned even when the run fails, for its
// diagnostics; the caller closes it.
func RunBuild(image *rangefs.Image, run Run) (*Machine, *dyldcache.Cache, error) {
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
		if f, ok := known[path]; ok {
			return f, nil
		}
		if !wanted {
			// A framework inside another is reached through a symbolic link (Frameworks, for
			// Versions/Current/Frameworks). Its code is in the shared cache under its real path,
			// which dyld finds by opening the framework's directory: that much of it exists.
			if !nestedFramework.MatchString(path) {
				return SystemFile{}, nil
			}
			real, err := realDirectory(image, path)
			if err != nil {
				return SystemFile{}, err
			}
			f := SystemFile{Dir: real != "" && real != path, Real: real}
			known[path] = f
			if !f.Dir {
				return SystemFile{}, nil
			}
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
	m, err := NewMachine(Config{Unicorn: run.Unicorn, Cache: cache, CacheDir: guestDir, Home: run.Home, Mounts: run.Mounts, Dyld: dyld, System: system, Trace: run.Trace, Survey: run.Survey,
		OS: map[string]string{"build": build, "version": image.Version(), "release": leadingDigits.FindString(build) + ".0.0"}})
	if err != nil {
		return nil, cache, err
	}
	env := slices.Clone(BaseEnv)
	if !slices.ContainsFunc(run.Daemons, func(d Daemon) bool { return slices.Contains(d.Services, preferencesAgent) }) {
		env = append(env, noPreferencesEnv...)
	}
	env = append(env, run.Env...)
	guests := []Guest{{Executable: run.Executable, Args: run.Args, Env: env, Services: run.Services}}
	for _, d := range run.Daemons {
		guests = append(guests, Guest{Executable: d.Executable, Args: d.Args, Env: env, Services: d.Services, Root: d.Root})
	}
	for _, g := range guests {
		if path, ok := strings.CutPrefix(g.Executable, SystemPrefix); ok {
			f, err := image.Open(rangefs.FileSystem, path)
			if err == nil && f == nil {
				err = fmt.Errorf("the system volume has no file %s", path)
			}
			if err == nil {
				g.Image, err = f.ReadAll()
			}
			if err != nil {
				return m, cache, fmt.Errorf("%s: %w", g.Executable, err)
			}
			g.Executable = path
		}
		if _, err := m.Spawn(g); err != nil {
			return m, cache, fmt.Errorf("%s: %w", g.Executable, err)
		}
	}
	return m, cache, m.Run(run.Timeout)
}
