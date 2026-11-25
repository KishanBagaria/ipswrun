// ipswrun runs an arm64 Mach-O inside the userland of any macOS or iOS build, taken from that
// build's restore image (IPSW) over HTTP range requests. See the readme.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
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

const usage = "ipswrun [flags] <release> <guest executable or source> [guest arguments...]"

// checkArgs says what is wrong with the positional arguments, before anything is fetched for
// them, or returns the releases the first one names.
func checkArgs(known, betas []release, args []string, current, index func() ([]release, error)) (selected []release, note error, err error) {
	if len(args) < 2 {
		return nil, nil, fmt.Errorf("expected a release and a guest executable, got %d argument(s)", len(args))
	}
	// The guest first: a name may need looking up, and a mistyped path needs nothing.
	if err := checkExecutable(args[1]); err != nil {
		return nil, nil, fmt.Errorf("guest executable: %w", err)
	}
	return selectReleases(known, betas, args[0], current, index)
}

// checkExecutable says what is wrong with the path of an executable on the host, or of a source
// file to build one from. One that is taken from the release (system:/usr/libexec/dirhelper) is
// looked for when the release is open.
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
	case compiler(path, "") != nil && runtime.GOOS != "darwin":
		return fmt.Errorf("compiling %s needs Xcode on a Mac; pass a prebuilt arm64 Mach-O on this host", path)
	}
	return nil
}

// compiler is the command xcrun runs to build an executable for system (see osOf) from a single
// source file, or nil for a path that is not one. The flags are those of examples/Makefile, and
// are to be kept the same as its. An iOS guest is built against the iOS SDK, which comes with
// Xcode and not with the command line tools alone.
func compiler(path, system string) []string {
	if strings.HasPrefix(path, emulator.SystemPrefix) {
		return nil
	}
	sdk, minimum, target := []string{}, "-mmacosx-version-min=11.0", "arm64-apple-macos11.0"
	if system == iOS {
		sdk, minimum, target = []string{"--sdk", "iphoneos"}, "-mios-version-min=14.0", "arm64-apple-ios14.0"
	}
	clang := append(sdk, "clang", "-arch", "arm64", minimum, "-Wl,-no_fixup_chains")
	switch filepath.Ext(path) {
	case ".c":
		return clang
	case ".m":
		return append(clang, "-fobjc-arc", "-O1", "-framework", "Foundation")
	case ".swift":
		return append(sdk, "swiftc", "-target", target, "-O", "-Xlinker", "-no_fixup_chains")
	}
	return nil
}

// compile returns the path of an executable built for system from path, if that is a source file,
// and path itself if not. It is built once, for every release it is to run on.
func compile(path, system string) (string, error) {
	build := compiler(path, system)
	if build == nil {
		return path, nil
	}
	// A directory each, as two sources may share a name, and under the name make would give it.
	dir, err := tempDir("build-")
	if err != nil {
		return "", err
	}
	binary := filepath.Join(dir, strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)))
	cmd := exec.Command("xcrun", append(build, "-o", binary, path)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("compiling %s with %s: %w", path, strings.Join(build, " "), err)
	}
	return binary, nil
}

// temps is the directory every temporary one of this process is in, made when the first is.
var temps struct {
	sync.Mutex
	root string
}

// tempDir makes a temporary directory, which removeTemps removes if nothing does before.
func tempDir(pattern string) (dir string, err error) {
	temps.Lock()
	defer temps.Unlock()
	if temps.root == "" {
		if temps.root, err = os.MkdirTemp("", "ipswrun-"); err != nil {
			return "", err
		}
	}
	return os.MkdirTemp(temps.root, pattern)
}

// removeTemps removes every temporary directory, at the end of the process.
func removeTemps() {
	temps.Lock()
	defer temps.Unlock()
	if temps.root != "" {
		os.RemoveAll(temps.root)
	}
}

// activeWorkers tracks running worker subprocesses so a terminating signal cleans them up.
var activeWorkers struct {
	sync.Mutex
	cmds []*exec.Cmd
}

func trackWorker(cmd *exec.Cmd) {
	activeWorkers.Lock()
	activeWorkers.cmds = append(activeWorkers.cmds, cmd)
	activeWorkers.Unlock()
}

func untrackWorker(cmd *exec.Cmd) {
	activeWorkers.Lock()
	activeWorkers.cmds = slices.DeleteFunc(activeWorkers.cmds, func(c *exec.Cmd) bool { return c == cmd })
	activeWorkers.Unlock()
}

