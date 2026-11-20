// Research tool, not product code. The host's half of machine provisioning: the HTTP exchange
// with Apple's provisioning service, whose blobs a guest then hands to adid.
//
//	provision [flags] start            fetch a server challenge (spim), write it to <dir>/spim.b64
//	provision [flags] finish <cpim>    send adid's client challenge, write <dir>/ptm.b64 and tk.b64
//
// Every response is kept in <dir> (default research/live, which git ignores) and a request whose
// response is already there is not made again. Three requests make a whole exchange: the lookup
// of the service's URLs, start, and finish. Anything but a 200 with status 0 stops the tool; it
// never retries. No account is involved: the requests carry the emulated machine's identifier
// and versions and nothing about a person.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/blacktop/go-plist"
)

const lookupURL = "https://gsa.apple.com/grandslam/GsService2/lookup"

var (
	dir = flag.String("dir", "research/live", "where responses are kept and replayed from")
	// The identifier ipswrun's gethostuuid gives every guest (emulator/bsd.go).
	device  = flag.String("device", "1F5AE370-9C42-4D1B-8E6D-49505357524E", "the machine's UUID, as adid's gethostuuid returns it")
	model   = flag.String("model", "Macmini9,1", "hw.model of the emulated Mac")
	version = flag.String("version", "13.0", "macOS version of the release adid is from")
	build   = flag.String("build", "22A380", "its build")
	authKit = flag.String("authkit", "", "CFBundleVersion of that release's AuthKit.framework")
	akd     = flag.String("akd", "1.0", "CFBundleShortVersionString of its akd")
	network = flag.String("cfnetwork", "", "CFBundleVersion of its CFNetwork.framework")
	darwin  = flag.String("darwin", "", "its kern.osrelease")
	offline = flag.Bool("offline", false, "fail instead of making a request that is not cached")
)

func main() {
	flag.Parse()
	var err error
	switch {
	case flag.NArg() == 1 && flag.Arg(0) == "start":
		err = start()
	case flag.NArg() == 2 && flag.Arg(0) == "finish":
		err = finish(flag.Arg(1))
	default:
		err = errors.New("usage: provision [flags] start | finish <cpim file>")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "provision:", err)
		os.Exit(1)
	}
}

func start() error {
	url, err := serviceURL("midStartProvisioning")
	if err != nil {
		return err
	}
	response, err := call("start", url, map[string]any{})
	if err != nil {
		return err
	}
	return blob(response, "spim")
}

func finish(cpimFile string) error {
	cpim, err := os.ReadFile(cpimFile)
	if err != nil {
		return err
	}
	url, err := serviceURL("midFinishProvisioning")
	if err != nil {
		return err
	}
	// The plist's strings are the base64 text itself, as the service sent spim.
	response, err := call("finish", url, map[string]any{"cpim": string(bytes.TrimSpace(cpim))})
	if err != nil {
		return err
	}
	if err := blob(response, "ptm"); err != nil {
		return err
	}
	if err := blob(response, "tk"); err != nil {
		return err
	}
	fmt.Println("routing info (X-Apple-I-MD-RINFO):", response["X-Apple-I-MD-RINFO"])
	return nil
}

// blob writes one base64 field of a response as <dir>/<name>.b64 for the guest to read.
func blob(response map[string]any, name string) error {
	text, ok := response[name].(string)
	if !ok || text == "" {
		return fmt.Errorf("the response has no %s", name)
	}
	out := filepath.Join(*dir, name+".b64")
	fmt.Println(out, len(text), "characters of base64")
	return os.WriteFile(out, []byte(text), 0o600)
}

