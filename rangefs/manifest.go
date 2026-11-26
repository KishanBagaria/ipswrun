package rangefs

import (
	"archive/zip"
	"bytes"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/blacktop/go-plist"
	"github.com/blacktop/lzfse-cgo"
)

type buildManifest struct {
	BuildIdentities       []buildIdentity `plist:"BuildIdentities"`
	ProductBuildVersion   string          `plist:"ProductBuildVersion"`
	ProductVersion        string          `plist:"ProductVersion"`
	SupportedProductTypes []string        `plist:"SupportedProductTypes"`
}

type buildIdentity struct {
	// Trailing ",omitempty" is required because go-plist splits tags on the last comma.
	ApProductType string                      `plist:"Ap,ProductType,omitempty"`
	Info          identityInfo                `plist:"Info"`
	Manifest      map[string]identityManifest `plist:"Manifest"`
}

type identityInfo struct {
	DeviceClass string `plist:"DeviceClass"`
	Variant     string `plist:"Variant"`
}

type identityManifest struct {
	Info struct {
		Path string `plist:"Path"`
	} `plist:"Info"`
}

// parseBuildManifest locates and decodes BuildManifest.plist inside a restore image ZIP.
func parseBuildManifest(files []*zip.File) (*buildManifest, error) {
	const maxManifestSize = 64 << 20
	var target *zip.File
	for _, f := range files {
		if path.Base(f.Name) == "BuildManifest.plist" && !strings.Contains(f.Name, "Restore") {
			target = f
			break
		}
	}
	if target == nil {
		return nil, errors.New("restore image has no BuildManifest.plist")
	}
	data, err := readMember(target, maxManifestSize)
	if err != nil {
		return nil, err
	}
	var bm buildManifest
	if _, err := plist.Unmarshal(data, &bm); err != nil {
		return nil, fmt.Errorf("decode %s: %w", target.Name, err)
	}
	return &bm, nil
}

// forDevice returns the build identities matching device (a case-insensitive product type like
// "Macmini9,1" or device class like "j274ap"). Empty device returns all identities.
// Older IPSWs omit Ap,ProductType from BuildIdentities; in that case DeviceTree.*.im4p entries
// are read lazily to map each board config to its product type.
func (bm *buildManifest) forDevice(device string, files []*zip.File) ([]buildIdentity, error) {
	if device == "" {
		return bm.BuildIdentities, nil
	}
	match := func(boardToProduct map[string]string) []buildIdentity {
		var out []buildIdentity
		for _, bi := range bm.BuildIdentities {
			product := bi.ApProductType
			if product == "" && len(bm.SupportedProductTypes) == 1 {
				product = bm.SupportedProductTypes[0]
			}
			if product == "" {
				product = boardToProduct[strings.ToLower(bi.Info.DeviceClass)]
			}
			if strings.EqualFold(device, product) || strings.EqualFold(device, bi.Info.DeviceClass) {
				out = append(out, bi)
			}
		}
		return out
	}
	if ids := match(nil); len(ids) > 0 {
		return ids, nil
	}
	if boardToProduct := deviceTreeProducts(files); len(boardToProduct) > 0 {
		if ids := match(boardToProduct); len(ids) > 0 {
			return ids, nil
		}
	}
	return nil, fmt.Errorf("no BuildManifest identity matches device %q (use a product type or device class)", device)
}

func fileSystemDmg(identities []buildIdentity) (string, error) {
	var dmgs []string
	for _, bi := range identities {
		if strings.Contains(bi.Info.Variant, "Recovery") {
			continue
		}
		if comp, ok := bi.Manifest["OS"]; ok {
			if p := comp.Info.Path; p != "" && !slices.Contains(dmgs, p) {
				dmgs = append(dmgs, p)
			}
		}
	}
	switch len(dmgs) {
	case 0:
		return "", errors.New("no filesystem DMG found")
	case 1:
		return dmgs[0], nil
	default:
		return "", errors.New("multiple filesystem DMGs found")
	}
}

func systemOsDmg(identities []buildIdentity) (string, error) {
	var dmgs []string
	for _, bi := range identities {
		comp, ok := bi.Manifest["Cryptex1,SystemOS"]
		if !ok {
			continue
		}
		p := comp.Info.Path
		if p == "" {
			return "", fmt.Errorf("invalid SystemOS path for device class %q", bi.Info.DeviceClass)
		}
		if !slices.Contains(dmgs, p) {
			dmgs = append(dmgs, p)
		}
	}
	switch len(dmgs) {
	case 0:
		return "", fmt.Errorf("no SystemOS DMG found: %w", ErrNoSystemOS)
	case 1:
		return dmgs[0], nil
	default:
		return "", fmt.Errorf("multiple SystemOS DMGs found (%s); select a device", strings.Join(dmgs, ", "))
	}
}

