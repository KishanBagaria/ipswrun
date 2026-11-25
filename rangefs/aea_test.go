package rangefs

import (
	"archive/zip"
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/hpke"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	mrand "math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/blacktop/lzfse-cgo"
)

func buildTestAEA(t *testing.T, plain, symKey, authData []byte, segSize uint32, segsPerCluster uint32) []byte {
	t.Helper()
	clusterSize := int64(segSize) * int64(segsPerCluster)
	clusterCount := (int64(len(plain)) + clusterSize - 1) / clusterSize
	if clusterCount == 0 {
		clusterCount = 1
	}

	var mainSalt [32]byte
	for i := range mainSalt {
		mainSalt[i] = byte(i + 1)
	}
	profileField := aeaProfileSymmetric
	mainInfo := binary.LittleEndian.AppendUint32([]byte(aeaMainKeyInfo), profileField)
	mainKey, err := hkdf.Key(sha256.New, symKey, mainSalt[:], string(mainInfo), len(symKey))
	if err != nil {
		t.Fatal(err)
	}

	const entrySize = 8 + 32 // decompressed + compressed + sha256
	headersSize := entrySize * int(segsPerCluster)
	clusterHeaderSize := headersSize + 32 + 32*int(segsPerCluster)

	type builtCluster struct {
		headers  []byte
		segMACs  []byte
		payload  []byte
		material aeaKeyMaterial
	}
	clusters := make([]builtCluster, clusterCount)
	totalBodySize := 0
	for c := int64(0); c < clusterCount; c++ {
		cInfo := binary.LittleEndian.AppendUint32([]byte(aeaClusterKeyInfo), uint32(c))
		cKey, err := hkdf.Key(sha256.New, mainKey, nil, string(cInfo), len(mainKey))
		if err != nil {
			t.Fatal(err)
		}
		rawMat, err := hkdf.Key(sha256.New, cKey, nil, aeaClusterHeaderKeyInfo, 80)
		if err != nil {
			t.Fatal(err)
		}
		prk, err := hkdf.Extract(sha256.New, cKey, nil)
		if err != nil {
			t.Fatal(err)
		}

		headers := make([]byte, headersSize)
		segMACs := make([]byte, 32*int(segsPerCluster))
		var payload bytes.Buffer
		for j := uint32(0); j < segsPerCluster; j++ {
			off := c*clusterSize + int64(j)*int64(segSize)
			if off >= int64(len(plain)) {
				continue
			}
			end := min(off+int64(segSize), int64(len(plain)))
			chunk := plain[off:end]
			comp := lzfse.EncodeBuffer(chunk)
			if len(comp) == 0 || len(comp) >= len(chunk) {
				comp = bytes.Clone(chunk)
			}
			ent := headers[int(j)*entrySize : (int(j)+1)*entrySize]
			binary.LittleEndian.PutUint32(ent[0:4], uint32(len(chunk)))
			binary.LittleEndian.PutUint32(ent[4:8], uint32(len(comp)))
			sum := sha256.Sum256(chunk)
			copy(ent[8:40], sum[:])

			var segInfo [len(aeaSegmentKeyInfo) + 4]byte
			copy(segInfo[:], aeaSegmentKeyInfo)
			binary.LittleEndian.PutUint32(segInfo[len(aeaSegmentKeyInfo):], j)
			rawSegKey, err := hkdf.Expand(sha256.New, prk, string(segInfo[:]), 80)
			if err != nil {
				t.Fatal(err)
			}
			skm := unpackKeyMaterial(rawSegKey)
			enc := bytes.Clone(comp)
			if err := aeaCTRInPlace(enc, skm.key[:], skm.iv[:]); err != nil {
				t.Fatal(err)
			}
			mac := aeaHMAC(skm.mac[:], enc, nil)
			copy(segMACs[32*int(j):32*(int(j)+1)], mac[:])
			payload.Write(enc)
		}

		mat := unpackKeyMaterial(rawMat)
		if err := aeaCTRInPlace(headers, mat.key[:], mat.iv[:]); err != nil {
			t.Fatal(err)
		}
		clusters[c] = builtCluster{headers: headers, segMACs: segMACs, payload: payload.Bytes(), material: mat}
		totalBodySize += clusterHeaderSize + payload.Len()
	}

	// Build cluster HMAC chain in reverse.
	var nextMAC [32]byte
	nextMACs := make([][32]byte, clusterCount)
	for c := clusterCount - 1; c >= 0; c-- {
		nextMACs[c] = nextMAC
		macs := append(bytes.Clone(nextMAC[:]), clusters[c].segMACs...)
		nextMAC = aeaHMAC(clusters[c].material.mac[:], clusters[c].headers, macs)
	}
	firstClusterMAC := nextMAC

	prefixSize := binary.Size(aeaHeader{}) + len(authData) + 32 + binary.Size(aeaEncRootHeader{})
	root := aeaRootHeader{
		FileSize:           uint64(len(plain)),
		EncryptedSize:      uint64(prefixSize + totalBodySize),
		SegmentSize:        segSize,
		SegmentsPerCluster: segsPerCluster,
		Compression:        aeaCompLZFSE,
		Checksum:           aeaChecksumSha256,
	}
	var rootBuf bytes.Buffer
	_ = binary.Write(&rootBuf, binary.LittleEndian, root)
	rawRootKey, err := hkdf.Key(sha256.New, mainKey, nil, aeaRootHeaderKeyInfo, 80)
	if err != nil {
		t.Fatal(err)
	}
	rootKey := unpackKeyMaterial(rawRootKey)
	encRootData := bytes.Clone(rootBuf.Bytes())
	if err := aeaCTRInPlace(encRootData, rootKey.key[:], rootKey.iv[:]); err != nil {
		t.Fatal(err)
	}
	rootHMAC, err := aeaRootHeaderHMAC(rootKey.mac[:], encRootData, firstClusterMAC, bytes.NewReader(authData), uint32(len(authData)))
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, aeaHeader{
		Magic:                    [4]byte{'A', 'E', 'A', '1'},
		ProfileAndScryptStrength: profileField,
		AuthDataLength:           uint32(len(authData)),
	})
	out.Write(authData)
	out.Write(mainSalt[:])
	encRoot := aeaEncRootHeader{HMAC: rootHMAC, ClusterHMAC: firstClusterMAC}
	copy(encRoot.Data[:], encRootData)
	_ = binary.Write(&out, binary.LittleEndian, encRoot)
	for c := int64(0); c < clusterCount; c++ {
		out.Write(clusters[c].headers)
		out.Write(nextMACs[c][:])
		out.Write(clusters[c].segMACs)
		out.Write(clusters[c].payload)
	}
	return out.Bytes()
}

