package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
		"one argument":  {[]string{url}, "expected a release and a guest executable, got 1 argument(s)"},
		"plain http":    {[]string{"http://example.invalid/x.ipsw", guest}, `must be an https:// URL, not "http://example.invalid/x.ipsw"`},
		"local image":   {[]string{"./x.ipsw", guest}, `must be an https:// URL, not "./x.ipsw"`},
		"shared home":   {[]string{"--home", "out", "15.0,24G90", guest}, "--home out would be shared by 2 releases; put {build} or {version} in it, as in --home 'out-{build}'"},
		"shared mount":  {[]string{"--mount", "/var/db=out", "15.0,24G90", guest}, "as in --mount '/var/db=out-{build}'"},
		"mount no host": {[]string{"--mount", "/var/db", url, guest}, "--mount /var/db is not <guest dir>=<host dir>"},
		"mount on home": {[]string{"--mount", "/Users=out", url, guest}, "would replace the guest's home or what is above it"},
		"shared report": {[]string{"--report", "r.json", "11.0.1,11.6,12.6", guest}, "as in --report 'r-{build}.json'"},
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

func TestReleasesByName(t *testing.T) {
	known := parseReleases(releasesText)
	builds := map[string]bool{}
	for _, r := range known {
		if builds[r.Build] {
			t.Errorf("releases.tsv: %s is listed twice", r.Build)
		}
		if !strings.HasPrefix(r.URL, "https://") || !strings.HasSuffix(r.URL, "_"+r.Version+"_"+r.Build+"_Restore.ipsw") {
			t.Errorf("releases.tsv: the URL of %s is not that release's image: %s", r.name(), r.URL)
		}
		builds[r.Build] = true
	}
	// What Apple offers once a release newer than the list is out, and a feed that cannot be read.
	newer := release{"27.3", "26D50", "https://example.invalid/UniversalMac_27.3_26D50_Restore.ipsw"}
	const url = "https://example.invalid/UniversalMac_Restore.ipsw"
	const everything = "20B29 20G165 21A559 21G115 22A380 22G120 23A344 23G93 24A335 24G90 25A354 25G83 26A428 26B131"
	data, err := os.ReadFile("testdata/index.json") // api.ipsw.me's reply for Macmini9,1 of 2026-11-19, the fields read
	if err != nil {
		t.Fatal(err)
	}
	every, err := parseIndex(data)
	if err != nil || len(every) != 80 || every[0].Build != "20B29" || every[79].Build != "26B131" {
		t.Fatalf("the index parsed to %d releases (error %v), want 80 from 20B29 to 26B131", len(every), err)
	}
	// Built in is all the index had that day, in its order; "all" is the ends of each major version.
	if !slices.Equal(known, every) {
		t.Errorf("releases.tsv has %d releases, and they are not the %d of the index oldest first", len(known), len(every))
	}
	var ends []string
	for _, r := range sampled(known) {
		ends = append(ends, r.Build)
	}
	if strings.Join(ends, " ") != everything {
		t.Errorf("the first and last of each major version are %q, want %q", ends, everything)
	}
	for _, r := range every {
		if !onApple(r.URL) {
			t.Errorf("the index's %s is not taken to be on Apple's hosts", r.URL)
		}
	}
	// What the index has once a release newer than those built in has come and gone.
	every = append(every, release{"27.2.2", "26B140", "https://updates.cdn-apple.com/x/UniversalMac_27.2.2_26B140_Restore.ipsw"})
	for _, c := range []struct {
		selector, want             string
		asks, down, looks, noIndex bool
	}{
		{selector: "15.6.1", want: "24G90"},
		{selector: "24g90", want: "24G90"},
		{selector: "27,11.6,26B131", want: "26A428 26B131 26D50 20G165", asks: true},
		{selector: "11.0.1," + url, want: "20B29 " + url},
		{selector: "12.6,http://x/a"},
		{selector: "15.0,,27.0", asks: true},
		{selector: "1", asks: true},
		{selector: "sequoia", asks: true},
		{selector: "15", want: "24A335 24G90", asks: true},
		{selector: "27.3", want: "26D50", asks: true},
		{selector: "26d50", want: "26D50", asks: true},
		{selector: "latest", want: "26D50", asks: true},
		{selector: "latest,27.3,all", want: "26D50 " + everything, asks: true},
		{selector: "all", want: everything + " 26D50", asks: true},
		// A release neither the list nor the feed names is whichever the index says it is.
		{selector: "15.2", want: "24C101"},
		{selector: "24c101", want: "24C101"},
		{selector: "27.2", want: "26B123"},
		{selector: "15.2,24G90,24C101,26.3.1,15", want: "24C101 24G90 25D2128 24A335", asks: true},
		{selector: "27.2.2", want: "26B140", asks: true, looks: true},
		{selector: "26b140", want: "26B140", asks: true, looks: true},
		{selector: "11.0", want: "20B29", asks: true, looks: true},
		{selector: "27.2.2,15,11.0", want: "26B140 24A335 24G90 20B29", asks: true, looks: true},
		{selector: "9.9", asks: true, looks: true},
		{selector: "24Z999", asks: true, looks: true},
		{selector: "27.2.2", asks: true, looks: true, noIndex: true},
		// Without the feed the list still answers for what it has, and says what it went without.
		{selector: "all", want: everything, asks: true, down: true},
		{selector: "27", want: "26A428 26B131", asks: true, down: true},
		{selector: "27.3", asks: true, down: true, looks: true},
		{selector: "latest", asks: true, down: true},
		{selector: "27.2.1", want: "26B131", down: true},
		{selector: "27.2.2", want: "26B140", asks: true, down: true, looks: true},
	} {
		asked, looked := 0, 0
		selected, note, err := selectReleases(known, c.selector, func() ([]release, error) {
			asked++
			if c.down {
				return nil, errors.New("feed is down")
			}
			return []release{newer}, nil
		}, func() ([]release, error) {
			looked++
			if c.noIndex {
				return nil, errors.New("index is down")
			}
			return every, nil
		})
		var got []string
		for _, r := range selected {
			got = append(got, cmp.Or(r.Build, r.URL))
		}
		if strings.Join(got, " ") != c.want || (err == nil) != (c.want != "") {
			t.Errorf("%s: selected %q (error %v), want %q", c.selector, got, err, c.want)
		}
		if asked > 1 || (asked == 1) != c.asks {
			t.Errorf("%s: Apple's feed was asked %d time(s), want asked %v", c.selector, asked, c.asks)
		}
		if looked > 1 || (looked == 1) != c.looks {
			t.Errorf("%s: the index was asked %d time(s), want asked %v", c.selector, looked, c.looks)
		}
		// The note is for a feed whose answer the releases selected could have depended on.
		unread := c.asks && c.down && !c.looks
		if err == nil && (note != nil) != unread {
			t.Errorf("%s: note %v, want one only for a feed that could not be read", c.selector, note)
		}
		if err != nil && c.asks && c.down && !strings.Contains(err.Error(), "feed is down") {
			t.Errorf("%s: the error does not say the feed could not be read: %v", c.selector, err)
		}
		// A name that could not be looked up is a failed run; a name nothing has is a bad command line.
		if err != nil && errors.Is(err, errSource) != (c.noIndex || c.selector == "latest") {
			t.Errorf("%s: wrong kind of error: %v", c.selector, err)
		}
	}
	// The index is someone else's: an image it places anywhere but on Apple's hosts is not fetched.
	for _, bad := range []string{"http://updates.cdn-apple.com/x.ipsw", "https://example.invalid/x.ipsw", "https://updates.cdn-apple.com.example.invalid/x.ipsw", "https://apple.com@example.invalid/x.ipsw"} {
		_, _, err := selectReleases(known, "27.2.2", func() ([]release, error) { return nil, nil },
			func() ([]release, error) { return []release{{"27.2.2", "26B140", bad}}, nil })
		if err == nil || !strings.Contains(err.Error(), "not an https URL on a host of Apple's") {
			t.Errorf("%s from the index: error %v, want it refused", bad, err)
		}
	}
	if _, err := parseIndex([]byte(`{"status":404,"message":"not found"}`)); err == nil {
		t.Error("a reply with no restore images was accepted as the index")
	}
	if !(release{Version: "13.6"}).compressed() || !(release{Version: "14.0"}).compressed() || (release{Version: "15.0"}).compressed() || (release{URL: url}).compressed() {
		t.Error("only macOS 13 and 14 releases are skipped without --stream-compressed")
	}
}

