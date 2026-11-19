package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/blacktop/go-plist"
)

//go:embed releases.txt
var releasesText string

// release is a restore image to run against. One given as a bare URL has no version or build
// until its image has been opened.
type release struct {
	Version, Build, URL string
}

func (r release) name() string {
	if r.Build == "" {
		return r.URL
	}
	return fmt.Sprintf("macOS %s (%s)", r.Version, r.Build)
}

// compressed reports whether the release keeps its shared cache in a deflated ZIP member, which
// only --stream-compressed can read: macOS 13 and 14.
func (r release) compressed() bool {
	return strings.HasPrefix(r.Version, "13.") || strings.HasPrefix(r.Version, "14.")
}

// parseReleases reads releases.txt: one release a line, oldest first.
func parseReleases(text string) []release {
	var out []release
	for _, line := range strings.Split(text, "\n") {
		if fields := strings.Split(line, "\t"); len(fields) == 3 && !strings.HasPrefix(line, "#") {
			out = append(out, release{fields[0], fields[1], fields[2]})
		}
	}
	return out
}

// feedURL is Apple's own list of the restore image each Mac is currently offered. It names the
// current release only, so releases.txt is what remembers the earlier ones.
const feedURL = "https://mesu.apple.com/assets/macos/com_apple_macOSIPSW/com_apple_macOSIPSW.xml"

// errFeed is in the error of a name only the feed could have resolved: a failed run, not a bad
// command line.
var errFeed = errors.New("the feed could not be read")

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
	for _, version := range dict(feed, "MobileDeviceSoftwareVersionsByVersion") {
		for _, build := range dict(dict(version, "MobileDeviceSoftwareVersions"), device) {
			restore := dict(build, "Restore") // absent under "Unknown", which repeats the builds
			r := release{}
			r.Version, _ = restore["ProductVersion"].(string)
			r.Build, _ = restore["BuildVersion"].(string)
			r.URL, _ = restore["FirmwareURL"].(string)
			if r.Version != "" && r.Build != "" && strings.HasPrefix(r.URL, "https://") {
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

// fetchCurrent asks Apple which release device is currently offered.
func fetchCurrent(device string) ([]release, error) {
	client := http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(feedURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %s", feedURL, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return parseFeed(data, device)
}

// withCurrent adds the releases Apple currently offers to those in releases.txt that lack them.
func withCurrent(known, current []release) []release {
	out := slices.Clone(known)
	for _, r := range current {
		if !slices.ContainsFunc(known, func(k release) bool { return k.Build == r.Build }) {
			out = append(out, r)
		}
	}
	return out
}

// selectReleases turns the first argument into the releases it names, in the order named and
// each once. It is a comma-separated list of: "all"; "latest"; a version, which also stands for
// every known release under it ("15" is 15.0 and 15.6.1); a build; or the https URL of any image.
//
// current is asked, at most once, for what Apple offers now, and only by a name that releases.txt
// does not settle by itself: a version or build listed there needs nothing fetched to resolve it.
// When current fails and the names can do without it, the failure comes back as note.
func selectReleases(known []release, selector string, current func() ([]release, error)) (out []release, note error, err error) {
	var offered []release
	var feedErr error
	asked := false
	ask := func() {
		if !asked {
			asked = true
			offered, feedErr = current()
		}
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(selector, ",") {
		var matched []release
		switch {
		case strings.HasPrefix(part, "https://"):
			matched = []release{{URL: part}}
		case strings.Contains(part, "/") || strings.HasSuffix(part, ".ipsw"):
			return nil, nil, fmt.Errorf("a restore image must be an https:// URL, not %q (local files and plain http are not read)", part)
		case part == "latest":
			if ask(); feedErr != nil {
				return nil, nil, fmt.Errorf("latest is what Apple's feed says it is, and %w: %w", errFeed, feedErr)
			}
			matched = offered
		default:
			pool := known
			if !slices.ContainsFunc(known, func(r release) bool { return part == r.Version || strings.EqualFold(part, r.Build) }) {
				ask()
				pool = withCurrent(known, offered)
			}
			for _, r := range pool {
				if part == "all" || strings.EqualFold(part, r.Build) || part == r.Version || strings.HasPrefix(r.Version, part+".") {
					matched = append(matched, r)
				}
			}
		}
		if len(matched) == 0 {
			err := fmt.Errorf("no release %q among those known by name (ipswrun --releases prints them); any other runs from the https URL of its restore image", part)
			if feedErr != nil {
				err = fmt.Errorf("%w. Apple's feed, which names the current release, could not be read: %w", err, feedErr)
			}
			return nil, nil, err
		}
		for _, r := range matched {
			if !seen[r.URL] {
				seen[r.URL] = true
				out = append(out, r)
			}
		}
	}
	return out, feedErr, nil
}
