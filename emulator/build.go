package emulator

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KishanBagaria/ipswrun/dyldcache"
	"github.com/KishanBagaria/ipswrun/rangefs"
	"github.com/blacktop/go-plist"
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
// the feature flags, the system version, ICU's data (locales, calendars, time zones), the
// Objective-C runtime's block trampolines, which are not in the shared cache, and the sandbox
// profiles, which a daemon that cannot read its own gives up over, and the programs, which a
// guest that starts one looks for first.
var DefaultSystem = []string{"/System/Library/FeatureFlags/", "/System/Library/CoreServices/SystemVersion.plist", "/usr/share/icu/",
	"/usr/lib/libobjc-trampolines.dylib", "/System/Library/Sandbox/Profiles/", "/usr/share/sandbox/",
	"/bin/", "/sbin/", "/usr/bin/", "/usr/sbin/", "/usr/libexec/"}

// SystemPrefix starts the name of an executable that is taken from the build's system volume
// and not from the host: system:/usr/libexec/dirhelper.
const SystemPrefix = "system:"

// Run describes one guest execution against a build.
type Run struct {
	Unicorn    string   // path of the verified libunicorn (see EnsureUnicorn)
	Home       string   // host directory behind the guest's home (see HomeOf)
	Mounts     []Mount  // more host directories the guests may change (see CheckMounts)
	Data       string   // host directory that is the machine's data volume, if it has one (see data.go)
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
	// OnDemand are the Mach services, by name or by pattern (as path.Match has them: "*" is
	// every one), that the build's own daemon or agent is started for when one is first looked
	// up, from its launchd plist.
	OnDemand []string
	Survey   bool // answer calls nothing implements with an error instead of stopping (see Config.Survey)
	// Stdout and Stderr, if not nil, are given the guest's output as it writes it (see Config).
	Stdout, Stderr io.Writer
	Notes          io.Writer // told about a cache location that could not be read, unless the release has none there; os.Stderr if nil
}

var leadingDigits = regexp.MustCompile(`^\d+`)

