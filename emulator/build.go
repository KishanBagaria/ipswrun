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

// cacheLocations are where a restore image may keep its dyld shared cache, in the order they are
// looked in: macOS 13 and later and iOS 16 and later have it in the SystemOS cryptex, earlier
// releases on the system volume. macOS keeps it in /System/Library/dyld, iOS in the directory
// macOS used before 11.
var cacheLocations = []struct{ volume, dir string }{
	{rangefs.SystemOS, "/System/Library/dyld"}, {rangefs.SystemOS, "/System/Library/Caches/com.apple.dyld"},
	{rangefs.FileSystem, "/System/Library/dyld"}, {rangefs.FileSystem, "/System/Library/Caches/com.apple.dyld"},
}

// BaseEnv is the environment every guest gets: no preference daemon, no allocator features that
// need address space or kernel support the emulator lacks, and the emulated home (see HomeOf). A
// run that has what one of these stands in for takes it out again (see Run.Env).
func BaseEnv(home string) []string {
	return []string{"__CFPREFERENCES_AVOID_DAEMON=1", "MallocNanoZone=0", "MallocDeferredReclaim=0", "HOME=" + home,
		"CFFIXED_USER_HOME=" + home, "TMPDIR=" + home + "/tmp/"}
}

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
	Home       string   // host directory behind the guest's home (see HomeOf)
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

// underCacheDir says whether path is below one of dirs, other than / itself: whether a symbolic
// link on its way could lead it into another.
func underCacheDir(dirs map[string]bool, path string) bool {
	for d := pathpkg.Dir(path); d != "/" && d != "."; d = pathpkg.Dir(d) {
		if dirs[d] {
			return true
		}
	}
	return false
}

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
// and runs them until the guest exits. The build may be of macOS or of iOS: which one is the
// returned cache's Platform. The machine is returned even when the run fails, for its
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
	var volume, dir, guestDir string
	var skipped []error
	for _, candidate := range cacheLocations {
		f, err := image.Open(candidate.volume, candidate.dir+"/"+CacheName)
		if err != nil {
			// A release from before the cryptex has nothing to say about lacking one, and one
			// volume's failure is said once, not for each directory looked in there.
			if !errors.Is(err, rangefs.ErrNoSystemOS) && !slices.ContainsFunc(skipped, func(e error) bool { return e.Error() == err.Error() }) {
				fmt.Fprintf(notes, "note: %v\n", err)
			}
			skipped = append(skipped, err)
			continue
		}
		if f != nil {
			main, volume, dir, guestDir = f, candidate.volume, candidate.dir, candidate.dir
			if volume == rangefs.SystemOS {
				guestDir = cryptex + dir
			}
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
		f, err := image.Open(volume, dir+"/"+CacheName+suffix)
		if err == nil && f == nil {
			err = fmt.Errorf("%s:%s/%s%s is missing", volume, dir, CacheName, suffix)
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
	// cacheDirs are the directories on the way to the shared cache's images, by their install
	// names, without symbolic links.
	cacheDirs := sync.OnceValues(func() (map[string]bool, error) {
		paths, err := cache.ImagePaths()
		if err != nil {
			return nil, err
		}
		dirs := map[string]bool{}
		for _, p := range paths {
			for d := pathpkg.Dir(p); d != "/" && d != "." && !dirs[d]; d = pathpkg.Dir(d) {
				dirs[d] = true
			}
		}
		return dirs, nil
	})
	known := map[string]SystemFile{}
	system := func(path string) (SystemFile, error) {
		if f, ok := known[path]; ok {
			return f, nil
		}
		// What the SystemOS cryptex has is under its mount point, as dyld looks for it there when
		// it is not on the system volume (libobjc-trampolines.dylib from iOS 16). The same
		// paths of it are allowed as of the system volume.
		vol, rel := rangefs.FileSystem, path
		if inCryptex, ok := strings.CutPrefix(path, cryptex+"/"); ok {
			vol, rel = rangefs.SystemOS, "/"+inCryptex
		}
		if !isAllowed(rel) {
			// A sealed system volume has the directories on the way to every image of the shared
			// cache, although the images' code is only in the cache, and dyld looks at them: it
			// finds a framework by its directory, and dlopen walks the path it is given, through
			// symbolic links (a framework inside another is reached through Frameworks, for
			// Versions/Current/Frameworks). Those directories are there, as the volume has them,
			// listing only what is allowed or leads to the cache; nothing else is.
			if vol != rangefs.FileSystem {
				return SystemFile{}, nil
			}
			dirs, err := cacheDirs()
			if err != nil {
				return SystemFile{}, err
			}
			real := ""
			if dirs[path] || underCacheDir(dirs, path) {
				if real, err = realDirectory(image, path); err != nil {
					return SystemFile{}, err
				}
			}
			var f SystemFile
			if dirs[real] {
				f = SystemFile{Dir: true, ReadDir: sync.OnceValues(func() ([]string, error) {
					entries, err := image.ReadDir(vol, real)
					if err != nil {
						return nil, err
					}
					var visible []string
					for _, name := range entries {
						if p := pathpkg.Join(real, name); dirs[p] || isAllowed(p) {
							visible = append(visible, name)
						}
					}
					return visible, nil
				})}
				if real != path {
					f.Real = real
					// Its last part may be a link itself, which those who walk the path see.
					if parent, err := realDirectory(image, pathpkg.Dir(path)); err != nil {
						return SystemFile{}, err
					} else if entry, err := image.Stat(vol, pathpkg.Join(parent, pathpkg.Base(path))); err != nil {
						return SystemFile{}, err
					} else if entry.Kind == rangefs.Symlink {
						f.Link = entry.Target
					}
				}
			}
			known[path] = f
			return f, nil
		}
		entry, err := image.Stat(vol, rel)
		if errors.Is(err, rangefs.ErrNoSystemOS) { // a release with no cryptex has nothing there
			known[path] = SystemFile{}
			return SystemFile{}, nil
		}
		if err != nil {
			return SystemFile{}, err
		}
		var f SystemFile
		switch entry.Kind {
		case rangefs.Directory:
			dirPath := rel
			f = SystemFile{Dir: true, ReadDir: sync.OnceValues(func() ([]string, error) { // the image does not change
				entries, err := image.ReadDir(vol, dirPath)
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
		OS: map[string]string{"build": build, "version": image.Version(), "release": leadingDigits.FindString(build) + ".0.0", "model": image.Device(), "board": image.Board()}})
	if err != nil {
		return nil, cache, err
	}
	env := BaseEnv(HomeOf(cache.Platform))
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