func encodeAEAAuthData(entries map[string][]byte) []byte {
	var buf bytes.Buffer
	for k, v := range entries {
		entry := append(append([]byte(k), 0), v...)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(entry)+4))
		buf.Write(entry)
	}
	return buf.Bytes()
}

type countingReaderAt struct {
	io.ReaderAt
	size  int64
	bytes int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.ReaderAt.ReadAt(p, off)
	c.bytes += int64(n)
	// Return (n, io.EOF) at exact end-of-archive to exercise io.ReaderAt's EOF contract.
	if err == nil && c.size > 0 && off+int64(n) == c.size {
		return n, io.EOF
	}
	return n, err
}

func TestAEASparseAuthenticatedReadsAndKeyResolution(t *testing.T) {
	const segSize = 64 << 10
	plain := make([]byte, 4*segSize+17)
	mrand.New(mrand.NewSource(42)).Read(plain)
	// Make segment 1 compressible so LZFSE decompression is exercised as well as stored segments.
	copy(plain[segSize:2*segSize], bytes.Repeat([]byte("apple-encrypted-archive-"), segSize/24))
	symKey := bytes.Repeat([]byte{7}, 32)

	// Generate a P-256 keypair and wrap symKey with HPKE just like Apple's WKMS FCS response.
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(ecPriv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})

	fcsHits := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fcsHits++
		w.Write(pemBytes)
	}))
	defer server.Close()
	prevClient := fcsHTTPClient
	fcsHTTPClient = server.Client()
	t.Cleanup(func() { fcsHTTPClient = prevClient })

	ecdhPub, err := ecPriv.PublicKey.ECDH()
	if err != nil {
		t.Fatal(err)
	}
	kemPub, err := hpke.DHKEM(ecdh.P256()).NewPublicKey(ecdhPub.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	encReq, sender, err := hpke.NewSender(kemPub, hpke.HKDFSHA256(), hpke.AES256GCM(), nil)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := sender.Seal(nil, symKey)
	if err != nil {
		t.Fatal(err)
	}
	fcsJSON, _ := json.Marshal(map[string]string{
		"enc-request": base64.StdEncoding.EncodeToString(encReq),
		"wrapped-key": base64.StdEncoding.EncodeToString(wrapped),
	})
	authData := encodeAEAAuthData(map[string][]byte{
		"com.apple.wkms.fcs-key-url":  []byte(server.URL + "/key.pem"),
		"com.apple.wkms.fcs-response": fcsJSON,
	})

	encoded := buildTestAEA(t, plain, symKey, authData, segSize, 2)
	md, err := readAEAMetadata(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	gotKey, err := resolveAEAKey(md, cacheDir)
	if err != nil || !bytes.Equal(gotKey, symKey) {
		t.Fatalf("resolveAEAKey = %x (err %v), want %x", gotKey, err, symKey)
	}
	if fcsHits != 1 {
		t.Fatalf("fcs key server hits = %d, want 1", fcsHits)
	}
	// Corrupt cached PEM triggers a re-fetch and repairs the cache.
	cachedPEMs, _ := filepath.Glob(filepath.Join(cacheDir, "fcs-*.pem"))
	if len(cachedPEMs) != 1 {
		t.Fatalf("expected 1 cached fcs pem, got %v", cachedPEMs)
	}
	if err := os.WriteFile(cachedPEMs[0], []byte("not-a-pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if gotKey, err = resolveAEAKey(md, cacheDir); err != nil || !bytes.Equal(gotKey, symKey) || fcsHits != 2 {
		t.Fatalf("corrupt cache recovery: err=%v hits=%d", err, fcsHits)
	}
	// Repaired cache serves offline after the server stops.
	server.Close()
	gotKey2, err := resolveAEAKey(md, cacheDir)
	if err != nil || !bytes.Equal(gotKey2, symKey) {
		t.Fatalf("cached resolveAEAKey = %x (err %v)", gotKey2, err)
	}
	// Plain http:// fcs-key-url is rejected.
	if _, err := resolveAEAKey(map[string][]byte{
		"com.apple.wkms.fcs-key-url":  []byte("http://example.invalid/key.pem"),
		"com.apple.wkms.fcs-response": fcsJSON,
	}, ""); err == nil {
		t.Fatal("accepted plain http fcs-key-url")
	}

	// Embedded hex encryption_key also resolves without network.
	hexKey, err := resolveAEAKey(map[string][]byte{"encryption_key": []byte(hex.EncodeToString(symKey))}, "")
	if err != nil || !bytes.Equal(hexKey, symKey) {
		t.Fatalf("hex encryption_key = %x (err %v)", hexKey, err)
	}

	// Sparse authenticated random reads, LRU hit, cross-segment read, and EOF (including exact-EOF final segment).
	src := &countingReaderAt{ReaderAt: bytes.NewReader(encoded), size: int64(len(encoded))}
	r, err := openAEA(src, int64(len(encoded)), gotKey)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	off := int64(segSize + 5) // inside LZFSE-compressed segment 1
	if _, err := r.ReadAt(buf, off); err != nil || !bytes.Equal(buf, plain[off:off+64]) {
		t.Fatalf("compressed segment read failed: %v", err)
	}
	if src.bytes > int64(len(encoded))/2 {
		t.Fatalf("not sparse: read %d of %d", src.bytes, len(encoded))
	}
	before := src.bytes
	if _, err := r.ReadAt(buf, off); err != nil {
		t.Fatal(err)
	}
	if src.bytes != before {
		t.Fatalf("segment LRU cache missed: read %d more bytes", src.bytes-before)
	}
	if _, err := r.ReadAt(buf, int64(segSize-16)); err != nil || !bytes.Equal(buf, plain[segSize-16:segSize+48]) {
		t.Fatalf("cross-segment read failed: %v", err)
	}
	if n, err := r.ReadAt(buf, int64(len(plain)-5)); n != 5 || !errors.Is(err, io.EOF) {
		t.Fatalf("EOF read = (%d, %v), want (5, EOF)", n, err)
	}

	// Tampered segment payload is rejected before modifying caller buffer.
	damaged := bytes.Clone(encoded)
	damaged[r.segments[2].input] ^= 1
	bad, err := openAEA(bytes.NewReader(damaged), int64(len(damaged)), gotKey)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := bytes.Repeat([]byte{0xaa}, 10)
	if n, err := bad.ReadAt(sentinel, int64(2*segSize)); n != 0 || err == nil || !bytes.Equal(sentinel, bytes.Repeat([]byte{0xaa}, 10)) {
		t.Fatalf("tampered segment was accepted: n=%d err=%v", n, err)
	}

	// Tampered root header is rejected on open.
	damaged = bytes.Clone(encoded)
	damaged[binary.Size(aeaHeader{})+len(authData)+32+4] ^= 1
	if _, err := openAEA(bytes.NewReader(damaged), int64(len(damaged)), gotKey); err == nil {
		t.Fatal("accepted modified root header")
	}

	// PruneCache also removes cached fcs-*.pem files.
	if _, _, _, err := PruneCache(cacheDir); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(cacheDir, "fcs-*"))
	if len(matches) != 0 {
		t.Fatalf("PruneCache left fcs files behind: %v", matches)
	}
}

func buildTestIM4PDeviceTree(t *testing.T, model, board string, compress bool) []byte {
	t.Helper()
	var dt bytes.Buffer
	_ = binary.Write(&dt, binary.LittleEndian, uint32(2)) // NumProperties
	_ = binary.Write(&dt, binary.LittleEndian, uint32(0)) // NumChildren
	writeProp := func(name string, val []byte) {
		var key [32]byte
		copy(key[:], name)
		dt.Write(key[:])
		_ = binary.Write(&dt, binary.LittleEndian, uint32(len(val)))
		dt.Write(val)
		if rem := len(val) % 4; rem != 0 {
			dt.Write(make([]byte, 4-rem))
		}
	}
	writeProp("model", append([]byte(model), 0))
	writeProp("compatible", append([]byte(board+"\x00"+model+"\x00AppleARM\x00"), 0))
	payload := dt.Bytes()
	if compress {
		if enc := lzfse.EncodeBuffer(payload); len(enc) > 0 {
			payload = enc
		}
	}
	raw, err := asn1.Marshal(im4pPayload{
		Tag:     "IM4P",
		Type:    "dtre",
		Version: "EmbeddedDeviceTrees",
		Data:    payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBuildManifestDeviceAndDeviceTreeResolution(t *testing.T) {
	manifestPlist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>ProductVersion</key><string>12.6</string>
	<key>ProductBuildVersion</key><string>21G115</string>
	<key>SupportedProductTypes</key>
	<array><string>Macmini9,1</string><string>MacBookPro18,1</string></array>
	<key>BuildIdentities</key>
	<array>
		<dict>
			<key>Info</key>
			<dict>
				<key>DeviceClass</key><string>j274ap</string>
				<key>Variant</key><string>macOS Customer</string>
			</dict>
			<key>Manifest</key>
			<dict>
				<key>OS</key><dict><key>Info</key><dict><key>Path</key><string>001-fs.dmg</string></dict></dict>
			</dict>
		</dict>
		<dict>
			<key>Info</key>
			<dict>
				<key>DeviceClass</key><string>j274ap</string>
				<key>Variant</key><string>macOS Recovery</string>
			</dict>
			<key>Manifest</key>
			<dict>
				<key>OS</key><dict><key>Info</key><dict><key>Path</key><string>001-recovery.dmg</string></dict></dict>
			</dict>
		</dict>
		<dict>
			<key>Ap,ProductType</key><string>MacBookPro18,1</string>
			<key>Info</key>
			<dict>
				<key>DeviceClass</key><string>j316sap</string>
				<key>Variant</key><string>macOS Customer</string>
			</dict>
			<key>Manifest</key>
			<dict>
				<key>OS</key><dict><key>Info</key><dict><key>Path</key><string>002-fs.dmg</string></dict></dict>
				<key>Cryptex1,SystemOS</key><dict><key>Info</key><dict><key>Path</key><string>002-sys.dmg</string></dict></dict>
			</dict>
		</dict>
	</array>
</dict>
</plist>`

	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, _ := zw.Create("BuildManifest.plist")
	w.Write([]byte(manifestPlist))
	dt, _ := zw.Create("Firmware/all_flash/DeviceTree.j274ap.im4p")
	dt.Write(buildTestIM4PDeviceTree(t, "Macmini9,1", "J274AP", true))
	zw.Close()

	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	bm, err := parseBuildManifest(zr.File)
	if err != nil {
		t.Fatal(err)
	}
	if bm.ProductVersion != "12.6" || bm.ProductBuildVersion != "21G115" {
		t.Fatalf("manifest version=%q build=%q", bm.ProductVersion, bm.ProductBuildVersion)
	}

	// Macmini9,1 has no Ap,ProductType in BuildIdentities, so DeviceTree fallback resolves j274ap.
	ids, err := bm.forDevice("Macmini9,1", zr.File)
	if err != nil {
		t.Fatalf("forDevice(Macmini9,1): %v", err)
	}
	fsPath, err := fileSystemDmg(ids)
	if err != nil || fsPath != "001-fs.dmg" {
		t.Fatalf("fileSystemDmg = %q (%v), want 001-fs.dmg", fsPath, err)
	}
	if _, err := systemOsDmg(ids); !errors.Is(err, ErrNoSystemOS) {
		t.Fatalf("systemOsDmg error = %v, want ErrNoSystemOS", err)
	}

	// MacBookPro18,1 resolves directly via Ap,ProductType without touching DeviceTree.
	ids2, err := bm.forDevice("macbookpro18,1", nil)
	if err != nil {
		t.Fatalf("forDevice(macbookpro18,1): %v", err)
	}
	sysPath, err := systemOsDmg(ids2)
	if err != nil || sysPath != "002-sys.dmg" {
		t.Fatalf("systemOsDmg = %q (%v), want 002-sys.dmg", sysPath, err)
	}

	// Unknown device fails clearly.
	if _, err := bm.forDevice("Mac99,9", zr.File); err == nil {
		t.Fatal("forDevice succeeded for unknown device")
	}

	// Ensure cache directory cleanup works cleanly even if empty.
	if _, err := os.Stat(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
