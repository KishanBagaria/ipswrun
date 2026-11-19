// ipswrun runs an arm64 Mach-O inside the userland of any macOS build, taken from that build's
// restore image (IPSW) over HTTP range requests. See the readme.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// checkArgs says what is wrong with the positional arguments, before anything is fetched for them.
func checkArgs(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("expected an IPSW URL and a guest executable, got %d argument(s)", len(args))
	}
	if !strings.HasPrefix(args[0], "https://") {
		return fmt.Errorf("the restore image must be an https:// URL, not %q (local files and plain http are not read)", args[0])
	}
	switch st, err := os.Stat(args[1]); {
	case err != nil:
		return fmt.Errorf("guest executable: %w", err)
	case st.IsDir():
		return fmt.Errorf("guest executable %s is a directory", args[1])
	}
	return nil
}

// fix is the last line of a failure: what to do about it, for the failures where that is known.
func fix(err error, timeout float64, stream bool) string {
	var status *rangefs.StatusError
	var stopped *emulator.StopError
	switch {
	case errors.As(err, &status):
		return "fix: pass the direct https URL of a restore image (.ipsw), like those in examples/builds.txt or listed by https://api.ipsw.me; retrying this URL will not help"
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

// run returns the guest's exit status, or an error when the guest never got to exit.
func run() (int, error) {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...]\n       ipswrun --setup")
		flag.PrintDefaults()
	}
	home := flag.String("home", "", "host directory the guest sees as its home, "+emulator.GuestHome+" (default: a temporary one)")
	var env, system list
	flag.Var(&env, "env", "KEY=VALUE added to the guest environment (repeatable)")
	flag.Var(&system, "system", "system-volume file, or directory ending in /, the guest may read (repeatable; default: feature flags, SystemVersion.plist, /usr/share/icu/ and libobjc-trampolines.dylib; giving any replaces the default)")
	device := flag.String("device", "Macmini9,1", "BuildManifest product used to pick disk images")
	cache := flag.String("cache", "", "directory that keeps what was downloaded between runs (default: one in the system temporary directory, cleared on reboot)")
	stream := flag.Bool("stream-compressed", false, "download a compressed disk image member front to back when it cannot be read by range")
	timeout := flag.Float64("timeout", 900, "seconds the guest may run, counted from when it starts (opening the image is not counted)")
	trace := flag.Bool("trace", false, "print every emulated system call")
	reportPath := flag.String("report", "", "write a JSON summary of the run (build, cache UUID, exit code, transfer) to this file")
	unicornDir := flag.String("unicorn", "", "directory the pinned libunicorn is kept in (default: the user cache directory)")
	setup := flag.Bool("setup", false, "only fetch and verify libunicorn")
	flag.Parse()

	// Nothing is fetched for a command line that cannot run. --setup takes no arguments.
	if !*setup {
		if flag.NArg() == 0 {
			flag.Usage()
			return 2, nil
		}
		if err := checkArgs(flag.Args()); err != nil {
			fmt.Fprintf(os.Stderr, "ipswrun: %v\nusage: ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...] (--help lists the flags)\n", err)
			return 2, nil
		}
	}
	// The status line is for someone watching a terminal: never when stderr is redirected, and
	// not under --trace, which writes there throughout.
	status := newProgress(os.Stderr, !*trace && term.IsTerminal(int(os.Stderr.Fd())))
	defer status.stop()

	if *unicornDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return 0, err
		}
		*unicornDir = filepath.Join(base, "ipswrun")
	}
	status.set("preparing libunicorn", nil)
	unicorn, err := emulator.EnsureUnicorn(*unicornDir)
	if err != nil || *setup {
		return 0, err
	}
	if *cache == "" {
		*cache = filepath.Join(os.TempDir(), fmt.Sprintf("ipswrun-%d", os.Getuid()))
		if err := os.MkdirAll(*cache, 0o700); err != nil {
			return 0, err
		}
	}
	if *home == "" {
		if *home, err = os.MkdirTemp("", "ipswrun-home-"); err != nil {
			return 0, err
		}
		defer os.RemoveAll(*home)
	}

	// A failure ends with what to do about it, after everything that describes it.
	failed := func(err error) (int, error) {
		if line := fix(err, *timeout, *stream); line != "" {
			err = fmt.Errorf("%w\n%s", err, line)
		}
		return 0, err
	}
	started := time.Now()
	status.set("opening the restore image", nil)
	image, err := rangefs.Open(flag.Arg(0), *cache, *device)
	if err != nil {
		return failed(err)
	}
	if *stream {
		image.AllowStreaming()
	}
	status.set(fmt.Sprintf("macOS %s (%s)", image.Version(), image.Build()), image.Stats)
	process, dyldCache, err := emulator.RunBuild(image, emulator.Run{Unicorn: unicorn, Home: *home, Executable: flag.Arg(1), Args: flag.Args()[2:],
		Env: env, System: system, Timeout: time.Duration(*timeout * float64(time.Second)), Trace: *trace, Notes: status})
	status.stop()
	if process != nil {
		defer process.Close()
		os.Stdout.WriteString(process.Stdout())
		if !*trace { // tracing already echoed it
			os.Stderr.WriteString(process.Stderr())
		}
	}
	if err != nil {
		var stopped *emulator.StopError
		if process != nil && errors.As(err, &stopped) {
			err = fmt.Errorf("%w\n%s", err, process.Diagnostics())
		}
		return failed(err)
	}
	code, _ := process.ExitCode()
	if *reportPath != "" {
		requests, fetched := image.Stats()
		data, _ := json.MarshalIndent(report{Version: image.Version(), Build: image.Build(), CacheUUID: formatUUID(dyldCache.UUID), ExitCode: code,
			Pages: process.MappedPages(), Requests: requests, Bytes: fetched, Seconds: int(time.Since(started).Seconds())}, "", "  ")
		if err := os.WriteFile(*reportPath, append(data, '\n'), 0o666); err != nil {
			return 0, err
		}
	}
	return code, nil
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ipswrun: %v\n", err)
		os.Exit(70)
	}
	os.Exit(code)
}
