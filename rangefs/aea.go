package rangefs

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/hpke"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/blacktop/lzfse-cgo"
	"github.com/hashicorp/golang-lru/v2/simplelru"
)

const (
	aeaMagic                = "AEA1"
	aeaMainKeyInfo          = "AEA_AMK"
	aeaRootHeaderKeyInfo    = "AEA_RHEK"
	aeaClusterKeyInfo       = "AEA_CK"
	aeaClusterHeaderKeyInfo = "AEA_CHEK"
	aeaSegmentKeyInfo       = "AEA_SK"

	aeaProfileSymmetric uint32 = 1

	aeaMaxMetadataSize    = 16 << 20
	aeaMaxSegmentSize     = 256 << 20
	aeaMaxSegsPerCluster  = 256 << 10
	aeaMaxIndexedSegments = 1 << 20
	aeaCacheBudget        = 32 << 20
)

const (
	aeaChecksumNone   uint8 = 0
	aeaChecksumMurmur uint8 = 1
	aeaChecksumSha256 uint8 = 2
)

const (
	aeaCompLZBITMAP uint8 = 'b'
	aeaCompLZFSE    uint8 = 'e'
	aeaCompLZVN     uint8 = 'f'
	aeaCompLZMA     uint8 = 'x'
	aeaCompZLIB     uint8 = 'z'
)

type aeaHeader struct {
	Magic                    [4]byte
	ProfileAndScryptStrength uint32
	AuthDataLength           uint32
}

func (h aeaHeader) profileID() uint32 { return h.ProfileAndScryptStrength & 0xffffff }

type aeaEncRootHeader struct {
	HMAC        [32]byte
	Data        [48]byte
	ClusterHMAC [32]byte
}

type aeaRootHeader struct {
	FileSize           uint64
	EncryptedSize      uint64
	SegmentSize        uint32
	SegmentsPerCluster uint32
	Compression        uint8
	Checksum           uint8
	_                  [22]byte
}

type aeaKeyMaterial struct {
	mac [32]byte
	key [32]byte
	iv  [aes.BlockSize]byte
}

func unpackKeyMaterial(raw []byte) aeaKeyMaterial {
	var m aeaKeyMaterial
	copy(m.mac[:], raw[0:32])
	copy(m.key[:], raw[32:64])
	copy(m.iv[:], raw[64:80])
	return m
}

type aeaSegment struct {
	cluster, index   uint32
	decompressedSize uint32
	compressedSize   uint32
	checksum         [32]byte
	mac              [32]byte
	clusterPRK       []byte
	input, output    int64
}

var aeaHeaderSize = int64(binary.Size(aeaHeader{}))

// readAEAHeader reads and validates the fixed AEA1 header; its auth data follows at aeaHeaderSize.
func readAEAHeader(source io.ReaderAt, size int64) (aeaHeader, error) {
	var hdr aeaHeader
	if err := binary.Read(io.NewSectionReader(source, 0, size), binary.LittleEndian, &hdr); err != nil {
		return hdr, fmt.Errorf("read AEA header: %w", err)
	}
	if string(hdr.Magic[:]) != aeaMagic {
		return hdr, fmt.Errorf("invalid AEA header: found %q expected %q", hdr.Magic[:], aeaMagic)
	}
	if hdr.AuthDataLength > aeaMaxMetadataSize {
		return hdr, fmt.Errorf("AEA metadata exceeds %d bytes", aeaMaxMetadataSize)
	}
	if int64(hdr.AuthDataLength) > size-aeaHeaderSize {
		return hdr, fmt.Errorf("truncated AEA metadata: %w", io.ErrUnexpectedEOF)
	}
	return hdr, nil
}

// readAEAMetadata parses the key-value auth-data header of an AEA1 archive.
func readAEAMetadata(source io.ReaderAt, size int64) (map[string][]byte, error) {
	hdr, err := readAEAHeader(source, size)
	if err != nil {
		return nil, err
	}
	md := make(map[string][]byte)
	mdr := io.NewSectionReader(source, aeaHeaderSize, int64(hdr.AuthDataLength))
	for remaining := hdr.AuthDataLength; remaining > 0; {
		if remaining < 4 {
			return nil, fmt.Errorf("truncated AEA metadata length: %w", io.ErrUnexpectedEOF)
		}
		var length uint32
		if err := binary.Read(mdr, binary.LittleEndian, &length); err != nil {
			return nil, err
		}
		remaining -= 4
		if length < 5 || length-4 > remaining {
			return nil, fmt.Errorf("invalid AEA metadata entry length %d", length)
		}
		kv := make([]byte, length-4)
		if _, err := io.ReadFull(mdr, kv); err != nil {
			return nil, err
		}
		remaining -= length - 4
		k, v, ok := bytes.Cut(kv, []byte{0})
		if !ok {
			return nil, fmt.Errorf("invalid AEA metadata entry: missing key separator")
		}
		md[string(k)] = v
	}
	return md, nil
}

