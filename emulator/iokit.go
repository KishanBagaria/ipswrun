package emulator

import (
	"fmt"
	"slices"
)

// IOKit is how a process reaches a driver in the kernel: it asks the kernel for the services
// that match a description, opens a connection to one, and calls the driver's methods over it
// by number, with numbers and bytes going in and coming out. All of that is Mach messages to
// ports the kernel receives on, which are answered here. What is not here is any driver. One
// is given to the machine as a Driver (Config.Drivers), and with none the kernel has no
// services at all: every search comes back empty, as on a system where the driver is not loaded.

// Driver is a service of the kernel's that guests can find by its class and open.
type Driver interface {
	// Class is the name it is matched by, as the IOProviderClass of a search.
	Class() string
	// Open makes a connection for a process: kind is the type of connection it asked for. A
	// driver that will not have the client returns the IOReturn that says why.
	Open(client Client, kind uint32) (UserClient, uint32)
}

// UserClient is one process's connection to a driver.
type UserClient interface {
	// Call runs one of the driver's methods.
	Call(call Call) Result
	// Close is called once, when the process closes the connection or has let go of it.
	Close()
}

// Client is the process at the other end of a connection, as a driver may ask about it: who it
// is, what its code signature says of it, and its memory, which a method may be handed
// addresses in.
type Client struct {
	PID, UID     uint32
	Identity     string // the name its executable was signed under, if it was signed
	Entitlements string // what it was signed with, as a property list
	Read         func(addr, n uint64) ([]byte, error)
	Write        func(addr uint64, data []byte) error
}

// Call is one call of a method: its number, and what goes in. A driver may give back at most
// MaxScalars numbers and MaxOutput bytes.
type Call struct {
	Selector   uint32
	Scalars    []uint64
	Input      []byte
	MaxScalars int
	MaxOutput  int
}

// Result is what a method gives back: an IOReturn (0: success), and with success its output.
type Result struct {
	Code    uint32
	Scalars []uint64
	Output  []byte
}

// IOReturn values, and MIG's for a request it cannot read.
const (
	ioReturnNotFound    = 0xE00002F0
	ioReturnBadArgument = 0xE00002C2
	ioReturnUnsupported = 0xE00002C7
	migBadArguments     = 0xFFFFFED0 // -304

	ioServerVersion = 20150715 // what the kernel says of its side of the interface
	ioInbandMax     = 4096     // bytes that travel inside a message: more is left in the caller's memory
	ioScalarMax     = 16
)

// kernelObject is something of the kernel's that a process holds a port to and sends requests.
type kernelObject interface {
	// request answers the message with that id; false if the object has no such routine.
	request(p *Process, id uint32, body []byte) (migReply, bool)
	// released is called when the last right to the object has gone.
	released()
}

// kernelRight gives the process a send right to a new port that stands for an object.
func (p *Process) kernelRight(object kernelObject) uint32 {
	return p.sendRight(&port{m: p.m, object: object})
}

// migObject is a reply that hands over a right to a new object.
func (p *Process) migObject(object kernelObject) migReply { return migPort(p.kernelRight(object)) }

// ioMain is what every search starts from.
type ioMain struct{}

func (ioMain) released() {}

// migHostGetIOMain gives the process the port IOKit's searches are sent to.
func migHostGetIOMain(p *Process, _ []byte) migReply {
	if p.m.ioMain == nil || p.m.ioMain.dead { // nobody has had it, or everybody has let go of it
		p.m.ioMain = &port{m: p.m, object: ioMain{}}
	}
	return migPort(p.sendRight(p.m.ioMain))
}

func (ioMain) request(p *Process, id uint32, body []byte) (migReply, bool) {
	switch id {
	case 2877: // io_server_version
		return migData(le64(ioServerVersion)), true
	case 2880, 2881: // io_service_get_matching_service_bin, io_service_get_matching_services_bin
		r := ioReader{data: body, at: 32}
		matching := r.bytes(int(r.u32()))
		if r.failed {
			return migCode(migBadArguments), true
		}
		found := p.m.matching(p, matching)
		if id == 2881 {
			return p.migObject(&ioIterator{services: found}), true
		}
		if len(found) == 0 {
			return migCode(ioReturnNotFound), true
		}
		return found[0].found(p), true
	case 2884: // io_service_add_notification_bin
		// To be told when a service of a description arrives, is matched to its driver, or
		// goes. The answer is an iterator of those already there, and the port given is sent
		// a message for each one after: none, as the drivers of a machine are all it will have.
		at, ok := descriptors(body, func(int, byte) {})
		r := ioReader{data: body, at: at + 8}
		kind := r.text()
		matching := r.bytes(int(r.u32()))
		if !ok || r.failed {
			return migCode(migBadArguments), true
		}
		var found []ioFound
		switch kind {
		case "IOServicePublish", "IOServiceFirstPublish", "IOServiceMatched", "IOServiceFirstMatch":
			found = p.m.matching(p, matching)
		case "IOServiceTerminate", "IOServiceWillTerminate":
		default:
			return migReply{}, false
		}
		return p.migObject(&ioIterator{services: found}), true
	}
	return p.registryRequest(id, body)
}

