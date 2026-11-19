// ipswrun runs an arm64 Mach-O inside the userland of any macOS build, taken from that build's
// restore image (IPSW) over HTTP range requests. See the readme.
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/KishanBagaria/ipswrun/emulator"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

const issues = "https://github.com/KishanBagaria/ipswrun/issues"

// Unicorn needs the main thread's stack.
func init() { runtime.LockOSThread() }

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// report is what --report writes: enough to tie a run's output to the build that produced it.
type report struct {
	Version   string `json:"version"`
	Build     string `json:"build"`
	CacheUUID string `json:"cache_uuid"`
	ExitCode  int    `json:"exit_code"`
	Pages     int    `json:"guest_pages"`
	Requests  int64  `json:"range_requests"`
	Bytes     int64  `json:"bytes_fetched"`
	Seconds   int    `json:"seconds"`
}

func formatUUID(u [16]byte) string {
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}

const usage = "ipswrun [flags] <release> <guest executable> [guest arguments...]"

// checkArgs says what is wrong with the positional arguments, before anything is fetched for
// them, or returns the releases the first one names.
func checkArgs(known []release, args []string, current, index func() ([]release, error)) (selected []release, note error, err error) {
	if len(args) < 2 {
		return nil, nil, fmt.Errorf("expected a release and a guest executable, got %d argument(s)", len(args))
	}
	// The guest first: a name may need looking up, and a mistyped path needs nothing.
	if err := checkExecutable(args[1]); err != nil {
		return nil, nil, fmt.Errorf("guest executable: %w", err)
	}
	return selectReleases(known, args[0], current, index)
}

// checkExecutable says what is wrong with the path of an executable on the host. One that is
// taken from the release (system:/usr/sbin/cfprefsd) is looked for when the release is open.
func checkExecutable(path string) error {
	if rest, ok := strings.CutPrefix(path, emulator.SystemPrefix); ok {
		if !strings.HasPrefix(rest, "/") {
			return fmt.Errorf("%s is not %s and an absolute path in the release", path, emulator.SystemPrefix)
		}
		return nil
	}
	switch st, err := os.Stat(path); {
	case err != nil:
		return err
	case st.IsDir():
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

// feedNote says that Apple's feed could not be read, for a command that goes on without it.
func feedNote(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun: note: Apple's feed of the current release could not be read, so a release newer than those built in is not among the releases run: %v\n", err)
	}
}

// checkPerRelease refuses a --home or --report that several releases would share: each run would
// start in what the one before left, or overwrite it.
func checkPerRelease(releases int, flag, value string) error {
	if releases < 2 || value == "" || strings.Contains(value, "{build}") || strings.Contains(value, "{version}") {
		return nil
	}
	ext := filepath.Ext(value)
	return fmt.Errorf("--%s %s would be shared by %d releases; put {build} or {version} in it, as in --%s '%s-{build}%s'",
		flag, value, releases, flag, strings.TrimSuffix(value, ext), ext)
}

// parseMounts reads each --mount as <guest dir>=<host dir>, and refuses one that several releases
// would share or that the emulator would not take.
func parseMounts(releases int, values []string) (mounts []emulator.Mount, err error) {
	for _, value := range values {
		guest, host, ok := strings.Cut(value, "=")
		if !ok || host == "" {
			return nil, fmt.Errorf("--mount %s is not <guest dir>=<host dir>, as in --mount /var/db=out/db", value)
		}
		if err := checkPerRelease(releases, "mount", value); err != nil {
			return nil, err
		}
		mounts = append(mounts, emulator.Mount{Guest: guest, Host: host})
	}
	if err := emulator.CheckMounts(mounts); err != nil {
		return nil, fmt.Errorf("--mount: %w", err)
	}
	return mounts, nil
}

// parseDaemons reads each --daemon or --agent as <service>[,<service>...]=<executable>[ <argument>...].
func parseDaemons(flag string, values []string, root bool) (daemons []emulator.Daemon, err error) {
	for _, value := range values {
		names, command, ok := strings.Cut(value, "=")
		words := strings.Fields(command)
		if !ok || names == "" || len(words) == 0 {
			return nil, fmt.Errorf("--%s %s is not <service>[,<service>...]=<executable> [arguments], as in --%s 'com.example.echo=bin/echod --verbose'", flag, value, flag)
		}
		if err := checkExecutable(words[0]); err != nil {
			return nil, fmt.Errorf("--%s %s: %w", flag, value, err)
		}
		daemons = append(daemons, emulator.Daemon{Services: strings.Split(names, ","), Executable: words[0], Args: words[1:], Root: root})
	}
	return daemons, nil
}

// fix is the last line of a failure: what to do about it, for the failures where that is known.
func fix(err error, timeout float64, stream bool) string {
	var status *rangefs.StatusError
	var stopped *emulator.StopError
	switch {
	case errors.As(err, &status):
		return "fix: pass the direct https URL of a restore image (.ipsw), like those ipswrun --releases prints; retrying this URL will not help"
	case errors.Is(err, rangefs.ErrCompressedMember) && !stream:
		return "fix: rerun with --stream-compressed, which downloads that member whole (3.6-4.5 GB for macOS 13 and 14)"
	case errors.As(err, &stopped) && stopped.Kind == emulator.StopTimeout:
		return fmt.Sprintf("fix: rerun with a larger --timeout (it was %g seconds); what the guest waits for while its pages are fetched counts, and a rerun reuses what --cache kept", timeout)
	case errors.As(err, &stopped) && stopped.Kind == emulator.StopUnimplemented:
		return "fix: none on your side, and retrying will not help: the emulator does not implement this call yet. Report it with the output above at " + issues
	}
	return ""
}

// progress keeps one status line on a terminal while a run fetches. Everything else written to
// that terminal goes through Write, which takes the line away first. A progress with no terminal
// only passes writes on.
type progress struct {
	mu      sync.Mutex
	out     io.Writer
	live    bool
	shown   bool
	phase   string
	stats   func() (requests, bytes int64)
	started time.Time
	quit    chan struct{}
	done    sync.WaitGroup
}

func newProgress(out io.Writer, live bool) *progress {
	p := &progress{out: out, live: live, started: time.Now(), quit: make(chan struct{})}
	if live {
		p.done.Add(1)
		go func() {
			defer p.done.Done()
			tick := time.NewTicker(500 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-p.quit:
					return
				case <-tick.C:
					p.mu.Lock()
					if p.live && p.phase != "" {
						fmt.Fprint(p.out, "\r\x1b[K"+p.line())
						p.shown = true
					}
					p.mu.Unlock()
				}
			}
		}()
	}
	return p
}