func TestFeed(t *testing.T) {
	data, err := os.ReadFile("testdata/feed.xml") // Apple's feed of 2026-11-09, three devices of it
	if err != nil {
		t.Fatal(err)
	}
	for device, want := range map[string]string{
		"Macmini9,1": "27.2.1 26B131 https://updates.cdn-apple.com/2026FallFCS/c202a724-db7e-4d56-917b-110aa0226507/UniversalMac_27.2.1_26B131_Restore.ipsw",
		"Mac17,10":   "27.2.1 26B8132 https://updates.cdn-apple.com/2026FallFCS/ca5a1489-416d-4d75-a33a-0214da765fd7/UniversalMac_27.2.1_26B8132_Restore.ipsw",
		"Macmini1,1": "",
	} {
		got, err := parseFeed(data, device)
		if want == "" {
			if err == nil {
				t.Errorf("%s: %v, want an error for a model the feed does not have", device, got)
			}
			continue
		}
		if err != nil || len(got) != 1 || got[0].Version+" "+got[0].Build+" "+got[0].URL != want {
			t.Errorf("%s: %v (error %v), want %q", device, got, err, want)
		}
	}
	if _, err := parseFeed([]byte("<h1>not found</h1>"), "Macmini9,1"); err == nil {
		t.Error("a page that is not the feed was accepted")
	}
	// Apple's current release is already the last one listed, so it is not listed twice.
	known := parseReleases(releasesText)
	current, _ := parseFeed(data, "Macmini9,1")
	if all := withCurrent(known, current); len(all) != len(known) {
		t.Errorf("%d releases with the feed's, %d without", len(all), len(known))
	}
}