// matching are the services a search describes: drivers, and the device itself, which is the
// one service the kernel makes with no driver (see registry.go). The description is a dictionary
// in the kernel's binary serialization, and the one key understood in it is the class.
func (m *Machine) matching(p *Process, description []byte) []ioFound {
	d, ok := ioDictionary(description)
	class, _ := d["IOProviderClass"].(string)
	p.note("  IOKit search for %v", d)
	if !ok || class == "" || len(d) != 1 {
		return nil
	}
	var found []ioFound
	if class == deviceClass {
		if device := m.device(); device != nil {
			found = append(found, device)
		}
	}
	if class == powerClass {
		found = append(found, powerRoot{})
	}
	for _, driver := range m.cfg.Drivers {
		if driver.Class() == class {
			found = append(found, ioService{driver})
		}
	}
	return found
}

// ioFound is what an iterator hands out: a service, or an entry of the registry.
type ioFound interface {
	// found is a reply that hands over a right to it.
	found(p *Process) migReply
}

// ioIterator is the result of a search: the services found, handed out one at a time.
type ioIterator struct {
	services []ioFound
	next     int
}

func (*ioIterator) released() {}

func (o *ioIterator) request(p *Process, id uint32, body []byte) (migReply, bool) {
	switch id {
	case 2802: // io_iterator_next: nothing, once they have all been given
		if o.next >= len(o.services) {
			return migPort(0), true
		}
		o.next++
		return o.services[o.next-1].found(p), true
	case 2803: // io_iterator_reset
		o.next = 0
		return migCode(0), true
	case 2834: // io_iterator_is_valid: no service arrives or goes
		return migData(le32(1)), true
	}
	return migReply{}, false
}

// ioService is a driver as a process holds it.
type ioService struct{ driver Driver }

func (ioService) released() {}

func (o ioService) found(p *Process) migReply { return p.migObject(o) }

func (o ioService) request(p *Process, id uint32, body []byte) (migReply, bool) {
	if id != 2862 { // io_service_open_extended
		return migReply{}, false
	}
	// The task that will own the connection and properties for it come first, as descriptors;
	// then the type of connection asked for.
	at, ok := descriptors(body, func(int, byte) {})
	r := ioReader{data: body, at: at + 8}
	kind := r.u32()
	if !ok || r.failed {
		return migCode(migBadArguments), true
	}
	client, code := o.driver.Open(p.ioClient(), kind)
	p.note("  IOKit open of %s, type %d: %#x", o.driver.Class(), kind, code)
	var connection uint32
	if code == 0 {
		connection = p.kernelRight(&ioConnection{client: client})
	}
	return migPortData(connection, le32(code)), true
}

// ioClient is the process as a driver sees it.
func (p *Process) ioClient() Client {
	guarded := func(f func()) (err error) {
		defer func() {
			if r := recover(); r != nil {
				bad, ok := r.(fault)
				if !ok {
					panic(r)
				}
				err = fmt.Errorf("%s", bad.message)
			}
		}()
		f()
		return nil
	}
	return Client{PID: p.pid, UID: p.uid, Identity: p.identity, Entitlements: p.entitlements,
		Read: func(addr, n uint64) (data []byte, err error) {
			err = guarded(func() { data = p.read(addr, n) })
			return data, err
		},
		Write: func(addr uint64, data []byte) error { return guarded(func() { p.write(addr, data) }) }}
}

// ioConnection is an open connection to a driver.
type ioConnection struct {
	client UserClient
	closed bool
}

func (o *ioConnection) released() {
	if !o.closed {
		o.closed = true
		o.client.Close()
	}
}