func (p *progress) line() string {
	line := "ipswrun: " + p.phase
	if p.stats != nil {
		_, fetched := p.stats()
		line += fmt.Sprintf(", %.0f MB fetched", float64(fetched)/1e6)
	}
	return line + fmt.Sprintf(", %ds", int(time.Since(p.started).Seconds()))
}

// set names what the run is doing; stats, if not nil, is where the transfer so far is read.
func (p *progress) set(phase string, stats func() (int64, int64)) {
	p.mu.Lock()
	p.phase, p.stats = phase, stats
	p.mu.Unlock()
}

func (p *progress) clear() {
	if p.shown {
		fmt.Fprint(p.out, "\r\x1b[K")
		p.shown = false
	}
}

func (p *progress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
	return p.out.Write(b)
}

// stop removes the line for good.
func (p *progress) stop() {
	p.mu.Lock()
	wasLive := p.live
	p.live = false
	p.clear()
	p.mu.Unlock()
	if wasLive {
		close(p.quit)
		p.done.Wait()
	}
}

// options is what every release of a command line is run with.
type options struct {
	unicorn, cache, device    string
	home, report              string // may contain {build} and {version}
	guest                     string
	args, env, system         []string
	services                  []string
	daemons                   []emulator.Daemon
	mounts                    []emulator.Mount // the host side may contain {build} and {version}
	stream, trace, showStatus bool
	missing                   bool
	timeout                   float64
}

// outcome is one release's run. A run that failed may still have output from before it stopped.
type outcome struct {
	name           string // "macOS 15.6.1 (24G90)", or the URL of an image that never opened
	stdout, stderr string
	missing        string // under --missing, the calls the guest made that are not implemented
	code           int
	err            error
}

