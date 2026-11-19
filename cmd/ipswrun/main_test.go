package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KishanBagaria/ipswrun/emulator"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

// The test binary is ipswrun itself when asked to be, so that exit statuses and what reaches
// stderr are those of the command.
func TestMain(m *testing.M) {
	if os.Getenv("IPSWRUN_TEST_AS_COMMAND") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// command runs ipswrun with its libunicorn directory pointed at an empty one, and reports the
// exit status, stderr and whether anything was put in that directory.
func command(t *testing.T, args ...string) (status int, stderr string, fetched bool) {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], append([]string{"--unicorn", dir}, args...)...)
	cmd.Env = append(os.Environ(), "IPSWRUN_TEST_AS_COMMAND=1")
	var out bytes.Buffer
	cmd.Stderr = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	return cmd.ProcessState.ExitCode(), out.String(), len(entries) > 0
}

func TestArgumentsAreCheckedBeforeAnythingIsFetched(t *testing.T) {
	guest := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(guest, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	const url = "https://example.invalid/UniversalMac_Restore.ipsw"
	for name, c := range map[string]struct {
		args []string
		want string
	}{
		"nothing":       {nil, "usage: ipswrun"},
		"one argument":  {[]string{url}, "expected an IPSW URL and a guest executable, got 1 argument(s)"},
		"plain http":    {[]string{"http://example.invalid/x.ipsw", guest}, `must be an https:// URL, not "http://example.invalid/x.ipsw"`},
		"local image":   {[]string{"./x.ipsw", guest}, `must be an https:// URL, not "./x.ipsw"`},
		"missing guest": {[]string{url, guest + "-typo"}, "guest executable: stat " + guest + "-typo: no such file or directory"},
		"guest is dir":  {[]string{url, filepath.Dir(guest)}, "is a directory"},
	} {
		status, stderr, fetched := command(t, c.args...)
		if status != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d, stderr %q; want exit 2 and %q", name, status, stderr, c.want)
		}
		if fetched {
			t.Errorf("%s: libunicorn was fetched for a command line that cannot run", name)
		}
		if name != "nothing" && strings.Contains(stderr, "-stream-compressed") {
			t.Errorf("%s: the reason is buried under the flag list:\n%s", name, stderr)
		}
	}
}

// --setup takes no arguments (CI runs it that way), so the argument checks must not apply to it.
func TestSetupNeedsNoArguments(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads libunicorn")
	}
	status, stderr, fetched := command(t, "--setup")
	if status != 0 || !fetched {
		t.Fatalf("exit %d, fetched %v, stderr %q", status, fetched, stderr)
	}
}

func TestFix(t *testing.T) {
	compressed := fmt.Errorf("fs volume x.dmg: %w", fmt.Errorf("x.dmg is a compressed ZIP member (1 bytes) and %w", rangefs.ErrCompressedMember))
	for name, c := range map[string]struct {
		err    error
		stream bool
		want   string
	}{
		"status":        {&rangefs.StatusError{URL: "https://x", Status: "403 Forbidden"}, false, "examples/builds.txt"},
		"compressed":    {&emulator.StopError{Message: "no cache", Kind: emulator.StopNoCache, Cause: errors.Join(compressed)}, false, "--stream-compressed"},
		"streaming on":  {&emulator.StopError{Message: "no cache", Kind: emulator.StopNoCache, Cause: errors.Join(compressed)}, true, ""},
		"timeout":       {&emulator.StopError{Message: "timed out at 0x1", Kind: emulator.StopTimeout}, false, "larger --timeout (it was 900 seconds)"},
		"unimplemented": {fmt.Errorf("%w\n  backtrace", &emulator.StopError{Message: "unimplemented syscall 2 fork()", Kind: emulator.StopUnimplemented}), false, "retrying will not help"},
		"other stop":    {&emulator.StopError{Message: "guest raised signal 6"}, false, ""},
		"other error":   {errors.New("server does not advertise byte ranges"), false, ""},
	} {
		got := fix(c.err, 900, c.stream)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) || (got != "" && !strings.HasPrefix(got, "fix: ")) {
			t.Errorf("%s: fix %q, want one containing %q", name, got, c.want)
		}
	}
}

func TestProgressOnlyOnATerminal(t *testing.T) {
	var piped bytes.Buffer
	p := newProgress(&piped, false)
	p.set("macOS 15.6.1 (24G90)", func() (int64, int64) { return 3, 42e6 })
	time.Sleep(700 * time.Millisecond)
	fmt.Fprint(p, "note: one\n")
	p.stop()
	if piped.String() != "note: one\n" {
		t.Errorf("a redirected stderr got %q", piped.String())
	}

	out := &locked{}
	p = newProgress(out, true)
	p.set("macOS 15.6.1 (24G90)", func() (int64, int64) { return 3, 42e6 })
	time.Sleep(700 * time.Millisecond)
	fmt.Fprint(p, "note: one\n")
	p.stop()
	p.stop() // a second stop does nothing
	got := out.String()
	if !strings.Contains(got, "ipswrun: macOS 15.6.1 (24G90), 42 MB fetched, 0s") {
		t.Errorf("no status line in %q", got)
	}
	// The line is erased before anything else is written, and nothing is left of it at the end.
	if !strings.Contains(got, "\r\x1b[Knote: one\n") || !strings.HasSuffix(got, "note: one\n") {
		t.Errorf("the status line was not cleared around other output: %q", got)
	}
}

// locked is a buffer the status goroutine and the test may both touch; progress serialises its
// own writes, the test reads only after stop.
type locked struct{ bytes.Buffer }