func (o *ioConnection) request(p *Process, id uint32, body []byte) (migReply, bool) {
	switch id {
	case 2816: // io_service_close
		o.released()
		return migCode(0), true
	case 2865: // io_connect_method
		if o.closed {
			return migCode(ioReturnBadArgument), true
		}
		// Numbers and bytes in, each with its count before it; bytes too many to travel in the
		// message are named by where they are in the caller's memory instead. Then how much
		// room there is for what comes out, of each kind and in each place.
		r := ioReader{data: body, at: 32}
		call := Call{Selector: r.u32()}
		for range min(r.u32(), ioScalarMax) {
			call.Scalars = append(call.Scalars, r.u64())
		}
		call.Input = r.bytes(int(r.u32()))
		inputAt, inputSize := r.u64(), r.u64()
		inbandRoom, scalarRoom := r.u32(), r.u32()
		outputAt, outputRoom := r.u64(), r.u64()
		if r.failed || inbandRoom > ioInbandMax || scalarRoom > ioScalarMax || inputSize > 1<<26 || outputRoom > 1<<26 {
			return migCode(migBadArguments), true
		}
		if inputAt != 0 {
			call.Input = p.read(inputAt, inputSize)
		}
		call.MaxScalars, call.MaxOutput = int(scalarRoom), int(inbandRoom)
		if outputAt != 0 {
			call.MaxOutput = int(outputRoom)
		}
		result := o.client.Call(call)
		p.note("  IOKit method %d (%d numbers, %d bytes in): %#x, %d numbers, %d bytes out", call.Selector, len(call.Scalars), len(call.Input), result.Code, len(result.Scalars), len(result.Output))
		switch {
		case result.Code != 0:
			return migCode(result.Code), true
		case len(result.Scalars) > call.MaxScalars || len(result.Output) > call.MaxOutput:
			stopf("the driver's method %d gave back %d numbers and %d bytes, where there was room for %d and %d", call.Selector, len(result.Scalars), len(result.Output), call.MaxScalars, call.MaxOutput)
		}
		var inband []byte
		var left uint64
		if outputAt != 0 {
			p.write(outputAt, result.Output)
			left = uint64(len(result.Output))
		} else {
			inband = result.Output
		}
		out := append(le32(uint32(len(inband))), xpcPadded(slices.Clone(inband))...)
		out = append(append(out, le32(uint32(len(result.Scalars)))...), le64(result.Scalars...)...)
		return migData(append(out, le64(left)...)), true
	}
	return migReply{}, false
}

// ioReader reads the fields of a request one after another. Past the end it reads zeros, and
// says so in failed.
type ioReader struct {
	data   []byte
	at     int
	failed bool
}

// bytes is n bytes, and the padding after them to a whole word.
func (r *ioReader) bytes(n int) []byte {
	if n < 0 || r.at < 0 || r.at+n > len(r.data) {
		r.failed = true
		return make([]byte, max(min(n, 8), 0))
	}
	out := r.data[r.at : r.at+n]
	r.at += int(align(uint64(n), 4))
	return out
}

func (r *ioReader) u32() uint32 { return le.Uint32(r.bytes(4)) }
func (r *ioReader) u64() uint64 { return le.Uint64(r.bytes(8)) }

// The kernel's binary serialization of an object: a magic word, then for each object a word of
// its type, its length, and whether it is the last of the collection it is in.
const (
	ioSerializeMagic = 0x000000D3
	ioLast           = 0x80000000
	ioDictionaryType = 0x01
	ioNumberType     = 0x04
	ioSymbolType     = 0x08
	ioStringType     = 0x09
	ioBooleanType    = 0x0B
)

// ioDictionary reads a serialized dictionary of strings, numbers and booleans; false if it is
// anything else.
func ioDictionary(data []byte) (map[string]any, bool) {
	r := ioReader{data: data}
	head := r.u32()
	entries := r.u32()
	if head != ioSerializeMagic || entries>>24&0x7F != ioDictionaryType {
		return nil, false
	}
	// value reads one object that is not a collection, and whether its word could be read.
	value := func() (any, bool) {
		word := r.u32()
		switch length := int(word & 0xFFFFFF); word >> 24 & 0x7F {
		case ioSymbolType: // with the zero that ends it
			return string(r.bytes(length)[:max(length-1, 0)]), !r.failed
		case ioStringType:
			return string(r.bytes(length)), !r.failed
		case ioNumberType:
			return r.u64(), !r.failed
		case ioBooleanType:
			return length != 0, !r.failed
		}
		return nil, false
	}
	d := map[string]any{}
	for range entries & 0xFFFFFF {
		key, ok := value()
		name, isName := key.(string)
		v, ok2 := value()
		if !ok || !ok2 || !isName {
			return nil, false
		}
		d[name] = v
	}
	return d, true
}
