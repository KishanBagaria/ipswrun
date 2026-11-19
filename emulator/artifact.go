package emulator

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// The Unicorn builds the emulator is validated against, by host: the library inside the official
// unicorn 2.1.4 wheel on PyPI, pinned by the SHA-256 of both the wheel and the library. Nothing
// else is ever loaded.
type unicornBuild struct {
	wheel, wheelSHA256    string // under unicornWheels
	member, librarySHA256 string
}

const (
	unicornWheels  = "https://files.pythonhosted.org/packages/"
	unicornMaxSize = 64 << 20
)

var unicornBuilds = map[string]unicornBuild{
	"darwin/arm64": {"6c/ae/4943c6f8524d729ec7d5e69df6407ea05d710fe77471d91cecf3fc64eb57/unicorn-2.1.4-cp37-abi3-macosx_11_0_arm64.whl",
		"d6c93e0f60328d8f4a1792af3f834137a28050fcc2305f2ec01efe8558a9844e",
		"unicorn/lib/libunicorn.2.dylib", "7207c8e3d7a63118fb0bca73e01816797fd51b1d8a39a4cbc7abfd562ee59c85"},
	"linux/arm64": {"33/9f/32d41eb942221bcf4417cdc65537fc8b3bbbd6079d6c161e621f1dd4e94a/unicorn-2.1.4-cp37-abi3-manylinux_2_17_aarch64.manylinux2014_aarch64.whl",
		"bd1fb0c9af5f57e356d8a96928b4fe045b2e18f308ef23b481d5f970008aa722",
		"unicorn/lib/libunicorn.so.2", "a0b99458a82e268aee258205a40590411c3a9f28e42abf2942ce4e87b7d9ac65"},
	"linux/amd64": {"e7/df/ded5e3684c2d7600b30cc8a7530277b8cb36644a1a9d34cade7ebb45604c/unicorn-2.1.4-cp37-abi3-manylinux_2_17_x86_64.manylinux2014_x86_64.whl",
		"9d6e6dea140560de4ebd8446661f7ef84a357d428c14a3ef09dacd306ec8c239",
		"unicorn/lib/libunicorn.so.2", "ddb196ec82b52e502c18e4a34478bf7b9f61c83c2ebaa95c74d8ded45a95da9c"},
}

// unicorn is this host's build; ok is false where the emulator has none.
func unicorn() (build unicornBuild, ok bool) {
	build, ok = unicornBuilds[runtime.GOOS+"/"+runtime.GOARCH]
	return
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// unicornPath is where the pinned libunicorn is kept under dir.
func unicornPath(dir string) string {
	build, _ := unicorn()
	return filepath.Join(dir, "unicorn", "2.1.4", build.librarySHA256, filepath.Base(build.member))
}

// EnsureUnicorn returns the path of the pinned libunicorn under dir, downloading it on first use.
// The file is hashed on every call, so a library that was replaced or damaged is fetched again
// rather than loaded.
func EnsureUnicorn(dir string) (string, error) {
	build, ok := unicorn()
	if !ok {
		return "", fmt.Errorf("the emulator needs an Apple silicon Mac, or Linux on arm64 or x86-64 (this is %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	path := unicornPath(dir)
	if existing, err := os.ReadFile(path); err == nil && sha256Hex(existing) == build.librarySHA256 {
		return path, nil
	}
	library, err := fetchUnicorn(build)
	if err != nil {
		return "", fmt.Errorf("fetch unicorn: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Concurrent runs may install at once; each renames a complete, verified file into place.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".libunicorn-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(library); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

func fetchUnicorn(build unicornBuild) ([]byte, error) {
	url := unicornWheels + build.wheel
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	wheel, err := io.ReadAll(io.LimitReader(resp.Body, unicornMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(wheel) > unicornMaxSize || sha256Hex(wheel) != build.wheelSHA256 {
		return nil, fmt.Errorf("%s failed integrity verification", url)
	}
	archive, err := zip.NewReader(bytes.NewReader(wheel), int64(len(wheel)))
	if err != nil {
		return nil, err
	}
	member, err := archive.Open(build.member)
	if err != nil {
		return nil, err
	}
	defer member.Close()
	library, err := io.ReadAll(io.LimitReader(member, unicornMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(library) > unicornMaxSize || sha256Hex(library) != build.librarySHA256 {
		return nil, fmt.Errorf("%s in the unicorn wheel failed integrity verification", build.member)
	}
	return library, nil
}