// runOne runs the guest against one release. label starts the status line.
func (o *options) runOne(r release, label string) (out outcome) {
	out.name = r.name()
	status := newProgress(os.Stderr, o.showStatus)
	defer status.stop()
	// A failure ends with what to do about it, after everything that describes it.
	failed := func(err error) outcome {
		if line := fix(err, o.timeout, o.stream); line != "" {
			err = fmt.Errorf("%w\n%s", err, line)
		}
		out.err = err
		return out
	}
	started := time.Now()
	status.set(label+"opening the restore image", nil)
	image, err := rangefs.Open(r.URL, o.cache, o.device)
	if err != nil {
		return failed(err)
	}
	if r.Build != "" && r.Build != image.Build() {
		return failed(fmt.Errorf("the image at %s is build %s, not the %s it is listed as", r.URL, image.Build(), r.Build))
	}
	if o.stream {
		image.AllowStreaming()
	}
	out.name = release{Version: image.Version(), Build: image.Build()}.name()
	perRelease := strings.NewReplacer("{build}", image.Build(), "{version}", image.Version())
	home := perRelease.Replace(o.home)
	if home == "" {
		if home, err = os.MkdirTemp("", "ipswrun-home-"); err != nil {
			return failed(err)
		}
		defer os.RemoveAll(home)
	}
	mounts := slices.Clone(o.mounts)
	for i := range mounts {
		mounts[i].Host = perRelease.Replace(mounts[i].Host)
		if err := os.MkdirAll(mounts[i].Host, 0o777); err != nil {
			return failed(err)
		}
	}
	status.set(label+out.name, image.Stats)
	machine, dyldCache, err := emulator.RunBuild(image, emulator.Run{Unicorn: o.unicorn, Home: home, Mounts: mounts, Services: o.services, Executable: o.guest, Args: o.args,
		Daemons: o.daemons, Env: o.env, System: o.system, Timeout: time.Duration(o.timeout * float64(time.Second)), Trace: o.trace, Survey: o.missing, Notes: status})
	status.stop()
	pages := 0
	if machine != nil {
		defer machine.Close()
		for i, process := range machine.Processes() {
			pages += process.MappedPages()
			// Of the guest, what it wrote where it wrote it. A daemon has nobody to write to: what
			// it wrote is shown on standard error, under its name.
			who, stderr := "the guest", process.Stderr()
			if i == 0 {
				out.stdout = process.Stdout()
			} else {
				who = process.Label()
				stderr = labelled(who, process.Stdout()+stderr)
				if code, exited := process.ExitCode(); exited {
					stderr += fmt.Sprintf("ipswrun: %s exited with status %d\n", who, code)
				}
			}
			if !o.trace { // tracing already echoed it
				out.stderr += stderr
			}
			if o.missing {
				if list := process.Unimplemented(); list != "" {
					out.missing += "ipswrun: " + out.name + ": not implemented, and answered " + who + " with an error:\n" + list + "\n"
				}
			}
		}
		if o.missing && out.missing == "" {
			out.missing = "ipswrun: " + out.name + ": every call made is implemented\n"
		}
	}
	if err != nil {
		var stopped *emulator.StopError
		if machine != nil && errors.As(err, &stopped) {
			err = fmt.Errorf("%w\n%s", err, machine.Diagnostics())
		}
		return failed(err)
	}
	out.code, _ = machine.Main().ExitCode()
	if o.report != "" {
		requests, fetched := image.Stats()
		data, _ := json.MarshalIndent(report{Version: image.Version(), Build: image.Build(), CacheUUID: formatUUID(dyldCache.UUID), ExitCode: out.code,
			Pages: pages, Requests: requests, Bytes: fetched, Seconds: int(time.Since(started).Seconds())}, "", "  ")
		if err := os.WriteFile(perRelease.Replace(o.report), append(data, '\n'), 0o666); err != nil {
			return failed(err)
		}
	}
	return out
}

// labelled is what a daemon wrote, each line under its name.
func labelled(name, text string) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	return name + ": " + strings.Join(lines, "\n"+name+": ") + "\n"
}

