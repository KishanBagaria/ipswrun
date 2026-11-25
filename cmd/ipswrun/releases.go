package main

import (
	"cmp"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/KishanBagaria/ipswrun/rangefs"
	"github.com/blacktop/go-plist"
)

// releases.tsv is every release ipswrun resolves without asking anyone, oldest first: each restore
// image Apple has published for Macmini9,1. Add a release when it ships; Apple's feed names it while
// it is the current one, and after that only the index does.
//
//go:embed releases.tsv
var releasesText string

// ios.tsv is the same for iOS and iPadOS from 14 on, for the iPhones from iPhone11,2 and iPads from
// iPad8,1 on: a snapshot of AppleDB's, with the columns of betas.tsv. One image restores several
// devices, and each device has its own images, so a release is kept for a --device only where
// that device is among those its image restores.
//
//go:embed ios.tsv
var iosText string

// release is a restore image to run against. One given as a bare URL or local file path has no
// version or build until its image has been opened.
type release struct {
	Version, Build, URL string
	OS                  string `json:",omitempty"` // "iOS", or empty for macOS
}

func (r release) local() bool {
	return r.URL != "" && !rangefs.IsRemote(r.URL)
}

func (r release) name() string {
	if r.Build == "" {
		return r.URL
	}
	return fmt.Sprintf("%s %s (%s)", cmp.Or(r.OS, "macOS"), r.Version, r.Build)
}

// compressed reports whether the release keeps its shared cache in a deflated ZIP member, which
// only --stream-compressed can read: macOS 13 and 14, and iOS 16 and 17.
func (r release) compressed() bool {
	if r.OS == iOS {
		return strings.HasPrefix(r.Version, "16.") || strings.HasPrefix(r.Version, "17.")
	}
	return strings.HasPrefix(r.Version, "13.") || strings.HasPrefix(r.Version, "14.")
}

// iOS is the OS of a release for an iPhone, iPad or iPod touch.
const iOS = "iOS"

// osOf is the OS of the releases for a --device: iOS for an iPhone, iPad or iPod touch, and
// otherwise macOS, written as empty.
func osOf(device string) string {
	for _, prefix := range []string{"iPhone", "iPad", "iPod"} {
		if strings.HasPrefix(device, prefix) {
			return iOS
		}
	}
	return ""
}

// oldestIOS is the first major version of iOS that ipswrun runs. The system volume of iOS 12 and
// 13 cannot be read by range (its root directory ends early), so they are left out of the
// releases of the index.
const oldestIOS = 14

// supportedIOS reports whether ipswrun runs an iOS version.
func supportedIOS(version string) bool {
	first, _, _ := strings.Cut(version, ".")
	major, err := strconv.Atoi(first)
	return err != nil || major >= oldestIOS
}

// splitSelector is the comma-separated parts of a release selector. The URL or path of an iOS
// image has commas of its own (iPhone11,2,iPhone11,4_18.7_22H20_Restore.ipsw): a part that is
// one runs on to the next that ends in .ipsw, if one does.
func splitSelector(selector string) []string {
	parts := strings.Split(selector, ",")
	var out []string
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if image := strings.Contains(part, "/") || strings.HasPrefix(part, "file:"); image && !strings.HasSuffix(part, ".ipsw") {
			for j := i + 1; j < len(parts); j++ {
				if strings.HasSuffix(parts[j], ".ipsw") {
					part, i = strings.Join(parts[i:j+1], ","), j
					break
				}
			}
		}
		out = append(out, part)
	}
	return out
}

// parseReleases reads releases.tsv: a line of column names, then one release a line.
func parseReleases(text string) []release {
	var out []release
	for _, line := range strings.Split(text, "\n")[1:] {
		if fields := strings.Split(line, "\t"); len(fields) == 3 {
			out = append(out, release{Version: fields[0], Build: fields[1], URL: fields[2]})
		}
	}
	return out
}

// sampled returns the first and last release of each major version: with the current one, what
// "all" runs.
func sampled(known []release) []release {
	major := func(i int) string {
		if i < 0 || i >= len(known) {
			return ""
		}
		major, _, _ := strings.Cut(known[i].Version, ".")
		return major
	}
	var out []release
	for i, r := range known {
		if major(i) != major(i-1) || major(i) != major(i+1) {
			out = append(out, r)
		}
	}
	return out
}

// feedURL is Apple's own list of the restore image each Mac is currently offered. It names the
// current release only.
const feedURL = "https://mesu.apple.com/assets/macos/com_apple_macOSIPSW/com_apple_macOSIPSW.xml"