func TestSection(t *testing.T) {
	first := outcome{name: "macOS 11.0.1 (20B29)", stdout: "zlib 1.2.11"}
	for want, c := range map[string]struct {
		out  outcome
		same *outcome
	}{
		"==> macOS 11.0.1 (20B29)\nzlib 1.2.11\n":                 {first, nil},
		"==> macOS 11.6 (20G165): same as macOS 11.0.1 (20B29)\n": {outcome{name: "macOS 11.6 (20G165)", stdout: "zlib 1.2.11"}, &first},
		"==> macOS 15.0 (24A335)\nzlib 1.2.12\n":                  {outcome{name: "macOS 15.0 (24A335)", stdout: "zlib 1.2.12\n"}, &first},
		"==> macOS 15.0 (24A335), exit 3\nzlib 1.2.11\n":          {outcome{name: "macOS 15.0 (24A335)", stdout: "zlib 1.2.11", code: 3}, &first},
		"==> macOS 15.0 (24A335): failed\nzlib 1.2.11\n":          {outcome{name: "macOS 15.0 (24A335)", stdout: "zlib 1.2.11", err: errors.New("stopped")}, &first},
		"==> macOS 15.0 (24A335)\n":                               {outcome{name: "macOS 15.0 (24A335)"}, &first},
	} {
		if got := section(c.out, c.same); got != want {
			t.Errorf("section %q, want %q", got, want)
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
		"status":        {&rangefs.StatusError{URL: "https://x", Status: "403 Forbidden"}, false, "ipswrun --releases"},
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
