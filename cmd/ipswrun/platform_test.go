package main

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// machO writes an arm64 Mach-O that has only the one load command given, and returns its path.
func machO(t *testing.T, cmd uint32, body ...uint32) string {
	t.Helper()
	le := binary.LittleEndian
	command := le.AppendUint32(le.AppendUint32(nil, cmd), uint32(8+4*len(body)))
	for _, v := range body {
		command = le.AppendUint32(command, v)
	}
	var file []byte
	for _, v := range []uint32{0xFEEDFACF, 0x100000C, 0, 2, 1, uint32(len(command)), 0, 0} { // MH_EXECUTE
		file = le.AppendUint32(file, v)
	}
	path := filepath.Join(t.TempDir(), "guest")
	if err := os.WriteFile(path, append(file, command...), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckBuiltFor(t *testing.T) {
	mac := machO(t, lcBuildVersion, platformMacOS, 11<<16, 15<<16, 0)
	ios := machO(t, lcBuildVersion, platformIOS, 14<<16, 18<<16, 0)
	catalyst := machO(t, lcBuildVersion, platformCatalyst, 14<<16, 18<<16, 0)
	oldIOS := machO(t, lcVersionMinIPhone, 12<<16, 12<<16)
	simulator := machO(t, lcBuildVersion, 7, 14<<16, 18<<16, 0)
	script := filepath.Join(t.TempDir(), "script")
	os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	for _, c := range []struct {
		path, system, wantErr string
	}{
		{mac, "", ""},
		{catalyst, "", ""},
		{ios, iOS, ""},
		{oldIOS, iOS, ""},
		{script, iOS, ""},
		{"system:/usr/bin/true", iOS, ""},
		{ios, "", "is built for iOS, and this --device runs macOS"},
		{oldIOS, "", "is built for iOS, and this --device runs macOS"},
		{mac, iOS, "is built for macOS, and this --device runs iOS"},
		{catalyst, iOS, "is built for platform 6"},
		{simulator, iOS, "is built for platform 7"},
	} {
		err := checkBuiltFor(c.path, c.system)
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("checkBuiltFor(%s, %q) = %v, want %q", filepath.Base(c.path), c.system, err, c.wantErr)
		}
	}
}

func TestCheckDevice(t *testing.T) {
	for device, want := range map[string]error{
		"iPhone11,2": nil, "iPhone17,1": nil, "iPad8,1": nil, "iPad13,4": nil,
		"iPhone10,3": errOldChip, "iPad7,5": errOldChip, "iPod9,1": errOldChip,
	} {
		if err := checkDevice(device); !errors.Is(err, want) || (want == nil) != (err == nil) {
			t.Errorf("checkDevice(%s) = %v, want %v", device, err, want)
		}
	}
	for _, device := range []string{"iPhone", "iPhone,", "iPhoneX,1"} {
		if err := checkDevice(device); err == nil || errors.Is(err, errOldChip) {
			t.Errorf("checkDevice(%s) = %v, want it not to be a product type", device, err)
		}
	}
}

func TestCheckIOSReleases(t *testing.T) {
	if err := checkIOSReleases([]release{{Version: "14.0", Build: "18A373"}, {Version: "18.7.12", Build: "22H390"}, {URL: "https://x/y.ipsw"}}); err != nil {
		t.Error(err)
	}
	for _, version := range []string{"12.4.1", "13.0"} {
		if err := checkIOSReleases([]release{{Version: "18.0"}, {Version: version}}); err == nil || !strings.Contains(err.Error(), "iOS "+version) {
			t.Errorf("iOS %s: %v", version, err)
		}
	}
}
