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
	"sync"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

// CacheName is the file name of the arm64e dyld shared cache; subcaches add a suffix.
const CacheName = "dyld_shared_cache_arm64e"

// BaseEnv is the environment every guest gets: no preference daemon, no allocator features that
// need address space or kernel support the emulator lacks, and the emulated home. A run that
// has what one of these stands in for takes it out again (see Run.Env).
var BaseEnv = []string{"__CFPREFERENCES_AVOID_DAEMON=1", "MallocNanoZone=0", "MallocDeferredReclaim=0", "HOME=" + GuestHome,
	"CFFIXED_USER_HOME=" + GuestHome, "TMPDIR=" + GuestHome + "/tmp/"}

// DefaultSystem is what a guest may read from the build's system volume unless told otherwise:
// the feature flags, the system version, ICU's data (locales, calendars, time zones) and the
// Objective-C runtime's block trampolines, which are not in the shared cache.
var DefaultSystem = []string{"/System/Library/FeatureFlags/", "/System/Library/CoreServices/SystemVersion.plist", "/usr/share/icu/",
	"/usr/lib/libobjc-trampolines.dylib"}

// SystemPrefix starts the name of an executable that is taken from the build's system volume
// and not from the host: system:/usr/libexec/dirhelper.
const SystemPrefix = "system:"

// Run describes one guest execution against a build.
type Run struct {
	Unicorn    string   // path of the verified libunicorn (see EnsureUnicorn)
	Home       string   // host directory behind GuestHome
	Mounts     []Mount  // more host directories the guests may change (see CheckMounts)
	Services   []string // the Mach services the guest provides: the names it may check in with launchd
	Executable string   // host path of the guest Mach-O (arm64, linked against the cache), or with SystemPrefix its path in the build
	Args       []string // guest arguments after argv[0]
	// Daemons are more guests, started beside it to provide services: as root, as a daemon of
	// launchd's is, or as the guest's user, as an agent. Their environment is the guest's.
	Daemons []Guest
	Drivers []Driver      // the kernel's services, which guests reach through IOKit: none unless given
	Env     []string      // for every guest: KEY=VALUE is added to BaseEnv, and KEY alone is taken out of it
	System  []string      // system-volume files and directory prefixes (ending in "/") the guest may read; DefaultSystem if empty
	Timeout time.Duration // for the whole run
	Trace   bool          // print every system call to stderr
	Survey  bool          // answer calls nothing implements with an error instead of stopping (see Config.Survey)
	Notes   io.Writer     // told about a cache location that could not be read, unless the release has none there; os.Stderr if nil
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

	isAllowed := func(path string) bool {
		for _, prefix := range allowed {
			if strings.HasPrefix(path, prefix) || path+"/" == prefix || strings.HasPrefix(prefix, path+"/") {
				return true
			}
		}
		return false
	}
	known := map[string]SystemFile{}
	system := func(path string) (SystemFile, error) {
		if f, ok := known[path]; ok {
			return f, nil
		}
		if !isAllowed(path) {
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
			var f SystemFile
			if real != "" && real != path {
				f = SystemFile{Dir: true, Real: real}
			}
			known[path] = f
			return f, nil
		}
		entry, err := image.Stat(rangefs.FileSystem, path)
		if err != nil {
			return SystemFile{}, err
		}
		var f SystemFile
		switch entry.Kind {
		case rangefs.Directory:
			dirPath := path
			f = SystemFile{Dir: true, ReadDir: sync.OnceValues(func() ([]string, error) { // the image does not change
				entries, err := image.ReadDir(rangefs.FileSystem, dirPath)
				if err != nil {
					return nil, err
				}
				var visible []string
				for _, name := range entries {
					if isAllowed(pathpkg.Join(dirPath, name)) {
						visible = append(visible, name)
					}
				}
				return visible, nil
			})}
		case rangefs.Regular:
			f = SystemFile{File: entry.File}
		}
		known[path] = f
		return f, nil
	}

	build := image.Build()
	m, err := NewMachine(Config{Unicorn: run.Unicorn, Cache: cache, CacheDir: guestDir, Home: run.Home, Mounts: run.Mounts, Dyld: dyld, Drivers: run.Drivers, System: system, Trace: run.Trace, Survey: run.Survey,
		OS: map[string]string{"build": build, "version": image.Version(), "release": leadingDigits.FindString(build) + ".0.0", "model": image.Device()}})
	if err != nil {
		return nil, cache, err
	}
	env := slices.Clone(BaseEnv)
	for _, change := range run.Env {
		if name, _, set := strings.Cut(change, "="); set {
			env = append(env, change)
		} else {
			env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
		}
	}
	guests := []Guest{{Executable: run.Executable, Args: run.Args, Env: env, Services: run.Services}}
	for _, d := range run.Daemons {
		d.Env = env
		guests = append(guests, d)
	}
	read := map[string][]byte{} // the system's executables, each read once however many guests run it
	for _, g := range guests {
		if path, ok := strings.CutPrefix(g.Executable, SystemPrefix); ok {
			if read[path] == nil {
				f, err := image.Open(rangefs.FileSystem, path)
				if err == nil && f == nil {
					err = fmt.Errorf("the system volume has no file %s", path)
				}
				if err == nil {
					read[path], err = f.ReadAll()
				}
				if err != nil {
					return m, cache, fmt.Errorf("%s: %w", g.Executable, err)
				}
			}
			g.Image, g.Executable = read[path], path
		}
		if _, err := m.Spawn(g); err != nil {
			return m, cache, fmt.Errorf("%s: %w", g.Executable, err)
		}
	}
	return m, cache, m.Run(run.Timeout)
}