// inCacheDir says whether path is one of dirs or below one, other than / itself: whether it, or
// a symbolic link on its way, could lead into one.
func inCacheDir(dirs map[string]bool, path string) bool {
	for d := path; d != "/" && d != "."; d = pathpkg.Dir(d) {
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

// visibleEntries lists a directory of the volume with only the entries visible says may be seen.
func visibleEntries(image *rangefs.Image, vol, dir string, visible func(string) bool) func() ([]string, error) {
	return sync.OnceValues(func() ([]string, error) { // the image does not change
		entries, err := image.ReadDir(vol, dir)
		if err != nil {
			return nil, err
		}
		var shown []string
		for _, name := range entries {
			if visible(pathpkg.Join(dir, name)) {
				shown = append(shown, name)
			}
		}
		return shown, nil
	})
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
	reals := map[string]string{} // realDirectory, once for each path: walks share their directories
	realDir := func(path string) (string, error) {
		if real, ok := reals[path]; ok {
			return real, nil
		}
		real, err := realDirectory(image, path)
		if err == nil {
			reals[path] = real
		}
		return real, err
	}
	known := map[string]SystemFile{}
	var system func(path string) (SystemFile, error)
	system = func(path string) (SystemFile, error) {
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
			if inCacheDir(dirs, path) {
				if real, err = realDir(path); err != nil {
					return SystemFile{}, err
				}
			}
			var f SystemFile
			if dirs[real] {
				f = SystemFile{Dir: true, ReadDir: visibleEntries(image, vol, real, func(p string) bool { return dirs[p] || isAllowed(p) })}
				if real != path {
					// Its last part may be a link itself, which those who walk the path see.
					parent, err := realDir(pathpkg.Dir(path))
					if err != nil {
						return SystemFile{}, err
					}
					entry, err := image.Stat(vol, pathpkg.Join(parent, pathpkg.Base(path)))
					if err != nil {
						return SystemFile{}, err
					}
					f.Real = real
					if entry.Kind == rangefs.Symlink {
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
			f = SystemFile{Dir: true, ReadDir: visibleEntries(image, vol, rel, isAllowed)}
		case rangefs.Regular:
			f = SystemFile{File: entry.File}
		case rangefs.Symlink:
			// A link to a file, as /usr/bin/tar is to bsdtar: what it leads to, if a guest may
			// read that too, and the link's own text for those who ask what it is.
			to := entry.Target
			if !strings.HasPrefix(to, "/") {
				to = pathpkg.Join(pathpkg.Dir(path), to)
			}
			known[path] = SystemFile{} // a link that leads back to itself leads nowhere
			if f, err = system(to); err != nil {
				return SystemFile{}, err
			}
			if f.Dir || f.File != nil {
				f.Link = entry.Target
			}
		}
		known[path] = f
		return f, nil
	}

	// program reads a program of the build for a guest to start.
	program := func(path string) ([]byte, bool, error) {
		entry, err := image.Stat(rangefs.FileSystem, path)
		for links := 0; err == nil && entry.Kind == rangefs.Symlink && links < 16; links++ {
			if strings.HasPrefix(entry.Target, "/") {
				path = entry.Target
			} else {
				path = pathpkg.Join(pathpkg.Dir(path), entry.Target)
			}
			entry, err = image.Stat(rangefs.FileSystem, path)
		}
		switch {
		case err != nil:
			return nil, false, err
		case entry.Kind == rangefs.Directory:
			return nil, true, nil
		case entry.Kind != rangefs.Regular || entry.File == nil:
			return nil, false, nil
		}
		data, err := entry.File.Pread(0, entry.File.Size())
		return data, false, err
	}

	env := BaseEnv(HomeOf(cache.Platform))
	for _, change := range run.Env {
		if name, _, set := strings.Cut(change, "="); set {
			env = append(env, change)
		} else {
			env = slices.DeleteFunc(env, func(e string) bool { return strings.HasPrefix(e, name+"=") })
		}
	}
	mounts := run.Mounts
	if run.Data != "" {
		volume, err := dataVolume(image, run.Data)
		if err != nil {
			return nil, cache, err
		}
		mounts = append(slices.Clone(mounts), volume...)
	}
	var launch func(string, bool) (*Guest, error)
	if len(run.OnDemand) > 0 {
		find := launcher(image, program, env)
		launch = func(service string, system bool) (*Guest, error) {
			for _, pattern := range run.OnDemand {
				if ok, _ := pathpkg.Match(pattern, service); ok {
					return find(service, system)
				}
			}
			return nil, nil
		}
	}

	build := image.Build()
	m, err := NewMachine(Config{Program: program, Launch: launch, Unicorn: run.Unicorn, Cache: cache, CacheDir: guestDir, Home: run.Home, Mounts: mounts, Dyld: dyld, Drivers: run.Drivers, DeviceTree: image.DeviceTree, KernelCache: image.KernelCache, Stdout: run.Stdout, Stderr: run.Stderr, System: system, Trace: run.Trace, Survey: run.Survey,
		OS: map[string]string{"build": build, "version": image.Version(), "release": leadingDigits.FindString(build) + ".0.0", "model": image.Device(), "board": image.Board()}})
	if err != nil {
		return nil, cache, err
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

// The data volume, as a release has it: the list of the paths that are that volume's, each
// with where on the volume it is, and what a newly installed Mac's volume has in it.
const (
	firmlinks    = "/usr/share/firmlinks"
	dataTemplate = "/System/Library/Templates/Data"
)

// dataVolume is the mounts that make a host directory the machine's data volume: one for each
// path the release says is that volume's and whose directory its template has, read through to
// the template.
func dataVolume(image *rangefs.Image, host string) ([]Mount, error) {
	f, err := image.Open(rangefs.FileSystem, firmlinks)
	if err == nil && f == nil {
		err = fmt.Errorf("the release has no %s to say which paths are its data volume's, as a macOS release has", firmlinks)
	}
	var list []byte
	if err == nil {
		list, err = f.ReadAll()
	}
	if err != nil {
		return nil, fmt.Errorf("a data volume: %w", err)
	}
	var mounts []Mount
	for _, line := range strings.Split(string(list), "\n") {
		guest, rel, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		at := dataTemplate + "/" + rel
		if entry, err := image.Stat(rangefs.FileSystem, at); err != nil {
			return nil, fmt.Errorf("a data volume: %w", err)
		} else if entry.Kind != rangefs.Directory {
			continue // a path of the volume that a new one does not have
		}
		dir := filepath.Join(host, "volume", filepath.FromSlash(rel))
		if err := os.MkdirAll(dir, 0o777); err != nil {
			return nil, err
		}
		mounts = append(mounts, Mount{Guest: guest, Host: dir, Data: true, Record: filepath.Join(host, "filled"), Template: func(sub string) (TemplateEntry, error) {
			path := pathpkg.Join(at, sub)
			entry, err := image.Stat(rangefs.FileSystem, path)
			switch {
			case err != nil:
				return TemplateEntry{}, err
			case entry.Kind == rangefs.Directory:
				names, err := image.ReadDir(rangefs.FileSystem, path)
				return TemplateEntry{Dir: true, Names: names}, err
			case entry.Kind == rangefs.Symlink:
				return TemplateEntry{Link: entry.Target}, nil
			case entry.Kind == rangefs.Regular:
				return TemplateEntry{Size: entry.File.Size(), Read: entry.File.ReadAll, Mode: entry.File.Mode()}, nil
			}
			return TemplateEntry{}, nil
		}})
	}
	return mounts, nil
}

// launchdJob is what a launchd plist says of a daemon or an agent, of what is read here.
type launchdJob struct {
	Label                string            `plist:"Label"`
	Program              string            `plist:"Program"`
	ProgramArguments     []string          `plist:"ProgramArguments"`
	MachServices         map[string]any    `plist:"MachServices"`
	EnvironmentVariables map[string]string `plist:"EnvironmentVariables"`
	UserName             string            `plist:"UserName"`
	Disabled             bool              `plist:"Disabled"`
	agent                bool
}

// launcher finds the build's provider of a Mach service among its launchd plists, which are read
// once, when the first service is asked for: the daemons, which run as root or as the user their
// plist names, and the agents, which run as the guests' user. A daemon of a user the build's
// password file does not know, or whose program the system volume does not have, is not started.
func launcher(image *rangefs.Image, program func(string) ([]byte, bool, error), env []string) func(string, bool) (*Guest, error) {
	var daemons, agents map[string]*launchdJob
	users := map[string]Owner{}
	read := func(dir string, agent bool) (map[string]*launchdJob, error) {
		jobs := map[string]*launchdJob{}
		names, err := image.ReadDir(rangefs.FileSystem, dir)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			f, err := image.Open(rangefs.FileSystem, dir+"/"+name)
			if err != nil || f == nil || !strings.HasSuffix(name, ".plist") {
				continue
			}
			data, err := f.ReadAll()
			if err != nil {
				return nil, err
			}
			job := &launchdJob{agent: agent}
			if _, err := plist.Unmarshal(data, job); err != nil || job.Disabled {
				continue
			}
			for service := range job.MachServices {
				if jobs[service] == nil { // the first by name, for a run to repeat
					jobs[service] = job
				}
			}
		}
		return jobs, nil
	}
	return func(service string, system bool) (*Guest, error) {
		if daemons == nil {
			var err error
			if daemons, err = read("/System/Library/LaunchDaemons", false); err != nil {
				return nil, err
			}
			if agents, err = read("/System/Library/LaunchAgents", true); err != nil {
				return nil, err
			}
			if f, err := image.Open(rangefs.FileSystem, "/private/etc/passwd"); err == nil && f != nil {
				data, err := f.ReadAll()
				if err != nil {
					return nil, err
				}
				for _, line := range strings.Split(string(data), "\n") {
					if fields := strings.Split(line, ":"); len(fields) >= 4 && !strings.HasPrefix(line, "#") {
						uid, err1 := strconv.ParseUint(fields[2], 10, 32)
						gid, err2 := strconv.ParseUint(fields[3], 10, 32)
						if err1 == nil && err2 == nil {
							users[fields[0]] = Owner{uint32(uid), uint32(gid)}
						}
					}
				}
			}
		}
		job := daemons[service]
		if a := agents[service]; a != nil && !system {
			job = a
		}
		if job == nil {
			return nil, nil
		}
		args := job.ProgramArguments
		path := job.Program
		if path == "" && len(args) > 0 {
			path = args[0]
		}
		if len(args) > 0 {
			args = args[1:]
		}
		g := &Guest{Executable: path, Args: args, Root: !job.agent, Services: slices.Sorted(maps.Keys(job.MachServices))}
		if job.UserName != "" && job.UserName != "root" {
			owner, known := users[job.UserName]
			if !known {
				return nil, nil
			}
			g.Root, g.Owner = false, &owner
		}
		if path == "" {
			return nil, nil
		}
		var err error
		if g.Image, _, err = program(path); err != nil || g.Image == nil {
			return nil, err
		}
		g.Env = slices.Clone(env)
		for _, name := range slices.Sorted(maps.Keys(job.EnvironmentVariables)) {
			g.Env = append(g.Env, name+"="+job.EnvironmentVariables[name])
		}
		return g, nil
	}
}