func killWorkers(sig syscall.Signal) {
	activeWorkers.Lock()
	defer activeWorkers.Unlock()
	for _, c := range activeWorkers.cmds {
		if c.Process != nil {
			_ = c.Process.Signal(sig)
		}
	}
}

// dieOnSignal ends the process at Ctrl-C or a request to terminate, as the signal alone would,
// but leaving no temporary directory behind.
func dieOnSignal() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		received := (<-signals).(syscall.Signal)
		killWorkers(received)
		removeTemps()
		// To die of the signal is what tells a shell to stop its loop. The exit is for a
		// process that was started with the signal ignored, as a background job is, and waits
		// because the signal is not taken at once.
		signal.Reset(received)
		syscall.Kill(os.Getpid(), received)
		time.Sleep(time.Second)
		os.Exit(128 + int(received))
	}()
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
// would share or that the emulator would not take beside the guest's home.
func parseMounts(releases int, values []string, home string) (mounts []emulator.Mount, err error) {
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
	if err := emulator.CheckMounts(mounts, home); err != nil {
		return nil, fmt.Errorf("--mount: %w", err)
	}
	return mounts, nil
}

// parseDaemons reads each --daemon or --agent as <service>[,<service>...]=<executable>[ <argument>...].
func parseDaemons(flag string, values []string) (daemons []emulator.Guest, err error) {
	for _, value := range values {
		names, command, ok := strings.Cut(value, "=")
		words := strings.Fields(command)
		if !ok || names == "" || len(words) == 0 {
			return nil, fmt.Errorf("--%s %s is not <service>[,<service>...]=<executable> [arguments], as in --%s 'com.example.echo=bin/echod --verbose'", flag, value, flag)
		}
		if err := checkExecutable(words[0]); err != nil {
			return nil, fmt.Errorf("--%s %s: %w", flag, value, err)
		}
		daemons = append(daemons, emulator.Guest{Services: strings.Split(names, ","), Executable: words[0], Args: words[1:], Root: flag == "daemon"})
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
		return "fix: rerun with --stream-compressed, which downloads that member whole (3.6-4.5 GB for macOS 13 and 14, 1.1-1.4 GB for iOS 16 and 17)"
	case errors.As(err, &stopped) && stopped.Kind == emulator.StopTimeout:
		return fmt.Sprintf("fix: rerun with a larger --timeout (it was %g seconds); the guest had all of it, as time spent fetching its pages is not counted", timeout)
	case errors.As(err, &stopped) && stopped.Kind == emulator.StopUnimplemented:
		return "fix: none on your side, and retrying will not help: the emulator does not implement this call yet. Report it with the output above at " + issues
	}
	return ""
}

