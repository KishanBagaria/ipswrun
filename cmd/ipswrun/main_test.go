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
	"sync/atomic"
	"testing"
	"time"

	"github.com/KishanBagaria/ipswrun/emulator"
	"github.com/KishanBagaria/ipswrun/rangefs"
)

// The test binary is ipswrun itself when asked to be, so that exit statuses and what reaches
// stderr are those of the command.
func TestMain(m *testing.M) {
	if os.Getenv("IPSWRUN_TEST_AS_COMMAND") == "1" || os.Getenv(workerEnv) == "1" {
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
		"nothing":             {nil, "usage: ipswrun"},
		"one argument":        {[]string{url}, "expected a release and a guest executable, got 1 argument(s)"},
		"plain http":          {[]string{"http://example.invalid/x.ipsw", guest}, `must be an https:// URL, not "http://example.invalid/x.ipsw"`},
		"missing local image": {[]string{"./x.ipsw", guest}, "restore image: stat ./x.ipsw: no such file or directory"},
		"local image is dir":  {[]string{filepath.Dir(guest), guest}, "is a directory"},
		"bad jobs":            {[]string{"--jobs", "0", url, guest}, "--jobs must be at least 1"},
		"bisect one release":  {[]string{"--bisect", "15.0", guest}, "--bisect needs at least 2 releases"},
		"reversed range":      {[]string{"15.6.1..15.0", guest}, "15.6.1 is after 15.0"},
		"shared home":         {[]string{"--home", "out", "15.0,24G90", guest}, "--home out would be shared by 2 releases; put {build} or {version} in it, as in --home 'out-{build}'"},
		"shared mount":        {[]string{"--mount", "/var/db=out", "15.0,24G90", guest}, "as in --mount '/var/db=out-{build}'"},
		"mount no host":       {[]string{"--mount", "/var/db", url, guest}, "--mount /var/db is not <guest dir>=<host dir>"},
		"mount on home":       {[]string{"--mount", "/Users/emu=out", url, guest}, "--mount: a mount at /Users/emu: that is the guest's home"},
		"shared report":       {[]string{"--report", "r.json", "11.0.1,11.6,12.6", guest}, "as in --report 'r-{build}.json'"},
		"missing guest":       {[]string{url, guest + "-typo"}, "guest executable: stat " + guest + "-typo: no such file or directory"},
		"guest is dir":        {[]string{url, filepath.Dir(guest)}, "is a directory"},
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

func TestCompilerBySuffix(t *testing.T) {
	for path, want := range map[string]string{
		"guest": "", "bin/guest.o": "", "system:/usr/share/x.c": "",
		"guest.c": "clang", "dir.d/guest.m": "clang", "guest.swift": "swiftc",
	} {
		if got := append(compiler(path, ""), "")[0]; got != want {
			t.Errorf("compiler(%q) runs %q, want %q", path, got, want)
		}
	}
}

func TestCompilerForIOS(t *testing.T) {
	for path, want := range map[string][]string{
		"guest.c":     {"--sdk", "iphoneos", "clang", "-arch", "arm64", "-mios-version-min=14.0", "-Wl,-no_fixup_chains"},
		"guest.swift": {"--sdk", "iphoneos", "swiftc", "-target", "arm64-apple-ios14.0", "-O", "-Xlinker", "-no_fixup_chains"},
	} {
		if got := compiler(path, iOS); !slices.Equal(got, want) {
			t.Errorf("compiler(%q, iOS) = %q, want %q", path, got, want)
		}
	}
	if got := compiler("guest.c", ""); slices.Contains(got, "iphoneos") {
		t.Errorf("a macOS guest is built against the iOS SDK: %q", got)
	}
}

func TestOSOfDevice(t *testing.T) {
	for device, want := range map[string]string{"Macmini9,1": "", "MacBookPro18,3": "", "VirtualMac2,1": "", "iPhone11,2": iOS, "iPad13,1": iOS, "iPod9,1": iOS} {
		if got := osOf(device); got != want {
			t.Errorf("osOf(%q) = %q, want %q", device, got, want)
		}
	}
}

func TestSplitSelectorKeepsCommasOfImages(t *testing.T) {
	const ios = "https://updates.cdn-apple.com/x/iPhone11,2,iPhone11,4,iPhone11,6_18.7.12_22H390_Restore.ipsw"
	for selector, want := range map[string][]string{
		"15.2,27.2.1":                      {"15.2", "27.2.1"},
		ios:                                {ios},
		"15.2," + ios + ",latest":          {"15.2", ios, "latest"},
		"/tmp/iPhone15,2_27.2.1.ipsw,26.1": {"/tmp/iPhone15,2_27.2.1.ipsw", "26.1"},
		// A path that never ends in .ipsw is only up to its comma, as before.
		"/tmp/image,15.2": {"/tmp/image", "15.2"},
	} {
		if got := splitSelector(selector); !slices.Equal(got, want) {
			t.Errorf("splitSelector(%q) = %q, want %q", selector, got, want)
		}
	}
}

func TestIOSReleases(t *testing.T) {
	r := release{Version: "17.7", Build: "21H16", OS: iOS}
	if r.name() != "iOS 17.7 (21H16)" || (release{Version: "15.6", Build: "24G84"}).name() != "macOS 15.6 (24G84)" {
		t.Errorf("names: %q", r.name())
	}
	for version, want := range map[string]bool{"15.8": false, "16.0": true, "17.7": true, "18.0": false, "26.1": false} {
		if got := (release{Version: version, OS: iOS}).compressed(); got != want {
			t.Errorf("iOS %s compressed() = %v, want %v", version, got, want)
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
	newer := release{Version: "27.3", Build: "26D50", URL: "https://example.invalid/UniversalMac_27.3_26D50_Restore.ipsw"}
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
	every = append(every, release{Version: "27.2.2", Build: "26B140", URL: "https://updates.cdn-apple.com/x/UniversalMac_27.2.2_26B140_Restore.ipsw"})
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
		selected, note, err := selectReleases(known, nil, c.selector, func() ([]release, error) {
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
		_, _, err := selectReleases(known, nil, "27.2.2", func() ([]release, error) { return nil, nil },
			func() ([]release, error) { return []release{{Version: "27.2.2", Build: "26B140", URL: bad}}, nil })
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

func TestIOSFeed(t *testing.T) {
	data, err := os.ReadFile("testdata/ios-feed.xml") // Apple's iOS feed of 2026-11-25, two devices of it
	if err != nil {
		t.Fatal(err)
	}
	for device, want := range map[string]string{
		"iPhone11,2": "18.7.12 22H390 https://updates.cdn-apple.com/2026WinterFCS/6cd704b1-a653-4be2-a909-507c6058dcbb/iPhone11,2,iPhone11,4,iPhone11,6_18.7.12_22H390_Restore.ipsw",
		"iPhone15,2": "27.2.1 24B156 https://updates.cdn-apple.com/2026FallFCS/c8706739-7c62-4ce1-82dd-5cae7bb63fbe/iPhone15,2_27.2.1_24B156_Restore.ipsw",
	} {
		// The image is listed under each build it restores over, and comes back once.
		got, err := parseFeed(data, device)
		if err != nil || len(got) != 1 || got[0].Version+" "+got[0].Build+" "+got[0].URL != want {
			t.Errorf("%s: %v (error %v), want %q", device, got, err, want)
		}
		// The current release is already the last one built in.
		known := parseByDevice(iosText, device)
		if len(known) == 0 || known[len(known)-1].URL != got[0].URL || len(withCurrent(known, got)) != len(known) {
			t.Errorf("%s: the last of %d releases built in is not the feed's", device, len(known))
		}
	}
}

func TestIOSBuiltIn(t *testing.T) {
	offline := func() ([]release, error) {
		t.Fatal("a release built in was looked up")
		return nil, nil
	}
	known := parseByDevice(iosText, "iPhone11,2")
	got, _, err := selectReleases(known, nil, "15.7,18.0", offline, offline)
	if err != nil || len(got) != 2 || got[0].Build != "19H12" || got[1].Build != "22A3354" || !strings.Contains(got[0].URL, "iPhone11,2") {
		t.Errorf("15.7,18.0 on iPhone11,2: %v (error %v)", got, err)
	}
	for _, r := range parseByDevice(iosText, "") {
		t.Errorf("a release for no device: %v", r)
	}
	for _, line := range strings.Split(strings.TrimSpace(iosText), "\n")[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || !onApple(fields[2]) || !supportedIOS(fields[0]) {
			t.Errorf("ios.tsv: %q", line)
		}
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
	p.set("macOS 15.6.1 (24G90)", func() (int64, int64) { return 3, 42e6 }, nil)
	time.Sleep(700 * time.Millisecond)
	fmt.Fprint(p, "note: one\n")
	p.stop()
	if piped.String() != "note: one\n" {
		t.Errorf("a redirected stderr got %q", piped.String())
	}

	out := &locked{}
	p = newProgress(out, true)
	p.set("macOS 15.6.1 (24G90)", func() (int64, int64) { return 3, 42e6 }, nil)
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

	// Streaming progress replaces "MB fetched" while total > 0 and reverts once done.
	p = newProgress(&bytes.Buffer{}, false)
	var streaming atomic.Bool
	streaming.Store(true)
	p.set("macOS 14.7 (23H124)", func() (int64, int64) { return 3, 42e6 }, func() (int64, int64) {
		if streaming.Load() {
			return 1240e6, 4100e6
		}
		return 0, 0
	})
	if line := p.line(); !strings.Contains(line, "streaming 1240/4100 MB (30%)") {
		t.Errorf("streaming line = %q", line)
	}
	streaming.Store(false)
	if line := p.line(); !strings.Contains(line, "42 MB fetched") {
		t.Errorf("post-stream line = %q", line)
	}
}

func TestPruneCacheFlag(t *testing.T) {
	cacheDir := t.TempDir()
	cached := filepath.Join(cacheDir, "0123456789abcdef")
	if err := os.MkdirAll(cached, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cached, "data"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	headFile := filepath.Join(cacheDir, "head-0123456789abcdef")
	if err := os.WriteFile(headFile, []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keptFile := filepath.Join(cacheDir, "kept-0123456789abcdef")
	if err := os.WriteFile(keptFile, []byte("a reading"), 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(cacheDir, "keep.txt")
	if err := os.WriteFile(keep, []byte("stay"), 0o644); err != nil {
		t.Fatal(err)
	}

	status, stderr, fetched := command(t, "--cache", cacheDir, "--prune-cache")
	if status != 0 || fetched {
		t.Fatalf("status=%d fetched=%v stderr=%q", status, fetched, stderr)
	}
	if _, err := os.Stat(cached); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cached release dir still exists: %v", err)
	}
	if _, err := os.Stat(headFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("head file still exists: %v", err)
	}
	if _, err := os.Stat(keptFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("kept reading still exists: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file was deleted: %v", err)
	}
}

// locked is a buffer the status goroutine and the test may both touch; progress serialises its
// own writes, the test reads only after stop.
type locked struct{ bytes.Buffer }

// A beta is one of those built in for the device, or an error: nothing is fetched for it.
func TestBetaSelectionOffline(t *testing.T) {
	offline := func() ([]release, error) {
		t.Fatal("beta selection attempted a network lookup")
		return nil, nil
	}
	got, _, err := selectReleases(nil, parseByDevice(betasText, "Macmini9,1"), "26c5047E", offline, offline)
	if err != nil || len(got) != 1 || got[0].Build != "26C5047e" || !onApple(got[0].URL) {
		t.Fatalf("beta selection returned %v, %v", got, err)
	}
	if _, _, err := selectReleases(nil, parseByDevice(betasText, "unsupported"), "26C5047e", offline, offline); err == nil {
		t.Fatal("accepted a beta for an unsupported device")
	}
}

func TestLocalIPSWAndRangeSelection(t *testing.T) {
	offline := func() ([]release, error) {
		t.Fatal("unexpected network lookup")
		return nil, nil
	}
	local := filepath.Join(t.TempDir(), "test.ipsw")
	if err := os.WriteFile(local, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(local)
	for _, sel := range []string{local, "file://" + local} {
		got, _, err := selectReleases(nil, nil, sel, offline, offline)
		if err != nil || len(got) != 1 || got[0].URL != abs || !got[0].local() {
			t.Fatalf("selectReleases(%q) = %+v, %v", sel, got, err)
		}
	}

	known := parseReleases(releasesText)
	got, _, err := selectReleases(known, nil, "15.0..15.2", offline, offline)
	if err != nil {
		t.Fatalf("15.0..15.2: %v", err)
	}
	var builds []string
	for _, r := range got {
		builds = append(builds, r.Build)
	}
	if len(builds) < 3 || builds[0] != "24A335" || builds[len(builds)-1] != "24C101" {
		t.Fatalf("15.0..15.2 selected %v", builds)
	}

	// Build-ID range works offline too.
	got2, _, err := selectReleases(known, nil, "24a335..24c101", offline, offline)
	if err != nil || !slices.Equal(got, got2) {
		t.Fatalf("24a335..24c101 = %v (err %v), want %v", got2, err, got)
	}

	// Wildcard major version 15.* returns every macOS 15 release in releases.tsv.
	all15, _, err := selectReleases(known, nil, "15.*", func() ([]release, error) { return nil, nil }, offline)
	if err != nil || len(all15) <= 2 || all15[0].Build != "24A335" || all15[len(all15)-1].Build != "24G90" {
		t.Fatalf("15.* selected %d releases (%v), err %v", len(all15), all15, err)
	}

	// Beta build range resolves offline.
	betas := parseByDevice(betasText, "Macmini9,1")
	if len(betas) >= 3 {
		bgot, _, err := selectReleases(known, betas, betas[0].Build+".."+betas[2].Build, offline, offline)
		if err != nil || len(bgot) != 3 {
			t.Fatalf("beta range returned %d releases, err %v", len(bgot), err)
		}
	}
}

func TestBisectReleases(t *testing.T) {
	makeReleases := func(n int) []release {
		out := make([]release, n)
		for i := range out {
			out[i] = release{Version: fmt.Sprintf("15.%d", i), Build: fmt.Sprintf("24A%02d", i), URL: fmt.Sprintf("https://example.invalid/%d.ipsw", i)}
		}
		return out
	}

	// Finds exact transition in 16 releases using O(log N) runs.
	rels := makeReleases(16)
	var runs int
	var stdout, stderr bytes.Buffer
	status := bisectReleases(rels, func(r release, _ string) outcome {
		runs++
		idx := slices.IndexFunc(rels, func(x release) bool { return x.Build == r.Build })
		if idx < 11 {
			return outcome{name: r.name(), stdout: "old\n", code: 0}
		}
		return outcome{name: r.name(), stdout: "new\n", code: 0}
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("bisect status = %d, want 0; out=%q err=%q", status, stdout.String(), stderr.String())
	}
	if runs > 7 {
		t.Errorf("bisect took %d runs on 16 releases, want <= 7", runs)
	}
	wantSummary := "bisect: changed between macOS 15.10 (24A10) and macOS 15.11 (24A11)"
	if !strings.Contains(stdout.String(), wantSummary) {
		t.Errorf("bisect output %q does not contain %q", stdout.String(), wantSummary)
	}

	// No change between endpoints stops after 2 runs and exits 1.
	stdout.Reset()
	stderr.Reset()
	runs = 0
	status = bisectReleases(rels, func(r release, _ string) outcome {
		runs++
		return outcome{name: r.name(), stdout: "same\n", code: 0}
	}, &stdout, &stderr)
	if status != 1 || runs != 2 || !strings.Contains(stdout.String(), "bisect: no change between") {
		t.Errorf("no-change bisect: status=%d runs=%d out=%q", status, runs, stdout.String())
	}

	// Intermediate failure skips the failing build and still narrows around it.
	stdout.Reset()
	stderr.Reset()
	status = bisectReleases(rels[:6], func(r release, _ string) outcome {
		idx := slices.IndexFunc(rels[:6], func(x release) bool { return x.Build == r.Build })
		if idx == 2 {
			return outcome{name: r.name(), err: errors.New("boom")}
		}
		if idx < 3 {
			return outcome{name: r.name(), stdout: "old\n"}
		}
		return outcome{name: r.name(), stdout: "new\n"}
	}, &stdout, &stderr)
	if status != 70 || !strings.Contains(stdout.String(), "bisect: narrowed to macOS 15.1 (24A01) .. macOS 15.3 (24A03); 1 release(s) between them failed") {
		t.Errorf("intermediate-failure bisect: status=%d out=%q err=%q", status, stdout.String(), stderr.String())
	}
}

func TestRunParallelOrdersOutputAndRunsConcurrently(t *testing.T) {
	rels := []release{
		{Version: "11.0.1", Build: "20B29", URL: "https://example.invalid/0"},
		{Version: "11.6", Build: "20G165", URL: "https://example.invalid/1"},
		{Version: "15.0", Build: "24A335", URL: "https://example.invalid/2"},
		{Version: "15.6.1", Build: "24G90", URL: "https://example.invalid/3"},
	}
	var active, peak atomic.Int32
	var stdout, stderr bytes.Buffer
	o := &options{jobs: 3}
	status := o.runParallel(rels, func(r release) outcome {
		cur := active.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		defer active.Add(-1)
		// Reverse delays so later releases finish before earlier ones.
		idx := slices.IndexFunc(rels, func(x release) bool { return x.Build == r.Build })
		time.Sleep(time.Duration(4-idx) * 25 * time.Millisecond)
		if idx < 2 {
			return outcome{name: r.name(), stdout: "v1\n", code: 0}
		}
		return outcome{name: r.name(), stdout: "v2\n", code: 0}
	}, &stdout, &stderr)
	if status != 0 {
		t.Fatalf("runParallel status = %d, stderr = %q", status, stderr.String())
	}
	if peak.Load() < 2 {
		t.Errorf("peak concurrency = %d, want >= 2", peak.Load())
	}
	want := "==> macOS 11.0.1 (20B29)\nv1\n" +
		"==> macOS 11.6 (20G165): same as macOS 11.0.1 (20B29)\n" +
		"==> macOS 15.0 (24A335)\nv2\n" +
		"==> macOS 15.6.1 (24G90): same as macOS 15.0 (24A335)\n"
	if stdout.String() != want {
		t.Errorf("runParallel stdout:\n%s\nwant:\n%s", stdout.String(), want)
	}
}

func TestWorkerSubprocessProtocol(t *testing.T) {
	o := &options{runOptions: runOptions{Device: "Macmini9,1", Timeout: 10}}
	missing := filepath.Join(t.TempDir(), "does-not-exist.ipsw")
	out := o.runWorkerProcess(release{URL: missing})
	if out.err == nil || !strings.Contains(out.err.Error(), "no such file or directory") {
		t.Fatalf("worker subprocess returned out=%+v, want missing file error", out)
	}
}
