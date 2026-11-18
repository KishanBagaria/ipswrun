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

// The Unicorn build the emulator is validated against: the library inside the official
// unicorn 2.1.4 wheel for macOS arm64 on PyPI, pinned by the SHA-256 of both the wheel and the
// library. Nothing else is ever loaded.
const (
	unicornWheelURL = "https://files.pythonhosted.org/packages/6c/ae/4943c6f8524d729ec7d5e69df6407ea05d710fe77471d91cecf3fc64eb57/" +
		"unicorn-2.1.4-cp37-abi3-macosx_11_0_arm64.whl"
	unicornWheelSHA256   = "d6c93e0f60328d8f4a1792af3f834137a28050fcc2305f2ec01efe8558a9844e"
	unicornLibrarySHA256 = "7207c8e3d7a63118fb0bca73e01816797fd51b1d8a39a4cbc7abfd562ee59c85"
	unicornWheelMember   = "unicorn/lib/libunicorn.2.dylib"
	unicornMaxSize       = 64 << 20
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// unicornPath is where the pinned libunicorn is kept under dir.
func unicornPath(dir string) string {
	return filepath.Join(dir, "unicorn", "2.1.4", unicornLibrarySHA256, filepath.Base(unicornWheelMember))
}

// EnsureUnicorn returns the path of the pinned libunicorn under dir, downloading it on first use.
// The file is hashed on every call, so a library that was replaced or damaged is fetched again
// rather than loaded.
func EnsureUnicorn(dir string) (string, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return "", fmt.Errorf("the emulator needs an Apple silicon Mac (this is %s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	path := unicornPath(dir)
	if existing, err := os.ReadFile(path); err == nil && sha256Hex(existing) == unicornLibrarySHA256 {
		return path, nil
	}
	library, err := fetchUnicorn()
	if err != nil {
		return "", fmt.Errorf("fetch unicorn: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// Concurrent dumps may install at once; each renames a complete, verified file into place.
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

func fetchUnicorn() ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(unicornWheelURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", unicornWheelURL, resp.Status)
	}
	wheel, err := io.ReadAll(io.LimitReader(resp.Body, unicornMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(wheel) > unicornMaxSize || sha256Hex(wheel) != unicornWheelSHA256 {
		return nil, fmt.Errorf("%s failed integrity verification", unicornWheelURL)
	}
	archive, err := zip.NewReader(bytes.NewReader(wheel), int64(len(wheel)))
	if err != nil {
		return nil, err
	}
	member, err := archive.Open(unicornWheelMember)
	if err != nil {
		return nil, err
	}
	defer member.Close()
	library, err := io.ReadAll(io.LimitReader(member, unicornMaxSize+1))
	if err != nil {
		return nil, err
	}
	if len(library) > unicornMaxSize || sha256Hex(library) != unicornLibrarySHA256 {
		return nil, fmt.Errorf("%s in the unicorn wheel failed integrity verification", unicornWheelMember)
	}
	return library, nil
}