// repeats reports whether a run printed and exited as the earlier one did.
func (out outcome) repeats(earlier *outcome) bool {
	return earlier != nil && earlier.stdout == out.stdout && earlier.code == out.code
}

// section is how one of several releases appears on standard output: a line naming it, then what
// the guest printed there. A release whose output and exit status repeat those of same, the
// nearest earlier release that printed its own, says so instead of printing them again.
func section(out outcome, same *outcome) string {
	head := "==> " + out.name
	if out.err == nil && out.code != 0 {
		head += fmt.Sprintf(", exit %d", out.code)
	}
	switch {
	case out.err != nil:
		head += ": failed"
	case out.repeats(same):
		return head + ": same as " + same.name + "\n"
	}
	body := out.stdout
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return head + "\n" + body
}

// runAll runs the guest against each release in turn. One release behaves as ipswrun always
// has: the guest's output and nothing else, its exit status, an error if it never got to exit.
// Several are printed as sections, a failure does not stop the ones after it, and the status is
// 70 if any failed, else 1 if any guest exited with something other than 0.
func (o *options) runAll(releases []release) (int, error) {
	// Asking a compressed release for its cache would only fail, after fetching for it.
	var skipped []string
	if !o.stream {
		releases = slices.DeleteFunc(slices.Clone(releases), func(r release) bool {
			if r.compressed() {
				skipped = append(skipped, r.name())
			}
			return r.compressed()
		})
	}
	if len(skipped) > 0 {
		err := fmt.Errorf("not run: %s: the image of a macOS 13 or 14 release %w", strings.Join(skipped, ", "), rangefs.ErrCompressedMember)
		if len(releases) == 0 {
			return 0, fmt.Errorf("%w\n%s", err, fix(err, o.timeout, o.stream))
		}
		defer fmt.Fprintf(os.Stderr, "ipswrun: %v; pass --stream-compressed to run them too\n", err)
	}
	if len(releases) == 1 && len(skipped) == 0 {
		out := o.runOne(releases[0], "")
		os.Stdout.WriteString(out.stdout)
		os.Stderr.WriteString(out.stderr + out.missing)
		return out.code, out.err
	}
	var shown *outcome
	status := 0
	for i, r := range releases {
		out := o.runOne(r, fmt.Sprintf("%d/%d ", i+1, len(releases)))
		os.Stdout.WriteString(section(out, shown))
		os.Stderr.WriteString(out.stderr + out.missing)
		switch {
		case out.err != nil:
			fmt.Fprintf(os.Stderr, "ipswrun: %s: %v\n", out.name, out.err)
			status = 70
		case !out.repeats(shown):
			shown = &out
		}
		if out.code != 0 && status == 0 {
			status = 1
		}
	}
	return status, nil
}

