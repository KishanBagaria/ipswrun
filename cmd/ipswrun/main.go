// ipswrun runs an arm64 Mach-O inside the userland of any macOS build, taken from that build's
// restore image (IPSW) over HTTP range requests. See the readme.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/KishanBagaria/ipswrun/emulator"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

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

// run returns the guest's exit status, or an error when the guest never got to exit.
func run() (int, error) {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: ipswrun [flags] <https IPSW url> <guest executable> [guest arguments...]\n       ipswrun --setup")
		flag.PrintDefaults()
	}
	home := flag.String("home", "", "host directory the guest sees as its home, "+emulator.GuestHome+" (default: a temporary one)")
	var env, system list
	flag.Var(&env, "env", "KEY=VALUE added to the guest environment (repeatable)")
	flag.Var(&system, "system", "system-volume file, or directory ending in /, the guest may read (repeatable; default: feature flags and SystemVersion.plist)")
	device := flag.String("device", "Macmini9,1", "BuildManifest product used to pick disk images")
	cache := flag.String("cache", "", "directory that keeps what was downloaded between runs (default: one in the system temporary directory, cleared on reboot)")
	stream := flag.Bool("stream-compressed", false, "download a compressed disk image member front to back when it cannot be read by range")
	timeout := flag.Float64("timeout", 900, "seconds allowed for the run")
	trace := flag.Bool("trace", false, "print every emulated system call")
	reportPath := flag.String("report", "", "write a JSON summary of the run (build, cache UUID, exit code, transfer) to this file")
	unicornDir := flag.String("unicorn", "", "directory the pinned libunicorn is kept in (default: the user cache directory)")
	setup := flag.Bool("setup", false, "only fetch and verify libunicorn")
	flag.Parse()

	if *unicornDir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return 0, err
		}
		*unicornDir = filepath.Join(base, "ipswrun")
	}
	unicorn, err := emulator.EnsureUnicorn(*unicornDir)
	if err != nil || *setup {
		return 0, err
	}
	if flag.NArg() < 2 || !strings.HasPrefix(flag.Arg(0), "https://") {
		flag.Usage()
		return 2, nil
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

	started := time.Now()
	image, err := rangefs.Open(flag.Arg(0), *cache, *device)
	if err != nil {
		return 0, err
	}
	if *stream {
		image.AllowStreaming()
	}
	process, dyldCache, err := emulator.RunBuild(image, emulator.Run{Unicorn: unicorn, Home: *home, Executable: flag.Arg(1), Args: flag.Args()[2:],
		Env: env, System: system, Timeout: time.Duration(*timeout * float64(time.Second)), Trace: *trace})
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
		return 0, err
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