// resolveAEAKey obtains the 32-byte symmetric key from AEA metadata: either an embedded
// encryption_key or an HPKE-wrapped FCS key unwrapped with the public key at fcs-key-url.
func resolveAEAKey(md map[string][]byte, cacheDir string) ([]byte, error) {
	if encKey, ok := md["encryption_key"]; ok {
		key, err := hex.DecodeString(string(encKey))
		if err != nil {
			return nil, fmt.Errorf("decode hex symmetric key: %w", err)
		}
		return key, nil
	}
	fcsRaw, ok := md["com.apple.wkms.fcs-response"]
	if !ok {
		return nil, fmt.Errorf("no 'com.apple.wkms.fcs-response' found in AEA metadata")
	}
	var fcs struct {
		EncRequest string `json:"enc-request"`
		WrappedKey string `json:"wrapped-key"`
	}
	if err := json.Unmarshal(fcsRaw, &fcs); err != nil {
		return nil, err
	}
	encRequest, err := base64.StdEncoding.DecodeString(fcs.EncRequest)
	if err != nil {
		return nil, err
	}
	wrappedKey, err := base64.StdEncoding.DecodeString(fcs.WrappedKey)
	if err != nil {
		return nil, err
	}
	keyURLBytes, ok := md["com.apple.wkms.fcs-key-url"]
	if !ok {
		return nil, fmt.Errorf("fcs-key-url not found in AEA metadata")
	}
	ecdhKey, err := loadFCSPrivateKey(string(keyURLBytes), cacheDir)
	if err != nil {
		return nil, err
	}
	if ecdhKey.Curve() != ecdh.P256() {
		return nil, fmt.Errorf("fcs-key is not a P-256 key")
	}
	privKey, err := hpke.NewDHKEMPrivateKey(ecdhKey)
	if err != nil {
		return nil, err
	}
	recv, err := hpke.NewRecipient(encRequest, privKey, hpke.HKDFSHA256(), hpke.AES256GCM(), nil)
	if err != nil {
		return nil, err
	}
	return recv.Open(nil, wrappedKey)
}

var fcsHTTPClient = &http.Client{Timeout: 30 * time.Second}

func loadFCSPrivateKey(keyURL, cacheDir string) (*ecdh.PrivateKey, error) {
	u, err := url.Parse(keyURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("fcs-key-url must be an https URL, got %q", keyURL)
	}
	var cachePath string
	if cacheDir != "" {
		cachePath = urlCachePath(cacheDir, "fcs-", keyURL, ".pem")
		if b, err := os.ReadFile(cachePath); err == nil {
			if priv, err := parsePKCS8ECDSAPrivateKey(b); err == nil {
				return priv, nil
			}
		}
	}
	resp, err := fcsHTTPClient.Get(keyURL)
	if err != nil {
		return nil, fmt.Errorf("fetch fcs-key URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch fcs-key URL: %s", resp.Status)
	}
	const maxKeySize = 64 << 10
	pemBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(pemBytes) > maxKeySize {
		return nil, fmt.Errorf("fcs-key response exceeds %d bytes", maxKeySize)
	}
	priv, err := parsePKCS8ECDSAPrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	if cachePath != "" {
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			_ = writeFileAtomic(cacheDir, "fcs-*.tmp", cachePath, pemBytes)
		}
	}
	return priv, nil
}

func parsePKCS8ECDSAPrivateKey(pemBytes []byte) (*ecdh.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("decode PEM fcs-key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse PKCS8 fcs-key: %w", err)
	}
	ecKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("fcs-key is %T, want *ecdsa.PrivateKey", parsed)
	}
	return ecKey.ECDH()
}