// run returns the exit status, or an error when a single guest never got to exit.
func run() (int, error) {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: "+usage+`
       ipswrun --releases
       ipswrun --setup
<release> is the version or build of any macOS release since 11 (15.2, 24C101), latest, all (the first
and last release of each major version), a major version (15, for those of all under it), the https
URL of any restore image, or several of these separated by commas.`)
		flag.PrintDefaults()
	}
	home := flag.String("home", "", "host directory the guest sees as its home, "+emulator.GuestHome+"; {build} and {version} in it become the release's (default: a temporary one)")
	var env, system, mountValues, services, daemonValues, agentValues list
	flag.Var(&agentValues, "agent", "<service>[,<service>...]=<executable> [arguments]: another guest, started beside the first as the same user, that provides these Mach services to it (repeatable)")
	flag.Var(&daemonValues, "daemon", "like --agent, for a guest that runs as root, as a daemon of the system does (repeatable)")
	flag.Var(&services, "service", "name of a Mach service the guest provides, as a daemon does: it can check that name in, and look it up (repeatable)")
	flag.Var(&mountValues, "mount", "<guest dir>=<host dir>: a host directory the guest sees, and may change, at that guest path (repeatable); {build} and {version} in the host directory become the release's")
	flag.Var(&env, "env", "KEY=VALUE added to the guest environment (repeatable)")
	flag.Var(&system, "system", "system-volume file, or directory ending in /, the guest may read (repeatable; default: feature flags, SystemVersion.plist, /usr/share/icu/ and libobjc-trampolines.dylib; giving any replaces the default)")
	device := flag.String("device", "Macmini9,1", "BuildManifest product used to pick disk images")
	cache := flag.String("cache", "", "directory that keeps what was downloaded between runs (default: one in the system temporary directory, cleared on reboot)")
	stream := flag.Bool("stream-compressed", false, "download a compressed disk image member front to back when it cannot be read by range")
	timeout := flag.Float64("timeout", 900, "seconds the guest may run on each release, counted from when it starts (opening the image is not counted)")
	trace := flag.Bool("trace", false, "print every emulated system call")
	missing := flag.Bool("missing", false, "answer a system call or Mach trap that is not implemented with an error instead of stopping, and list them all when the guest ends")
	reportPath := flag.String("report", "", "write a JSON summary of the run (build, cache UUID, exit code, transfer) to this file; {build} and {version} in it become the release's")
	unicornDir := flag.String("unicorn", "", "directory the pinned libunicorn is kept in (default: the user cache directory)")
	setup := flag.Bool("setup", false, "only fetch and verify libunicorn")
	listReleases := flag.Bool("releases", false, "print the releases all stands for (version, build, URL): the first and last of each major version, and the one Apple currently offers")
	flag.Parse()

	known := parseReleases(releasesText)
	current := func() ([]release, error) { return fetchCurrent(*device) }
	index := func() ([]release, error) { return fetchIndex(*device) }
	if *listReleases {
		offered, err := current()
		feedNote(err)
		for _, r := range withCurrent(sampled(known), offered) {
			fmt.Printf("%s\t%s\t%s\n", r.Version, r.Build, r.URL)
		}
		return 0, nil
	}
	// Nothing is fetched for a command line that cannot run. --setup takes no arguments.
	var releases []release
	var mounts []emulator.Mount
	var daemons []emulator.Daemon
	if !*setup {
		if flag.NArg() == 0 {
			flag.Usage()
			return 2, nil
		}
		var note, err error
		if releases, note, err = checkArgs(known, flag.Args(), current, index); err == nil {
			feedNote(note)
			if err = cmp.Or(checkPerRelease(len(releases), "home", *home), checkPerRelease(len(releases), "report", *reportPath)); err == nil {
				mounts, err = parseMounts(len(releases), mountValues)
			}
			if err == nil {
				daemons, err = parseDaemons("agent", agentValues, false)
			}
			if err == nil {
				var asRoot []emulator.Daemon
				asRoot, err = parseDaemons("daemon", daemonValues, true)
				daemons = append(daemons, asRoot...)
			}
		}
		if errors.Is(err, errSource) {
			return 0, err
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ipswrun: %v\nusage: %s (--help lists the flags)\n", err, usage)
			return 2, nil
		}
	}
	// The status line is for someone watching a terminal: never when stderr is redirected, and
	// not under --trace, which writes there throughout.
	showStatus := !*trace && term.IsTerminal(int(os.Stderr.Fd()))

	if *unicornDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return 0, err
		}
		*unicornDir = filepath.Join(base, "ipswrun")
	}
	status := newProgress(os.Stderr, showStatus)
	status.set("preparing libunicorn", nil)
	unicorn, err := emulator.EnsureUnicorn(*unicornDir)
	status.stop()
	if err != nil || *setup {
		return 0, err
	}
	if *cache == "" {
		*cache = filepath.Join(os.TempDir(), fmt.Sprintf("ipswrun-%d", os.Getuid()))
		if err := os.MkdirAll(*cache, 0o700); err != nil {
			return 0, err
		}
	}
	o := &options{unicorn: unicorn, cache: *cache, device: *device, home: *home, report: *reportPath, guest: flag.Arg(1), args: flag.Args()[2:],
		env: env, system: system, mounts: mounts, services: services, daemons: daemons, stream: *stream, trace: *trace, missing: *missing, showStatus: showStatus, timeout: *timeout}
	return o.runAll(releases)
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun: %v\n", err)
		os.Exit(70)
	}
	os.Exit(code)
}