// iosFeedURL is the same for each iPhone and iPad, in the same form: the list Finder and iTunes
// restore from.
const iosFeedURL = "https://itunes.apple.com/WebObjects/MZStore.woa/wa/com.apple.jingle.appserver.client.MZITunesClientCheck/version"

// indexURL is where the releases neither the releases built in nor the feed name are looked up,
// which are those of another --device and those that came and went after this ipswrun was built:
// ipsw.me's list of every restore image Apple has published for a device. Apple publishes no such
// list itself.
const indexURL = "https://api.ipsw.me/v4/device/%s?type=ipsw"

// errSource is in the error of a name only the feed or the index could have resolved: a failed
// run, not a bad command line.
var errSource = errors.New("could not be read")

// parseFeed returns the releases the feed offers device: one, unless Apple is offering several.
func parseFeed(data []byte, device string) ([]release, error) {
	// Walked as generic dictionaries: the library does not fill maps whose values are structs.
	var feed map[string]any
	if _, err := plist.Unmarshal(data, &feed); err != nil {
		return nil, err
	}
	dict := func(v any, key string) map[string]any {
		d, _ := v.(map[string]any)
		next, _ := d[key].(map[string]any)
		return next
	}
	var out []release
	seen := map[string]bool{}
	for _, version := range dict(feed, "MobileDeviceSoftwareVersionsByVersion") {
		for _, build := range dict(dict(version, "MobileDeviceSoftwareVersions"), device) {
			// Absent under "Unknown" and from a "SameAs", which repeat the builds. The iOS feed
			// also lists the image once under each build it restores over.
			restore := dict(build, "Restore")
			r := release{}
			r.Version, _ = restore["ProductVersion"].(string)
			r.Build, _ = restore["BuildVersion"].(string)
			r.URL, _ = restore["FirmwareURL"].(string)
			if r.Version != "" && r.Build != "" && strings.HasPrefix(r.URL, "https://") && !seen[r.URL] {
				seen[r.URL] = true
				out = append(out, r)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("it offers %s no restore image", device)
	}
	slices.SortFunc(out, func(a, b release) int { return strings.Compare(a.Build, b.Build) })
	return out, nil
}

// parseIndex returns every release the index has for a device, oldest first.
func parseIndex(data []byte) ([]release, error) {
	var reply struct {
		Firmwares []struct {
			Version string `json:"version"`
			Build   string `json:"buildid"`
			URL     string `json:"url"`
		} `json:"firmwares"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, err
	}
	var out []release
	for _, f := range reply.Firmwares {
		if f.Version != "" && f.Build != "" && f.URL != "" {
			out = append(out, release{Version: f.Version, Build: f.Build, URL: f.URL})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("it lists no restore image")
	}
	slices.SortFunc(out, func(a, b release) int {
		return cmp.Or(slices.Compare(versionNumbers(a.Version), versionNumbers(b.Version)), strings.Compare(a.Build, b.Build))
	})
	return out, nil
}

// versionNumbers is a version as numbers, so that 15.10 sorts after 15.9.
func versionNumbers(version string) []int {
	var out []int
	for _, field := range strings.Split(version, ".") {
		n, _ := strconv.Atoi(field)
		out = append(out, n)
	}
	return out
}

// onApple reports whether a URL the index gave is an https one on a host of Apple's. The index is
// not Apple's, and what it names is fetched and run, so nothing it says is followed elsewhere.
func onApple(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return strings.HasSuffix(host, ".apple.com") || strings.HasSuffix(host, ".cdn-apple.com")
}

func get(from string) ([]byte, error) {
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(from)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", from, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// fetchCurrent asks Apple which release device is currently offered.
func fetchCurrent(device string) ([]release, error) {
	from := feedURL
	if osOf(device) == iOS {
		from = iosFeedURL
	}
	data, err := get(from)
	if err != nil {
		return nil, err
	}
	return parseFeed(data, device)
}

// fetchIndex asks the index for every release of device.
func fetchIndex(device string) ([]release, error) {
	data, err := get(fmt.Sprintf(indexURL, url.PathEscape(device)))
	if err != nil {
		return nil, err
	}
	return parseIndex(data)
}

// betas.tsv is a snapshot of AppleDB's macOS beta restore images: the columns of releases.tsv, then
// the devices each image restores, separated by spaces.
//
//go:embed betas.tsv
var betasText string

// parseByDevice reads betas.tsv or ios.tsv, keeping the releases there are for device.
func parseByDevice(text, device string) []release {
	var out []release
	for _, line := range strings.Split(text, "\n")[1:] {
		if fields := strings.Split(line, "\t"); len(fields) == 4 && slices.Contains(strings.Fields(fields[3]), device) {
			out = append(out, release{Version: fields[0], Build: fields[1], URL: fields[2]})
		}
	}
	return out
}

// withCurrent adds the releases Apple currently offers to those of known that lack them.
func withCurrent(known, current []release) []release {
	out := slices.Clone(known)
	for _, r := range current {
		if !slices.ContainsFunc(known, func(k release) bool { return k.Build == r.Build }) {
			out = append(out, r)
		}
	}
	return out
}

var (
	versionName = regexp.MustCompile(`^\d+(\.\d+)+$`)
	buildName   = regexp.MustCompile(`^\d+[A-Za-z]\d+[a-z]?$`)
	// betaBuildName is a build that ends in a letter, as a beta's does, in either case.
	betaBuildName = regexp.MustCompile(`(?i)^\d+[a-z]\d+[a-z]$`)
)

func filter(pool []release, keep func(release) bool) []release {
	return slices.DeleteFunc(slices.Clone(pool), func(r release) bool { return !keep(r) })
}

// selectReleases turns the first argument into the releases it names, in the order named and
// each once. It is a comma-separated list of: "all", the first and last release of each major
// version and the current one; "latest"; a major version ("15"), which is those of "all" under it;
// "15.*", every release of that major version; a range "15.0..15.6.1" or "24A335..24G90", every
// release between the two inclusive; the version or build of any release ("15.2", "24C101"); a
// version no release has ("11.0"), which is every release under it; the build of a beta
// ("26C5047e"); the https URL of any image; or the path of a local .ipsw file.
//
// Nothing is fetched for a version or build in releases.tsv, a local file, nor for the build of a
// beta, which is one of betas or an error. Any other name asks current, at most once, for what
// Apple offers now, and a version or build that this does not settle either asks index, at most
// once, for every release there has been. When current fails and the names can do without it, the
// failure comes back as note.
func selectReleases(known, betas []release, selector string, current, index func() ([]release, error)) (out []release, note error, err error) {
	current, index = sync.OnceValues(current), sync.OnceValues(index)
	seen := map[string]bool{}
	for _, part := range splitSelector(selector) {
		exactly := named(part)
		under := func(r release) bool { return strings.HasPrefix(r.Version, part+".") }
		var feedErr error
		offered := func() []release {
			var offered []release
			offered, feedErr = current()
			return offered
		}
		noRelease := func(among string) error {
			err := fmt.Errorf("no release %q among %s; one that is not listed runs from the https URL or local path of its restore image", part, among)
			if feedErr != nil {
				err = fmt.Errorf("%w. Apple's feed, which names the current release, could not be read: %w", err, feedErr)
			}
			return err
		}
		var matched []release
		switch {
		case strings.HasPrefix(part, "https://"):
			matched = []release{{URL: part}}
		case strings.HasPrefix(part, "http://"):
			return nil, nil, fmt.Errorf("a restore image URL must be an https:// URL, not %q (plain http is not read)", part)
		case strings.HasPrefix(part, "file://") || strings.Contains(part, "/") || strings.HasSuffix(part, ".ipsw"):
			path := strings.TrimPrefix(part, "file://")
			st, statErr := os.Stat(path)
			if statErr != nil {
				return nil, nil, fmt.Errorf("restore image: %w", statErr)
			}
			if st.IsDir() {
				return nil, nil, fmt.Errorf("restore image: %s is a directory", path)
			}
			abs, absErr := filepath.Abs(path)
			if absErr != nil {
				return nil, nil, fmt.Errorf("restore image: %w", absErr)
			}
			matched = []release{{URL: abs}}
		case strings.Contains(part, ".."):
			from, to, _ := strings.Cut(part, "..")
			if from == "" || to == "" || strings.Contains(to, "..") {
				return nil, nil, fmt.Errorf("release range %q must be <from>..<to>, as in 15.0..15.6.1", part)
			}
			var rangeNote error
			matched, rangeNote, err = selectRange(known, betas, from, to, current, index)
			if err != nil {
				return nil, nil, err
			}
			note = cmp.Or(note, rangeNote)
		case strings.HasSuffix(part, ".*"):
			prefix := strings.TrimSuffix(part, "*")
			pool := withCurrent(known, offered())
			matched = filter(pool, func(r release) bool { return strings.HasPrefix(r.Version, prefix) })
			note = cmp.Or(note, feedErr)
			if len(matched) == 0 {
				return nil, nil, noRelease("the releases known for this --device")
			}
		case part == "latest":
			if matched = offered(); feedErr != nil {
				return nil, nil, latestErr(feedErr)
			}
		case slices.ContainsFunc(known, exactly):
			matched = filter(known, exactly)
		case betaBuildName.MatchString(part):
			if matched = filter(betas, exactly); len(matched) == 0 {
				return nil, nil, noRelease("the betas built in for this --device")
			}
		case slices.ContainsFunc(offered(), exactly):
			matched = filter(offered(), exactly)
		case versionName.MatchString(part) || buildName.MatchString(part):
			every, indexErr := index()
			if indexErr != nil {
				return nil, nil, fmt.Errorf("%q is not among the releases built in, so it is looked up in the index of every release at api.ipsw.me, which %w: %w\nfix: pass the https URL of its restore image instead", part, errSource, indexErr)
			}
			if matched = filter(every, exactly); len(matched) == 0 {
				matched = filter(every, under)
			}
			if len(matched) == 0 {
				return nil, nil, noRelease("the releases api.ipsw.me lists for this --device")
			}
			if err := offApple(matched); err != nil {
				return nil, nil, err
			}
		default: // "all", a major version, or nothing at all: only the releases with the feed's can say
			if matched = withCurrent(sampled(known), offered()); part != "all" {
				matched = filter(matched, under)
			}
			note = cmp.Or(note, feedErr)
		}
		if len(matched) == 0 {
			return nil, nil, noRelease("the first and last of each major version, which ipswrun --releases prints")
		}
		for _, r := range matched {
			if !seen[r.URL] {
				seen[r.URL] = true
				out = append(out, r)
			}
		}
	}
	return out, note, nil
}

// named matches a release by its version or build.
func named(name string) func(release) bool {
	return func(r release) bool { return name == r.Version || strings.EqualFold(name, r.Build) }
}

// lastIndex is the index of the last release that matches, or -1.
func lastIndex(rs []release, match func(release) bool) int {
	for k := len(rs) - 1; k >= 0; k-- {
		if match(rs[k]) {
			return k
		}
	}
	return -1
}

func latestErr(feedErr error) error {
	return fmt.Errorf("latest is what Apple's feed says it is, and the feed %w: %w", errSource, feedErr)
}

// offApple refuses the first release api.ipsw.me says is somewhere other than Apple's hosts.
func offApple(rs []release) error {
	for _, r := range rs {
		if !onApple(r.URL) {
			return fmt.Errorf("api.ipsw.me has %s at %s, which is not an https URL on a host of Apple's; pass that URL yourself if you trust it", r.name(), r.URL)
		}
	}
	return nil
}

// selectRange returns every release from from through to inclusive, in chronological order.
func selectRange(known, betas []release, from, to string, current, index func() ([]release, error)) ([]release, error, error) {
	missing := func(i int) string {
		if i < 0 {
			return from
		}
		return to
	}
	span := func(pool []release, i, j int, note error) ([]release, error, error) {
		if i > j {
			return nil, nil, fmt.Errorf("release range %s..%s: %s is after %s", from, to, from, to)
		}
		return slices.Clone(pool[i : j+1]), note, nil
	}
	if betaBuildName.MatchString(from) || betaBuildName.MatchString(to) {
		i, j := slices.IndexFunc(betas, named(from)), lastIndex(betas, named(to))
		if i < 0 || j < 0 {
			return nil, nil, fmt.Errorf("no beta release %q among the betas built in for this --device", missing(i))
		}
		return span(betas, i, j, nil)
	}
	bounds := func(pool, latest []release) (int, int) {
		at := func(name string, find func([]release, func(release) bool) int) int {
			if name != "latest" {
				return find(pool, named(name))
			}
			if len(latest) == 0 {
				return -1
			}
			build := latest[len(latest)-1].Build
			return slices.IndexFunc(pool, func(r release) bool { return r.Build == build })
		}
		return at(from, slices.IndexFunc[[]release]), at(to, lastIndex)
	}
	pool := known
	var latest []release
	var feedErr error
	i, j := bounds(pool, nil)
	if from == "latest" || to == "latest" || i < 0 || j < 0 {
		latest, feedErr = current()
		if (from == "latest" || to == "latest") && feedErr != nil {
			return nil, nil, latestErr(feedErr)
		}
		pool = withCurrent(known, latest)
		i, j = bounds(pool, latest)
	}
	if i < 0 || j < 0 {
		every, indexErr := index()
		if indexErr != nil {
			return nil, nil, fmt.Errorf("%q is not among the releases built in, so it is looked up in the index of every release at api.ipsw.me, which %w: %w", missing(i), errSource, indexErr)
		}
		pool = withCurrent(every, latest)
		i, j = bounds(pool, latest)
		if i >= 0 && j >= 0 && i <= j {
			if err := offApple(pool[i : j+1]); err != nil {
				return nil, nil, err
			}
		}
	}
	if i < 0 || j < 0 {
		return nil, nil, fmt.Errorf("no release %q for range %s..%s", missing(i), from, to)
	}
	return span(pool, i, j, feedErr)
}