// aeaReader provides authenticated, sparse random-access reads over an AEA1 symmetric archive.
type aeaReader struct {
	mu       sync.Mutex
	source   io.ReaderAt
	root     aeaRootHeader
	segments []aeaSegment
	cache    *simplelru.LRU[int, []byte]
	spare    []byte // buffer of the last evicted segment, reused for the next decode
	dataBuf  []byte
	scratch  []byte
}

func (r *aeaReader) Size() int64 { return int64(r.root.FileSize) }

func openAEA(source io.ReaderAt, size int64, key []byte) (*aeaReader, error) {
	mainKey, root, clusterMAC, offset, err := readAEAPrologue(source, size, key)
	if err != nil {
		return nil, err
	}
	checksumBytes, err := validateAEARoot(root, size)
	if err != nil {
		return nil, err
	}
	segments, err := indexAEASegments(source, offset, size, mainKey, clusterMAC, root, checksumBytes)
	if err != nil {
		return nil, err
	}
	r := &aeaReader{source: source, root: root, segments: segments}
	r.cache, err = simplelru.NewLRU(max(1, aeaCacheBudget/int(root.SegmentSize)), func(_ int, data []byte) { r.spare = data })
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *aeaReader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if off < 0 {
		return 0, fmt.Errorf("negative AEA offset")
	}
	n := 0
	for n < len(p) && off < r.Size() {
		index := int(off / int64(r.root.SegmentSize))
		seg := r.segments[index]
		data, ok := r.cache.Get(index)
		if !ok {
			out := r.spare
			r.spare = nil
			var err error
			if data, err = r.decryptSegment(seg, out); err != nil {
				return n, err
			}
			r.cache.Add(index, data)
		}
		count := copy(p[n:], data[off-seg.output:])
		n += count
		off += int64(count)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// decryptSegment returns the plaintext of seg in out (reallocated if too small), which the caller
// then owns. One spare byte past the segment lets the decoders report overlong output.
func (r *aeaReader) decryptSegment(seg aeaSegment, out []byte) ([]byte, error) {
	compSize, decSize := int(seg.compressedSize), int(seg.decompressedSize)
	if cap(out) < decSize+1 {
		out = make([]byte, decSize+1, int(r.root.SegmentSize)+1)
	}
	compressed := compSize != decSize
	data := out[:compSize]
	if compressed {
		if cap(r.dataBuf) < compSize {
			r.dataBuf = make([]byte, compSize)
		}
		data = r.dataBuf[:compSize]
	}
	if err := readExactAt(r.source, data, seg.input); err != nil {
		return nil, fmt.Errorf("read AEA segment data: %w", err)
	}
	var keyInfo [len(aeaSegmentKeyInfo) + 4]byte
	copy(keyInfo[:], aeaSegmentKeyInfo)
	binary.LittleEndian.PutUint32(keyInfo[len(aeaSegmentKeyInfo):], seg.index)
	rawKey, err := hkdf.Expand(sha256.New, seg.clusterPRK, string(keyInfo[:]), 80)
	if err != nil {
		return nil, fmt.Errorf("derive AEA segment key: %w", err)
	}
	km := unpackKeyMaterial(rawKey)
	mac := aeaHMAC(km.mac[:], data, nil)
	if !hmac.Equal(seg.mac[:], mac[:]) {
		return nil, fmt.Errorf("invalid AEA segment #%d HMAC", seg.index)
	}
	if err := aeaCTRInPlace(data, km.key[:], km.iv[:]); err != nil {
		return nil, err
	}

	plain := data
	if compressed {
		plain = out[:decSize]
		switch r.root.Compression {
		case aeaCompLZBITMAP:
			if err := validateLZBitmapSize(data, decSize); err != nil {
				return nil, err
			}
			if status := lzfse.LzBitMapDecompress(data, plain); status != 0 {
				return nil, fmt.Errorf("decompress LZBITMAP segment %d: status %d", seg.index, status)
			}
		case aeaCompLZFSE:
			if r.scratch == nil {
				r.scratch = make([]byte, lzfse.DecodeScratchSize())
			}
			if n := lzfse.DecodeBufferWithScratch(data, out[:decSize+1], r.scratch); n != decSize {
				return nil, fmt.Errorf("invalid LZFSE segment %d size: got %d, want %d", seg.index, n, decSize)
			}
		case aeaCompLZMA, aeaCompLZVN:
			if n := lzfse.DecodeLZVNBuffer(data, out[:decSize+1]); n != uint(decSize) {
				return nil, fmt.Errorf("invalid LZVN segment %d size: got %d, want %d", seg.index, n, decSize)
			}
		case aeaCompZLIB:
			zr, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("zlib reader: %w", err)
			}
			defer zr.Close()
			if _, err := io.ReadFull(zr, plain); err != nil {
				return nil, fmt.Errorf("read zlib segment: %w", err)
			}
			var extra [1]byte
			if n, err := zr.Read(extra[:]); n != 0 || err != io.EOF {
				return nil, fmt.Errorf("invalid zlib segment %d size or checksum", seg.index)
			}
		default:
			return nil, fmt.Errorf("unsupported AEA compression %q", r.root.Compression)
		}
	}

	switch r.root.Checksum {
	case aeaChecksumNone:
	case aeaChecksumSha256:
		if computed := sha256.Sum256(plain); computed != seg.checksum {
			return nil, fmt.Errorf("invalid SHA256 checksum for AEA segment %d (cluster %d)", seg.index, seg.cluster)
		}
	case aeaChecksumMurmur:
		expected := binary.LittleEndian.Uint64(seg.checksum[:8])
		if computed := aeaMurmur64(plain); computed != expected {
			return nil, fmt.Errorf("invalid Murmur checksum for AEA segment %d (cluster %d)", seg.index, seg.cluster)
		}
	default:
		return nil, fmt.Errorf("unsupported AEA checksum %d", r.root.Checksum)
	}
	return plain, nil
}

func readAEAPrologue(r io.ReaderAt, size int64, symmetricKey []byte) (mainKey []byte, root aeaRootHeader, clusterMAC [32]byte, offset int64, err error) {
	if len(symmetricKey) != 32 {
		return nil, root, clusterMAC, 0, fmt.Errorf("invalid AEA symmetric key length %d, want 32", len(symmetricKey))
	}
	hdr, err := readAEAHeader(r, size)
	if err != nil {
		return nil, root, clusterMAC, 0, err
	}
	if hdr.profileID() != aeaProfileSymmetric {
		return nil, root, clusterMAC, 0, fmt.Errorf("unsupported AEA profile %d", hdr.profileID())
	}
	authSize := int64(hdr.AuthDataLength)
	if size < aeaHeaderSize+authSize+32+int64(binary.Size(aeaEncRootHeader{})) {
		return nil, root, clusterMAC, 0, fmt.Errorf("truncated AEA prologue: %w", io.ErrUnexpectedEOF)
	}
	authData := io.NewSectionReader(r, aeaHeaderSize, authSize)
	f := io.NewSectionReader(r, 0, size)
	if _, err := f.Seek(aeaHeaderSize+authSize, io.SeekStart); err != nil {
		return nil, root, clusterMAC, 0, err
	}
	mainSalt := make([]byte, 32)
	if _, err := io.ReadFull(f, mainSalt); err != nil {
		return nil, root, clusterMAC, 0, fmt.Errorf("read AEA main salt: %w", err)
	}
	mainInfo := binary.LittleEndian.AppendUint32([]byte(aeaMainKeyInfo), hdr.ProfileAndScryptStrength)
	mainKey, err = hkdf.Key(sha256.New, symmetricKey, mainSalt, string(mainInfo), len(symmetricKey))
	if err != nil {
		return nil, root, clusterMAC, 0, fmt.Errorf("derive AEA main key: %w", err)
	}
	var encRoot aeaEncRootHeader
	if err := binary.Read(f, binary.LittleEndian, &encRoot); err != nil {
		return nil, root, clusterMAC, 0, fmt.Errorf("read AEA root header: %w", err)
	}
	rawRootKey, err := hkdf.Key(sha256.New, mainKey, nil, aeaRootHeaderKeyInfo, 80)
	if err != nil {
		return nil, root, clusterMAC, 0, fmt.Errorf("derive AEA root header key: %w", err)
	}
	rootKey := unpackKeyMaterial(rawRootKey)
	rhmac, err := aeaRootHeaderHMAC(rootKey.mac[:], encRoot.Data[:], encRoot.ClusterHMAC, authData, hdr.AuthDataLength)
	if err != nil {
		return nil, root, clusterMAC, 0, err
	}
	if !hmac.Equal(encRoot.HMAC[:], rhmac[:]) {
		return nil, root, clusterMAC, 0, fmt.Errorf("invalid AEA root header HMAC")
	}
	rootPlain := encRoot.Data
	if err := aeaCTRInPlace(rootPlain[:], rootKey.key[:], rootKey.iv[:]); err != nil {
		return nil, root, clusterMAC, 0, err
	}
	if err := binary.Read(bytes.NewReader(rootPlain[:]), binary.LittleEndian, &root); err != nil {
		return nil, root, clusterMAC, 0, err
	}
	offset, _ = f.Seek(0, io.SeekCurrent)
	return mainKey, root, encRoot.ClusterHMAC, offset, nil
}

func validateAEARoot(root aeaRootHeader, archiveSize int64) (uint32, error) {
	var checksumBytes uint32
	switch root.Checksum {
	case aeaChecksumNone:
		checksumBytes = 0
	case aeaChecksumMurmur:
		checksumBytes = 8
	case aeaChecksumSha256:
		checksumBytes = 32
	default:
		return 0, fmt.Errorf("unsupported AEA checksum type %d", root.Checksum)
	}
	if root.FileSize > math.MaxInt64 || root.SegmentSize == 0 || root.SegmentSize > aeaMaxSegmentSize ||
		root.SegmentsPerCluster == 0 || root.SegmentsPerCluster > aeaMaxSegsPerCluster {
		return 0, fmt.Errorf("invalid AEA root dimensions")
	}
	if (root.FileSize+uint64(root.SegmentSize)-1)/uint64(root.SegmentSize) > aeaMaxIndexedSegments {
		return 0, fmt.Errorf("AEA segment index exceeds limit")
	}
	clusterSize := uint64(root.SegmentSize) * uint64(root.SegmentsPerCluster)
	if root.FileSize > 0 && (root.FileSize-1)/clusterSize >= 1<<32 {
		return 0, fmt.Errorf("too many AEA clusters")
	}
	if root.EncryptedSize != uint64(archiveSize) {
		return 0, fmt.Errorf("invalid AEA encrypted size: %d, want %d", archiveSize, root.EncryptedSize)
	}
	return checksumBytes, nil
}

func indexAEASegments(r io.ReaderAt, offset, archiveSize int64, mainKey []byte, clusterMAC [32]byte, root aeaRootHeader, checksumBytes uint32) ([]aeaSegment, error) {
	entrySize := int(checksumBytes) + 8
	headersSize := entrySize * int(root.SegmentsPerCluster)
	clusterHeader := make([]byte, headersSize+32+32*int(root.SegmentsPerCluster))
	var segments []aeaSegment
	var output uint64
	for cluster := uint32(0); output < root.FileSize; cluster++ {
		clusterInfo := binary.LittleEndian.AppendUint32([]byte(aeaClusterKeyInfo), cluster)
		clusterKey, err := hkdf.Key(sha256.New, mainKey, nil, string(clusterInfo), len(mainKey))
		if err != nil {
			return nil, fmt.Errorf("derive AEA cluster key: %w", err)
		}
		rawMat, err := hkdf.Key(sha256.New, clusterKey, nil, aeaClusterHeaderKeyInfo, 80)
		if err != nil {
			return nil, fmt.Errorf("derive AEA cluster header key: %w", err)
		}
		material := unpackKeyMaterial(rawMat)
		if offset > archiveSize-int64(len(clusterHeader)) {
			return nil, fmt.Errorf("read AEA cluster %d header: %w", cluster, io.ErrUnexpectedEOF)
		}
		if err := readExactAt(r, clusterHeader, offset); err != nil {
			return nil, fmt.Errorf("read AEA cluster %d header: %w", cluster, err)
		}
		headers, macs := clusterHeader[:headersSize], clusterHeader[headersSize:]
		mac := aeaHMAC(material.mac[:], headers, macs)
		if !hmac.Equal(clusterMAC[:], mac[:]) {
			return nil, fmt.Errorf("invalid AEA cluster #%d HMAC", cluster)
		}
		copy(clusterMAC[:], macs[:32])
		if err := aeaCTRInPlace(headers, material.key[:], material.iv[:]); err != nil {
			return nil, err
		}
		offset += int64(len(clusterHeader))
		prk, err := hkdf.Extract(sha256.New, clusterKey, nil)
		if err != nil {
			return nil, err
		}
		for index := uint32(0); index < root.SegmentsPerCluster; index++ {
			entry := headers[int(index)*entrySize : (int(index)+1)*entrySize]
			seg := aeaSegment{
				cluster:          cluster,
				index:            index,
				decompressedSize: binary.LittleEndian.Uint32(entry),
				compressedSize:   binary.LittleEndian.Uint32(entry[4:]),
				clusterPRK:       prk,
				input:            offset,
				output:           int64(output),
			}
			copy(seg.checksum[:], entry[8:])
			copy(seg.mac[:], macs[32+32*int(index):32+32*(int(index)+1)])
			expected := min(uint64(root.SegmentSize), root.FileSize-output)
			if uint64(seg.decompressedSize) != expected {
				return nil, fmt.Errorf("invalid AEA segment %d (cluster %d) size: %d, want %d", index, cluster, seg.decompressedSize, expected)
			}
			compressed := int64(seg.compressedSize)
			if expected == 0 {
				if compressed != 0 {
					return nil, fmt.Errorf("nonempty unused AEA segment %d (cluster %d)", index, cluster)
				}
				continue
			}
			if compressed == 0 || compressed > int64(root.SegmentSize) || compressed > archiveSize-offset {
				return nil, fmt.Errorf("invalid compressed size for AEA segment %d (cluster %d): %d", index, cluster, compressed)
			}
			segments = append(segments, seg)
			offset += compressed
			output += expected
		}
	}
	return segments, nil
}

// readExactAt reads exactly len(p) bytes at off, accepting (len(p), io.EOF) as allowed by io.ReaderAt
// and reporting a short read as io.ErrUnexpectedEOF.
func readExactAt(r io.ReaderAt, p []byte, off int64) error {
	n, err := r.ReadAt(p, off)
	if n == len(p) && (err == nil || errors.Is(err, io.EOF)) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func aeaCTRInPlace(data, key, iv []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return fmt.Errorf("AES cipher: %w", err)
	}
	cipher.NewCTR(block, iv).XORKeyStream(data, data)
	return nil
}

func aeaHMAC(key, data, salt []byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(salt)
	mac.Write(data)
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(salt)))
	mac.Write(lenBuf[:])
	return [32]byte(mac.Sum(nil))
}