// deviceTreeProducts reads unencrypted DeviceTree.*.im4p payloads from the IPSW and maps
// lowercased board configs (e.g. "j274ap") to product types (e.g. "Macmini9,1").
func deviceTreeProducts(files []*zip.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		base := path.Base(f.Name)
		if !strings.HasPrefix(base, "DeviceTree") || !strings.HasSuffix(base, ".im4p") {
			continue
		}
		raw, err := readMember(f, maxDeviceTreeSize)
		if err != nil {
			continue
		}
		if board, model, ok := parseIM4PDeviceTree(raw); ok {
			out[strings.ToLower(board)] = model
		}
	}
	return out
}

const maxDeviceTreeSize = 16 << 20

// readMember reads a whole ZIP member of at most max bytes.
func readMember(f *zip.File, max int) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", f.Name, err)
	}
	data, err := io.ReadAll(io.LimitReader(rc, int64(max)+1))
	rc.Close()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f.Name, err)
	}
	if len(data) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", f.Name, max)
	}
	return data, nil
}

type im4pCompression struct {
	Algorithm        int
	UncompressedSize int
}

type im4pPayload struct {
	Tag         string `asn1:"ia5"`
	Type        string `asn1:"ia5"`
	Version     string `asn1:"ia5"`
	Data        []byte
	Compression im4pCompression `asn1:"optional,omitempty"`
	Keybag      []byte          `asn1:"optional"`
	Properties  asn1.RawValue   `asn1:"optional,tag:0,class:context,explicit"`
	Hash        []byte          `asn1:"optional"`
}

func parseIM4PDeviceTree(raw []byte) (board, model string, ok bool) {
	data, err := im4pPayloadData(raw)
	if err != nil {
		return "", "", false
	}
	return parseDeviceTreeRoot(data)
}

// ErrEncrypted is in the error of a read of a firmware file whose payload is encrypted with a key
// only the device has.
var ErrEncrypted = errors.New("payload is encrypted")

// im4pPayloadData is what an Image4 payload holds, decompressed.
func im4pPayloadData(raw []byte) ([]byte, error) {
	var p im4pPayload
	if _, err := asn1.Unmarshal(raw, &p); err != nil || p.Tag != "IM4P" {
		return nil, errors.New("not an Image4 payload")
	}
	if len(p.Keybag) > 0 {
		return nil, ErrEncrypted
	}
	data := p.Data
	if !isLZFSE(data) {
		return data, nil
	}
	// The payload says how long it is unpacked, where it says anything; where it does not, room
	// is made for more and more until it fits (a kernelcache unpacks to over four times its size).
	if size := p.Compression.UncompressedSize; size > 0 {
		out := make([]byte, size+1)
		if n := lzfse.DecodeBufferInto(data, out); n == size {
			return out[:n], nil
		}
		return nil, errors.New("payload does not decompress")
	}
	for room := 4 * len(data); room <= maxUnpackedPayload; room *= 2 {
		out := make([]byte, room)
		if n := lzfse.DecodeBufferInto(data, out); n > 0 && n < room {
			return out[:n], nil
		}
	}
	return nil, errors.New("payload does not decompress")
}

const maxUnpackedPayload = 1 << 30

func isLZFSE(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	switch binary.LittleEndian.Uint32(data[:4]) {
	case 0x2d787662, // bvx-
		0x31787662, // bvx1
		0x32787662, // bvx2
		0x6e787662: // bvxn
		return true
	}
	return false
}

// parseDeviceTreeRoot reads only the root node properties of an Apple DeviceTree to recover its
// "model" (product type) and board config from "compatible".
func parseDeviceTreeRoot(data []byte) (board, model string, ok bool) {
	if len(data) < 8 {
		return "", "", false
	}
	numProps := binary.LittleEndian.Uint32(data[0:4])
	off := 8
	var compat []string
	for range numProps {
		if off+36 > len(data) {
			return "", "", false
		}
		name := string(bytes.TrimRight(data[off:off+32], "\x00"))
		length := int(binary.LittleEndian.Uint32(data[off+32:off+36]) & 0x7fffffff)
		padded := (length + 3) &^ 3
		off += 36
		if length < 0 || padded < length || off+padded > len(data) {
			return "", "", false
		}
		val := data[off : off+length]
		off += padded
		switch name {
		case "model":
			model = string(bytes.TrimRight(val, "\x00"))
		case "compatible":
			for _, part := range bytes.Split(bytes.TrimRight(val, "\x00"), []byte{0}) {
				if len(part) > 0 {
					compat = append(compat, string(part))
				}
			}
		}
	}
	if model == "" {
		return "", "", false
	}
	for _, elem := range compat {
		if !strings.Contains(elem, "Apple") && !strings.Contains(elem, model) {
			return elem, model, true
		}
	}
	return "", "", false
}