// live is where a guest's output goes as it is written: to out, with the status line taken away
// first. Nil, for output that is kept until the run ends, if out is.
func live(status *progress, out io.Writer) io.Writer {
	if out == nil {
		return nil
	}
	return writerFunc(func(b []byte) (int, error) {
		status.erase()
		return out.Write(b)
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }

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
	stream  func() (done, total int64)
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
	var done, total int64
	if p.stream != nil {
		done, total = p.stream()
	}
	if total > 0 {
		line += fmt.Sprintf(", streaming %.0f/%.0f MB (%.0f%%)", float64(done)/1e6, float64(total)/1e6, float64(done)*100/float64(total))
	} else if p.stats != nil {
		_, fetched := p.stats()
		line += fmt.Sprintf(", %.0f MB fetched", float64(fetched)/1e6)
	}
	return line + fmt.Sprintf(", %ds", int(time.Since(p.started).Seconds()))
}

// set names what the run is doing; stats and stream, if not nil, report transfer and streaming progress.
func (p *progress) set(phase string, stats, stream func() (int64, int64)) {
	p.mu.Lock()
	p.phase, p.stats, p.stream = phase, stats, stream
	p.mu.Unlock()
}

func (p *progress) clear() {
	if p.shown {
		fmt.Fprint(p.out, "\r\x1b[K")
		p.shown = false
	}
}

func (p *progress) erase() {
	p.mu.Lock()
	p.clear()
	p.mu.Unlock()
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
	runOptions
	showStatus, bisect bool
	jobs               int
	// Where the guest's output goes as it is written, for the one run of a command line that
	// has one; nil for output that is kept until the run ends.
	liveStdout, liveStderr io.Writer
}

// runOptions is what one release is run with: all a worker is sent.
type runOptions struct {
	Unicorn, Cache, Device string
	Home, Report, Data     string // may contain {build} and {version}
	Guest                  string
	Args, Env, System      []string
	Services               []string
	Daemons                []emulator.Guest
	Mounts                 []emulator.Mount // the host side may contain {build} and {version}
	Stream, Trace, Missing bool
	OnDemand               []string
	Timeout                float64
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
		if line := fix(err, o.Timeout, o.Stream); line != "" {
			err = fmt.Errorf("%w\n%s", err, line)
		}
		out.err = err
		return out
	}
	started := time.Now()
	status.set(label+"opening the restore image", nil, nil)
	image, err := rangefs.Open(r.URL, o.Cache, o.Device)
	if err != nil {
		return failed(err)
	}
	defer image.Close()
	if r.Build != "" && r.Build != image.Build() {
		return failed(fmt.Errorf("the image at %s is build %s, not the %s it is listed as", r.URL, image.Build(), r.Build))
	}
	if o.Stream {
		image.AllowStreaming()
	}
	out.name = release{Version: image.Version(), Build: image.Build(), OS: r.OS}.name()
	perRelease := strings.NewReplacer("{build}", image.Build(), "{version}", image.Version())
	home := perRelease.Replace(o.Home)
	if home == "" {
		if home, err = tempDir("home-"); err != nil {
			return failed(err)
		}
		defer os.RemoveAll(home)
	}
	data := perRelease.Replace(o.Data)
	mounts := slices.Clone(o.Mounts)
	for i := range mounts {
		mounts[i].Host = perRelease.Replace(mounts[i].Host)
		if err := os.MkdirAll(mounts[i].Host, 0o777); err != nil {
			return failed(err)
		}
	}
	var stats, stream func() (int64, int64)
	if !r.local() {
		stats, stream = image.Stats, image.StreamProgress
	}
	status.set(label+out.name, stats, stream)
	machine, dyldCache, err := emulator.RunBuild(image, emulator.Run{Unicorn: o.Unicorn, Home: home, Data: data, Mounts: mounts, Services: o.Services, Executable: o.Guest, Args: o.Args,
		Daemons: o.Daemons, Env: o.Env, System: o.System, Timeout: time.Duration(o.Timeout * float64(time.Second)), Trace: o.Trace, Survey: o.Missing, OnDemand: o.OnDemand, Notes: status,
		Stdout: live(status, o.liveStdout), Stderr: live(status, o.liveStderr)})
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
				if code, _ := process.ExitCode(); process.LeftEarly() {
					stderr += fmt.Sprintf("ipswrun: %s exited with status %d while the guest was running\n", who, code)
				}
			}
			if !o.Trace { // tracing already echoed it
				out.stderr += stderr
			}
			if o.Missing {
				if list := process.Unimplemented(); list != "" {
					out.missing += "ipswrun: " + out.name + ": not implemented, and answered " + who + " with an error:\n" + list + "\n"
				}
			}
		}
		if o.Missing && out.missing == "" {
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
	if sig := machine.Main().Signal(); sig != 0 {
		out.stderr += fmt.Sprintf("ipswrun: %s: the guest was ended by signal %d\n", out.name, sig)
		if crash := machine.Main().Crash(); crash != "" {
			out.stderr += "ipswrun: " + crash + "\n"
		}
	}
	if o.Report != "" {
		requests, fetched := image.Stats()
		data, _ := json.MarshalIndent(report{Version: image.Version(), Build: image.Build(), CacheUUID: formatUUID(dyldCache.UUID), ExitCode: out.code,
			Pages: pages, Requests: requests, Bytes: fetched, Seconds: int(time.Since(started).Seconds())}, "", "  ")
		if err := os.WriteFile(perRelease.Replace(o.Report), append(data, '\n'), 0o666); err != nil {
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

// runAll runs the guest against each release in turn (or concurrently with --jobs, or by binary
// search with --bisect). One release behaves as ipswrun always has: the guest's output and
// nothing else, its exit status, an error if it never got to exit. Several are printed as
// sections, a failure does not stop the ones after it, and the status is 70 if any failed, else 1
// if any guest exited with something other than 0.
func (o *options) runAll(releases []release) (int, error) {
	// Asking a compressed release for its cache would only fail, after fetching for it.
	var skipped []string
	if !o.Stream {
		releases = slices.DeleteFunc(slices.Clone(releases), func(r release) bool {
			if r.compressed() {
				skipped = append(skipped, r.name())
			}
			return r.compressed()
		})
	}
	if len(skipped) > 0 {
		err := fmt.Errorf("not run: %s: the image keeps its shared cache in a compressed ZIP member, which %w", strings.Join(skipped, ", "), rangefs.ErrCompressedMember)
		if len(releases) == 0 {
			return 0, fmt.Errorf("%w\n%s", err, fix(err, o.Timeout, o.Stream))
		}
		defer fmt.Fprintf(os.Stderr, "ipswrun: %v; pass --stream-compressed to run them too\n", err)
	}
	if o.bisect {
		if len(releases) < 2 {
			return 0, fmt.Errorf("--bisect needs at least 2 runnable releases (got %d after skipping compressed releases)", len(releases))
		}
		return bisectReleases(releases, o.runOne, os.Stdout, os.Stderr), nil
	}
	if len(releases) == 1 && len(skipped) == 0 {
		// One run: what the guest writes is passed on as it writes it. A trace already echoes it.
		if !o.Trace {
			o.liveStdout, o.liveStderr = os.Stdout, os.Stderr
		}
		out := o.runOne(releases[0], "")
		os.Stdout.WriteString(out.stdout)
		os.Stderr.WriteString(out.stderr + out.missing)
		return out.code, out.err
	}
	if o.jobs > 1 && !o.Trace && len(releases) > 1 {
		return o.runParallel(releases, o.runWorkerProcess, os.Stdout, os.Stderr), nil
	}
	shown := sections{stdout: os.Stdout, stderr: os.Stderr}
	for i, r := range releases {
		shown.add(o.runOne(r, fmt.Sprintf("%d/%d ", i+1, len(releases))))
	}
	return shown.status, nil
}

// sections prints runs in release order, each run whose output repeats the last one shown as only
// a line saying so, and keeps the exit status: 70 if a run failed, 1 if a guest exited nonzero.
type sections struct {
	stdout, stderr io.Writer
	shown          *outcome
	status         int
}

func (s *sections) add(out outcome) {
	fmt.Fprint(s.stdout, section(out, s.shown))
	fmt.Fprint(s.stderr, out.stderr+out.missing)
	switch {
	case out.err != nil:
		fmt.Fprintf(s.stderr, "ipswrun: %s: %v\n", out.name, out.err)
		s.status = 70
	case !out.repeats(s.shown):
		s.shown = &out
	}
	if out.code != 0 && s.status == 0 {
		s.status = 1
	}
}

// bisectReleases binary-searches releases for the first release whose output or exit status
// differs from the first release's. It returns 0 when a change is found, 1 when both endpoints
// behave identically, and 70 if an endpoint or intermediate run failed.
func bisectReleases(releases []release, run func(release, string) outcome, stdout, stderr io.Writer) int {
	results := make([]outcome, len(releases))
	tried := make([]bool, len(releases))
	runs := 0
	anyErr := false
	eval := func(i int) outcome {
		runs++
		out := run(releases[i], fmt.Sprintf("bisect %d/%d ", runs, len(releases)))
		results[i], tried[i] = out, true
		fmt.Fprint(stdout, section(out, nil))
		fmt.Fprint(stderr, out.stderr+out.missing)
		if out.err != nil {
			fmt.Fprintf(stderr, "ipswrun: %s: %v\n", out.name, out.err)
			anyErr = true
		}
		return out
	}

	lo, hi := 0, len(releases)-1
	first := eval(lo)
	last := eval(hi)
	if first.err != nil || last.err != nil {
		return 70
	}
	if last.repeats(&first) {
		fmt.Fprintf(stdout, "bisect: no change between %s and %s (%d of %d releases run)\n", first.name, last.name, runs, len(releases))
		return 1
	}

	for hi-lo > 1 {
		mid := pickMid(lo, hi, tried)
		if mid < 0 {
			break
		}
		out := eval(mid)
		if out.err != nil {
			continue
		}
		if out.repeats(&results[lo]) {
			lo = mid
		} else {
			hi = mid
		}
	}
	if hi-lo == 1 {
		fmt.Fprintf(stdout, "bisect: changed between %s and %s (%d of %d releases run)\n", results[lo].name, results[hi].name, runs, len(releases))
	} else {
		fmt.Fprintf(stdout, "bisect: narrowed to %s .. %s; %d release(s) between them failed (%d of %d releases run)\n",
			results[lo].name, results[hi].name, hi-lo-1, runs, len(releases))
	}
	if anyErr {
		return 70
	}
	return 0
}

// pickMid chooses an untested index strictly between lo and hi, closest to the midpoint.
func pickMid(lo, hi int, tried []bool) int {
	center := lo + (hi-lo)/2
	for delta := 0; center-delta > lo || center+delta < hi; delta++ {
		if i := center + delta; i > lo && i < hi && !tried[i] {
			return i
		}
		if delta > 0 {
			if i := center - delta; i > lo && i < hi && !tried[i] {
				return i
			}
		}
	}
	return -1
}

// workerEnv switches the executable into single-release worker mode for --jobs > 1, giving each
// concurrent Unicorn instance its own process and main OS thread.
const workerEnv = "IPSWRUN_WORKER"

type workerJob struct {
	Options runOptions `json:"options"`
	Release release    `json:"release"`
}

type workerResult struct {
	Name    string `json:"name"`
	Stdout  string `json:"stdout"`
	Stderr  string `json:"stderr"`
	Missing string `json:"missing"`
	Code    int    `json:"code"`
	Err     string `json:"err,omitempty"`
}

func (o *options) runWorkerProcess(r release) outcome {
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	payload, err := json.Marshal(workerJob{Options: o.runOptions, Release: r})
	if err != nil {
		return outcome{name: r.name(), err: err}
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), workerEnv+"=1")
	cmd.Stdin = bytes.NewReader(payload)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if err := cmd.Start(); err != nil {
		return outcome{name: r.name(), err: fmt.Errorf("worker start: %w", err)}
	}
	trackWorker(cmd)
	err = cmd.Wait()
	untrackWorker(cmd)
	if err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return outcome{name: r.name(), err: fmt.Errorf("worker failed: %v: %s", err, msg)}
		}
		return outcome{name: r.name(), err: fmt.Errorf("worker failed: %w", err)}
	}
	var res workerResult
	if err := json.Unmarshal(outBuf.Bytes(), &res); err != nil {
		return outcome{name: r.name(), err: fmt.Errorf("worker reply: %w", err)}
	}
	out := outcome{name: res.Name, stdout: res.Stdout, stderr: res.Stderr, missing: res.Missing, code: res.Code}
	if res.Err != "" {
		out.err = errors.New(res.Err)
	}
	return out
}