func serviceURL(name string) (string, error) {
	body, err := cached("lookup", func() (*http.Request, error) { return http.NewRequest("GET", lookupURL, nil) })
	if err != nil {
		return "", err
	}
	var lookup struct {
		URLs map[string]string `plist:"urls"`
	}
	if _, err := plist.Unmarshal(body, &lookup); err != nil {
		return "", fmt.Errorf("lookup: %w", err)
	}
	url := lookup.URLs[name]
	if !strings.HasPrefix(url, "https://gsa.apple.com/") {
		return "", fmt.Errorf("lookup: %s is %q, not a URL of the service", name, url)
	}
	return url, nil
}

// call posts {Header: {}, Request: request} and returns the Response dictionary of a success.
func call(name, url string, request map[string]any) (map[string]any, error) {
	body, err := cached(name, func() (*http.Request, error) {
		data, err := plist.MarshalIndent(map[string]any{"Header": map[string]any{}, "Request": request}, plist.XMLFormat, "\t")
		if err != nil {
			return nil, err
		}
		r, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err == nil {
			r.Header.Set("Content-Type", "text/x-xml-plist")
		}
		return r, err
	})
	if err != nil {
		return nil, err
	}
	var answer struct {
		Response map[string]any `plist:"Response"`
	}
	if _, err := plist.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	status, _ := answer.Response["Status"].(map[string]any)
	if code := fmt.Sprint(status["ec"]); code != "0" {
		return nil, fmt.Errorf("%s: the service answered status %s (%v): stopping, see %s", name, code, status["em"], filepath.Join(*dir, name+".response"))
	}
	return answer.Response, nil
}

// cached returns the kept response body of the named request, or makes it once and keeps what
// came back, whatever it was: a refusal is kept too, so that it is not asked for twice.
func cached(name string, request func() (*http.Request, error)) ([]byte, error) {
	file := filepath.Join(*dir, name+".response")
	if body, err := os.ReadFile(file); err == nil {
		fmt.Fprintln(os.Stderr, name+": replayed from", file)
		return body, nil
	}
	if *offline {
		return nil, fmt.Errorf("%s: not cached, and --offline", name)
	}
	if *authKit == "" || *network == "" || *darwin == "" {
		return nil, errors.New("--authkit, --cfnetwork and --darwin are needed to make a request")
	}
	r, err := request()
	if err != nil {
		return nil, err
	}
	id, err := hex.DecodeString(strings.ReplaceAll(*device, "-", ""))
	if err != nil || len(id) != 16 {
		return nil, fmt.Errorf("--device %q is not a UUID", *device)
	}
	localUser := sha256.Sum256(id)
	now := time.Now().UTC()
	for key, value := range map[string]string{
		"User-Agent":            fmt.Sprintf("akd/%s CFNetwork/%s Darwin/%s", *akd, *network, *darwin),
		"Accept":                "*/*",
		"X-Mme-Device-Id":       strings.ToLower(*device),
		"X-Apple-I-MD-LU":       hex.EncodeToString(localUser[:]),
		"X-MMe-Client-Info":     fmt.Sprintf("<%s> <Mac OS X;%s;%s> <com.apple.AuthKit/%s (com.apple.akd/%s)>", *model, *version, *build, *authKit, *akd),
		"X-Apple-I-SRL-NO":      "0",
		"X-Apple-I-Client-Time": now.Format("2006-01-02T15:04:05Z"),
		"X-Apple-I-TimeZone":    "UTC",
		"X-Apple-Locale":        "en_US",
	} {
		r.Header.Set(key, value)
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, name+":", r.Method, r.URL)
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	var headers bytes.Buffer
	fmt.Fprintln(&headers, response.Status)
	response.Header.Write(&headers)
	os.WriteFile(filepath.Join(*dir, name+".headers"), headers.Bytes(), 0o600)
	if response.StatusCode != http.StatusOK {
		os.WriteFile(filepath.Join(*dir, name+".refused"), body, 0o600)
		return nil, fmt.Errorf("%s: %s: stopping, the body is in %s", name, response.Status, filepath.Join(*dir, name+".refused"))
	}
	return body, os.WriteFile(file, body, 0o600)
}