func aeaRootHeaderHMAC(key, data []byte, clusterMAC [32]byte, authData io.Reader, authSize uint32) ([32]byte, error) {
	mac := hmac.New(sha256.New, key)
	mac.Write(clusterMAC[:])
	if _, err := io.CopyN(mac, authData, int64(authSize)); err != nil {
		return [32]byte{}, fmt.Errorf("read AEA auth data: %w", err)
	}
	mac.Write(data)
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(clusterMAC))+uint64(authSize))
	mac.Write(lenBuf[:])
	return [32]byte(mac.Sum(nil)), nil
}

// aeaMurmur64 implements MurmurHash2's 64-bit variant as used by AEA.
func aeaMurmur64(data []byte) uint64 {
	const multiplier uint64 = 0xc6a4a7935bd1e995
	h := uint64(0xE2236FDC26A5F6D2) ^ uint64(len(data))*multiplier
	for len(data) >= 8 {
		k := binary.LittleEndian.Uint64(data)
		k *= multiplier
		k ^= k >> 47
		k *= multiplier
		h = (h ^ k) * multiplier
		data = data[8:]
	}
	for i, b := range data {
		h ^= uint64(b) << (8 * i)
	}
	if len(data) != 0 {
		h *= multiplier
	}
	h ^= h >> 47
	h *= multiplier
	return h ^ (h >> 47)
}

func validateLZBitmapSize(data []byte, remaining int) error {
	if len(data) < 4 || binary.LittleEndian.Uint32(data) != 0x094d425a {
		return fmt.Errorf("invalid LZBITMAP magic")
	}
	data = data[4:]
	for len(data) >= 6 {
		compressed := int(data[0]) | int(data[1])<<8 | int(data[2])<<16
		decoded := int(data[3]) | int(data[4])<<8 | int(data[5])<<16
		if compressed < 6 || compressed > len(data) || decoded > 0x8000 || decoded > remaining {
			return fmt.Errorf("invalid LZBITMAP chunk size")
		}
		remaining -= decoded
		data = data[compressed:]
		if decoded == 0 {
			if remaining != 0 {
				return fmt.Errorf("short LZBITMAP output: missing %d bytes", remaining)
			}
			return nil
		}
	}
	return fmt.Errorf("missing LZBITMAP terminator")
}