func runWorker() int {
	dieOnSignal()
	defer removeTemps()
	var job workerJob
	if err := json.NewDecoder(os.Stdin).Decode(&job); err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun worker: %v\n", err)
		return 70
	}
	out := (&options{runOptions: job.Options}).runOne(job.Release, "")
	res := workerResult{Name: out.name, Stdout: out.stdout, Stderr: out.stderr, Missing: out.missing, Code: out.code}
	if out.err != nil {
		res.Err = out.err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun worker: %v\n", err)
		return 70
	}
	return 0
}

// runParallel runs releases with up to o.jobs concurrent workers, printing sections in release
// order as contiguous results arrive so output and "same as" deduplication match serial runs.
func (o *options) runParallel(releases []release, run func(release) outcome, stdout, stderr io.Writer) int {
	prog := newProgress(stderr, o.showStatus)
	defer prog.stop()
	results := make([]outcome, len(releases))
	ready := make([]bool, len(releases))
	var (
		mu        sync.Mutex
		nextPrint int
		done      int
		shown     = sections{stdout: stdout, stderr: prog}
	)
	updateStatus := func(running int) {
		prog.set(fmt.Sprintf("running %d release(s) (%d/%d done)", running, done, len(releases)), nil, nil)
	}
	flush := func() {
		for nextPrint < len(releases) && ready[nextPrint] {
			prog.erase()
			shown.add(results[nextPrint])
			nextPrint++
		}
	}

	sem := make(chan struct{}, min(o.jobs, len(releases)))
	var wg sync.WaitGroup
	active := 0
	for i, r := range releases {
		sem <- struct{}{}
		mu.Lock()
		active++
		updateStatus(active)
		mu.Unlock()
		wg.Add(1)
		go func(i int, r release) {
			defer wg.Done()
			defer func() { <-sem }()
			out := run(r)
			mu.Lock()
			results[i], ready[i] = out, true
			active--
			done++
			flush()
			if active > 0 {
				updateStatus(active)
			}
			mu.Unlock()
		}(i, r)
	}
	wg.Wait()
	return shown.status
}

