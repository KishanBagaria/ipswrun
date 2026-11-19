package emulator

import (
	"fmt"
	"slices"
	"strings"
)

// libxpc talks to launchd in its own serialization: a magic word and a version, then one object,
// most often a dictionary. This is enough of it to read what a process asks of launchd and to
// write the answer. Ports do not appear in it: an object that is one stands for the next port
// descriptor of the Mach message it travels in.

const (
	xpcMagic   = 0x40585043 // "CPX@"
	xpcVersion = 5

	xpcNull     = 0x1000
	xpcBool     = 0x2000
	xpcInt64    = 0x3000
	xpcUint64   = 0x4000
	xpcDouble   = 0x5000
	xpcDate     = 0x7000
	xpcData     = 0x8000
	xpcString   = 0x9000
	xpcUUID     = 0xA000
	xpcFD       = 0xB000
	xpcSend     = 0xD000
	xpcArray    = 0xE000
	xpcDict     = 0xF000
	xpcEndpoint = 0x12000
	xpcReceive  = 0x15000
)

// xpcPort is a port in an XPC object: the right it arrives as, and its name.
type xpcPort struct {
	kind uint32 // xpcSend, xpcReceive, xpcEndpoint or xpcFD
	name uint32
}

// xpcDictionary keeps the order of its keys, which is the order they are written in.
type xpcDictionary struct {
	keys   []string
	values map[string]any
}

func (d *xpcDictionary) set(key string, value any) *xpcDictionary {
	if d.values == nil {
		d.values = map[string]any{}
	}
	if _, ok := d.values[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.values[key] = value
	return d
}

func (d *xpcDictionary) uint(key string) uint64 {
	switch v := d.values[key].(type) {
	case uint64:
		return v
	case int64:
		return uint64(v)
	}
	return 0
}

func (d *xpcDictionary) bool(key string) bool {
	b, _ := d.values[key].(bool)
	return b
}

func (d *xpcDictionary) string(key string) string {
	s, _ := d.values[key].(string)
	return s
}

func (d *xpcDictionary) String() string {
	var out []string
	for _, key := range d.keys {
		switch v := d.values[key].(type) {
		case []byte:
			out = append(out, fmt.Sprintf("%s: %d bytes", key, len(v)))
		case xpcPort:
			out = append(out, fmt.Sprintf("%s: port %#x", key, v.name))
		case string:
			out = append(out, fmt.Sprintf("%s: %q", key, v))
		default:
			out = append(out, fmt.Sprintf("%s: %v", key, v))
		}
	}
	return "{" + strings.Join(out, ", ") + "}"
}

// xpcReader reads one serialized object; the ports are the names in the message's descriptors,
// in order. A malformed object panics with errXPC.
type xpcReader struct {
	data  []byte
	ports []uint32
}

type errXPC string

func (r *xpcReader) take(n int) []byte {
	if n < 0 || len(r.data) < n {
		panic(errXPC("truncated"))
	}
	out := r.data[:n]
	r.data = r.data[n:]
	return out
}

func (r *xpcReader) u32() uint32 { return le.Uint32(r.take(4)) }
func (r *xpcReader) u64() uint64 { return le.Uint64(r.take(8)) }

func (r *xpcReader) port(kind uint32) xpcPort {
	if len(r.ports) == 0 {
		return xpcPort{kind: kind}
	}
	name := r.ports[0]
	r.ports = r.ports[1:]
	return xpcPort{kind, name}
}

func (r *xpcReader) object() any {
	switch kind := r.u32(); kind {
	case xpcNull:
		return nil
	case xpcBool:
		return r.u32() != 0
	case xpcInt64:
		return int64(r.u64())
	case xpcUint64:
		return r.u64()
	case xpcDouble, xpcDate:
		return r.u64()
	case xpcData:
		n := int(r.u32())
		return slices.Clone(r.take(int(align(uint64(n), 4)))[:n])
	case xpcString:
		n := int(r.u32())
		return strings.TrimRight(string(r.take(int(align(uint64(n), 4)))[:n]), "\x00")
	case xpcUUID:
		return slices.Clone(r.take(16))
	case xpcSend, xpcReceive, xpcEndpoint, xpcFD:
		return r.port(kind)
	case xpcArray:
		r.u32() // its length in bytes
		out := make([]any, 0, 8)
		for range r.u32() {
			out = append(out, r.object())
		}
		return out
	case xpcDict:
		r.u32()
		d := &xpcDictionary{}
		for range r.u32() {
			rest := r.data
			end := slices.Index(rest, 0)
			if end < 0 {
				panic(errXPC("unterminated key"))
			}
			r.take(int(align(uint64(end+1), 4)))
			d.set(string(rest[:end]), r.object())
		}
		return d
	default:
		panic(errXPC(fmt.Sprintf("object of type %#x", kind)))
	}
}

// xpcDecode reads the dictionary a message to launchd carries. ports are the names in the
// message's port descriptors.
func xpcDecode(data []byte, ports []uint32) (d *xpcDictionary, err error) {
	defer func() {
		if r := recover(); r != nil {
			problem, ok := r.(errXPC)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("XPC message: %s", string(problem))
		}
	}()
	r := &xpcReader{data: data, ports: ports}
	if r.u32() != xpcMagic || r.u32() != xpcVersion {
		return nil, fmt.Errorf("XPC message: not version %d of the serialization", xpcVersion)
	}
	d, ok := r.object().(*xpcDictionary)
	if !ok {
		return nil, fmt.Errorf("XPC message: not a dictionary")
	}
	return d, nil
}

func xpcPadded(b []byte) []byte {
	return append(b, make([]byte, int(align(uint64(len(b)), 4))-len(b))...)
}

// xpcAppend writes one object, and returns with it the ports it stands on, in order.
func xpcAppend(out []byte, ports []xpcPort, value any) ([]byte, []xpcPort) {
	switch v := value.(type) {
	case nil:
		out = append(out, le32(xpcNull)...)
	case bool:
		out = append(out, le32(xpcBool, uint32(truth(v)))...)
	case int64:
		out = append(append(out, le32(xpcInt64)...), le64(uint64(v))...)
	case uint64:
		out = append(append(out, le32(xpcUint64)...), le64(v)...)
	case string:
		out = append(append(out, le32(xpcString, uint32(len(v)+1))...), xpcPadded(str(v))...)
	case []byte:
		out = append(append(out, le32(xpcData, uint32(len(v)))...), xpcPadded(slices.Clone(v))...)
	case xpcPort:
		out, ports = append(out, le32(v.kind)...), append(ports, v)
	case *xpcDictionary:
		var body []byte
		for _, key := range v.keys {
			body = append(body, xpcPadded(str(key))...)
			body, ports = xpcAppend(body, ports, v.values[key])
		}
		out = append(append(out, le32(xpcDict, uint32(len(body)+4), uint32(len(v.keys)))...), body...)
	default:
		panic(fmt.Sprintf("xpcAppend: %T", value))
	}
	return out, ports
}

// xpcEncode is a dictionary as libxpc reads it, and the ports that go in the descriptors of the
// message carrying it.
func xpcEncode(d *xpcDictionary) ([]byte, []xpcPort) {
	return xpcAppend(le32(xpcMagic, xpcVersion), nil, d)
}