// defaultCacheDir returns the persistent per-user cache directory for downloaded IPSW ranges,
// falling back to the system temporary directory if the user cache directory is unavailable.
func defaultCacheDir() string {
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, "ipswrun", "cache")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("ipswrun-%d", os.Getuid()))
}

// run returns the exit status, or an error when a single guest never got to exit.
func run() (int, error) {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: "+usage+`
       ipswrun --releases
       ipswrun --prune-cache
       ipswrun --setup
<release> is the version or build of any macOS release since 11 (15.2, 24C101), latest, all (the first
and last release of each major version), a major version (15, for those of all under it, or 15.* for
every release of it), a range (15.0..15.6.1), the https URL or local path of any restore image, or
several of these separated by commas. With an iPhone or iPad --device (iPhone11,2) they are the
releases of iOS for it.`)
		flag.PrintDefaults()
	}
	data := flag.String("data", "", "host directory to be the machine's data volume, where a device keeps what changes (/Library, /private/var, /Users and the rest a macOS release names): what the release has for a new one, and then what the guests make of it, kept for the next run; {build} and {version} in it become the release's (default: none)")
	home := flag.String("home", "", "host directory the guest sees as its home, "+emulator.GuestHome+" ("+emulator.GuestHomeIOS+" on iOS); {build} and {version} in it become the release's (default: a temporary one)")
	var env, system, mountValues, services, daemonValues, agentValues, onDemand list
	flag.Var(&agentValues, "agent", "<service>[,<service>...]=<executable> [arguments]: another guest, started beside the first as the same user, that provides these Mach services to it; its executable may be a source file, as the first's may (repeatable)")
	flag.Var(&daemonValues, "daemon", "like --agent, for a guest that runs as root, as a daemon of the system does (repeatable)")
	flag.Var(&onDemand, "on-demand", "<service>: start the release's own daemon or agent for this Mach service when it is first looked up, as its launchd plist says to; a pattern names several (com.apple.cfprefsd.*), and * every one (repeatable)")
	flag.Var(&services, "service", "name of a Mach service the guest provides, as a daemon does: it can check that name in, and look it up (repeatable)")
	flag.Var(&mountValues, "mount", "<guest dir>=<host dir>: a host directory the guest sees, and may change, at that guest path (repeatable); {build} and {version} in the host directory become the release's")
	flag.Var(&env, "env", "KEY=VALUE added to the environment of every guest, or KEY alone taken out of it (repeatable)")
	flag.Var(&system, "system", "system-volume file, or directory ending in /, the guest may read (repeatable; default: feature flags, SystemVersion.plist, /usr/share/icu/ and libobjc-trampolines.dylib; giving any replaces the default)")
	device := flag.String("device", "Macmini9,1", "BuildManifest product used to pick disk images and the releases looked up; an iPhone or iPad (iPhone11,2) runs iOS")
	cache := flag.String("cache", "", "directory that keeps what was downloaded between runs (default: the user cache directory)")
	pruneCache := flag.Bool("prune-cache", false, "remove cached restore image downloads from --cache (skipping any a running ipswrun has open), then exit")
	stream := flag.Bool("stream-compressed", false, "download a compressed disk image member front to back when it cannot be read by range")
	timeout := flag.Float64("timeout", 900, "seconds the guest may run on each release, counted from when it starts (opening the image and fetching its pages are not counted)")
	trace := flag.Bool("trace", false, "print every emulated system call")
	missing := flag.Bool("missing", false, "answer a system call or Mach trap that is not implemented with an error instead of stopping, and list them all when the guest ends")
	bisect := flag.Bool("bisect", false, "binary-search the selected releases for where the guest's output or exit status changed")
	jobs := flag.Int("jobs", 1, "releases to run at once when running several")
	reportPath := flag.String("report", "", "write a JSON summary of the run (build, cache UUID, exit code, transfer) to this file; {build} and {version} in it become the release's")
	unicornDir := flag.String("unicorn", "", "directory the pinned libunicorn is kept in (default: the user cache directory)")
	setup := flag.Bool("setup", false, "only fetch and verify libunicorn")
	listReleases := flag.Bool("releases", false, "print the releases all stands for (version, build, URL): the first and last of each major version, and the one Apple currently offers")
	flag.Parse()
	defaultCache := *cache == ""
	if defaultCache {
		*cache = defaultCacheDir()
	}

	known := parseReleases(releasesText)
	current := func() ([]release, error) { return fetchCurrent(*device) }
	index := func() ([]release, error) { return fetchIndex(*device) }
	guestOS := osOf(*device)
	if guestOS == iOS {
		if err := checkDevice(*device); err != nil {
			fmt.Fprintf(os.Stderr, "ipswrun: %v\n", err)
			return 2, nil
		}
		known = parseByDevice(iosText, *device)
		index = func() ([]release, error) {
			every, err := fetchIndex(*device)
			return filter(every, func(r release) bool { return supportedIOS(r.Version) }), err
		}
	}
	if *listReleases {
		offered, err := current()
		feedNote(err)
		for _, r := range withCurrent(sampled(known), offered) {
			fmt.Printf("%s\t%s\t%s\n", r.Version, r.Build, r.URL)
		}
		return 0, nil
	}
	if *pruneCache {
		removed, inUse, freed, err := rangefs.PruneCache(*cache)
		if err != nil {
			return 0, err
		}
		fmt.Printf("removed %d cached image(s) (%.1f MB) from %s\n", removed, float64(freed)/1e6, *cache)
		if inUse > 0 {
			fmt.Printf("left %d image(s) in use by a running ipswrun\n", inUse)
		}
		return 0, nil
	}
	// Nothing is fetched for a command line that cannot run. --setup takes no arguments.
	var releases []release
	var guest string
	var mounts []emulator.Mount
	var daemons []emulator.Guest
	if !*setup {
		if flag.NArg() == 0 {
			flag.Usage()
			return 2, nil
		}
		var note, err error
		if *jobs < 1 {
			err = fmt.Errorf("--jobs must be at least 1, got %d", *jobs)
		}
		if err == nil {
			releases, note, err = checkArgs(known, parseByDevice(betasText, *device), flag.Args(), current, index)
		}
		if err == nil {
			feedNote(note)
			if *bisect && len(releases) < 2 {
				err = fmt.Errorf("--bisect needs at least 2 releases, got %d", len(releases))
			}
			if err == nil {
				err = cmp.Or(checkPerRelease(len(releases), "home", *home), checkPerRelease(len(releases), "data", *data), checkPerRelease(len(releases), "report", *reportPath))
			}
			if err == nil {
				mounts, err = parseMounts(len(releases), mountValues, emulator.HomeOf(platformOf(guestOS)))
			}
			if err == nil {
				daemons, err = parseDaemons("agent", agentValues)
			}
			if err == nil {
				var more []emulator.Guest
				more, err = parseDaemons("daemon", daemonValues)
				daemons = append(daemons, more...)
			}
		}
		if errors.Is(err, errSource) {
			return 0, err
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ipswrun: %v\nusage: %s (--help lists the flags)\n", err, usage)
			return 2, nil
		}
		for i := range releases {
			releases[i].OS = guestOS
		}
		if guest, err = compile(flag.Arg(1), guestOS); err != nil {
			return 0, err
		}
		for i := range daemons {
			if daemons[i].Executable, err = compile(daemons[i].Executable, guestOS); err != nil {
				return 0, err
			}
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
	status.set("preparing libunicorn", nil, nil)
	unicorn, err := emulator.EnsureUnicorn(*unicornDir)
	status.stop()
	if err != nil || *setup {
		return 0, err
	}
	if defaultCache {
		if err := os.MkdirAll(*cache, 0o700); err != nil {
			return 0, err
		}
	}
	o := &options{runOptions: runOptions{Unicorn: unicorn, Cache: *cache, Device: *device, Home: *home, Data: *data, Report: *reportPath, Guest: guest, Args: flag.Args()[2:],
		Env: env, System: system, Mounts: mounts, Services: services, Daemons: daemons, Stream: *stream, Trace: *trace, Missing: *missing, OnDemand: onDemand, Timeout: *timeout},
		bisect: *bisect, jobs: *jobs, showStatus: showStatus}
	return o.runAll(releases)
}

func main() {
	if os.Getenv(workerEnv) == "1" {
		os.Exit(runWorker())
	}
	dieOnSignal()
	code, err := run()
	removeTemps()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun: %v\n", err)
		os.Exit(70)
	}
	os.Exit(code)
}
