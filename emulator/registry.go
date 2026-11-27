package emulator

import (
	"bytes"
	"errors"
	"slices"
	"strings"
)

// The I/O Registry is the kernel's tree of the machine's hardware, which a process reads over the
// same port it searches for drivers on. What is here of it is what the restore image says of the
// device: its device tree, the tree the boot loader hands the kernel and the kernel shows as the
// plane IODeviceTree. Nothing a driver would add is here, as no driver is.
//
// The tree is as the image has it and not as the boot loader leaves it, so what the boot loader
// fills in is left out and not invented: a value the image marks as one to be filled, and a value
// that is all zeros, which is how the image holds the place of most of the others (limits.md has
// what that rule was measured against). Left out with them is everything
// only a running device has: its serial number, what is in its NVRAM, and the classes, locations
// and identifiers its drivers give the entries.

const (
	dtPlane      = "IODeviceTree"
	servicePlane = "IOService"

	ioSerializeData = 0x0A

	ioRegistryRecursively = 1
	ioRegistryParents     = 2

	ioReturnNoDevice = 0xE00002C0

	// deviceClass is the class of the entry the device tree hangs from, which the kernel
	// itself makes of the tree, whatever the device, and deviceClasses what that class is a
	// kind of. No other entry's class is known: each is its driver's.
	deviceClass = "IOPlatformExpertDevice"
)

var deviceClasses = []string{deviceClass, "IOService", "IORegistryEntry", "OSObject"}

// ioEntry is an entry of the registry: its root, or a node of the device tree.
type ioEntry struct {
	name       string // as the device tree names it
	properties []ioProperty
	children   []*ioEntry
	parent     *ioEntry
	root       bool  // the registry's root, which the device tree hangs from
	kernelOnly bool  // the kernel keeps what it holds from processes
	port       *port // what processes hold it by, once one has
}

type ioProperty struct {
	name  string
	value []byte
}

func (*ioEntry) released() {}

// parseDeviceTree reads a flattened device tree: for each node its properties, each a name of 32
// bytes, a length and the value, and then its children.
func parseDeviceTree(data []byte) (*ioEntry, error) {
	bad := errors.New("the device tree is cut short")
	at := 0
	var node func(depth int) (*ioEntry, error)
	node = func(depth int) (*ioEntry, error) {
		if at+8 > len(data) || depth > 64 {
			return nil, bad
		}
		properties, children := le.Uint32(data[at:]), le.Uint32(data[at+4:])
		at += 8
		e := &ioEntry{}
		for range properties {
			if at+36 > len(data) {
				return nil, bad
			}
			name, _, _ := strings.Cut(string(data[at:at+32]), "\x00")
			word := le.Uint32(data[at+32:])
			length := int(word & 0x7FFFFFFF)
			at += 36
			if length > len(data)-at {
				return nil, bad
			}
			value := data[at : at+length]
			at += int(align(uint64(length), 4))
			switch {
			case name == "name":
				e.name = string(bytes.TrimRight(value, "\x00"))
			case name == "kernel-only":
				e.kernelOnly = true
			}
			// To be filled in by the boot loader, or holding the place of what it fills in.
			if word>>31 != 0 || len(value) > 0 && !slices.ContainsFunc(value, func(b byte) bool { return b != 0 }) {
				continue
			}
			e.properties = append(e.properties, ioProperty{name, value})
		}
		for range children {
			child, err := node(depth + 1)
			if err != nil {
				return nil, err
			}
			child.parent = e
			e.children = append(e.children, child)
		}
		return e, nil
	}
	tree, err := node(0)
	if err != nil {
		return nil, err
	}
	if at > len(data) {
		return nil, bad
	}
	return tree, nil
}

// registry is the root of the I/O Registry, with the device tree under it; nil on a machine that
// was given no device tree. A tree that cannot be read stops the run.
func (m *Machine) registry() *ioEntry {
	if m.ioRoot != nil || m.cfg.DeviceTree == nil {
		return m.ioRoot
	}
	data, err := m.cfg.DeviceTree()
	if err != nil {
		stopf("the I/O Registry is the device's tree, which could not be read: %v", err)
	}
	tree, err := parseDeviceTree(data)
	if err != nil {
		stopf("the I/O Registry is the device's tree: %v", err)
	}
	m.ioRoot = &ioEntry{name: "Root", root: true, children: []*ioEntry{tree}}
	tree.parent = m.ioRoot
	return m.ioRoot
}

// device is the entry the device tree hangs from: the device itself. Nil with no registry.
func (m *Machine) device() *ioEntry {
	if root := m.registry(); root != nil {
		return root.children[0]
	}
	return nil
}

// isDevice says whether e is the device itself.
func (e *ioEntry) isDevice() bool { return e.parent != nil && e.parent.root }

// found is a reply that hands over a right to an entry, the same port each time: two rights to
// one entry are one name in a process.
func (e *ioEntry) found(p *Process) migReply {
	if e.port == nil || e.port.dead {
		e.port = &port{m: p.m, object: e}
	}
	return migPort(p.sendRight(e.port))
}

// migEntry is a reply that hands over a right to an entry, or no port for none.
func (p *Process) migEntry(e *ioEntry) migReply {
	if e == nil {
		return migPort(0)
	}
	return e.found(p)
}

// property is the value of the one of some properties that has a name.
func property(properties []ioProperty, name string) ([]byte, bool) {
	for _, property := range properties {
		if property.name == name {
			return property.value, true
		}
	}
	return nil, false
}

// inPlane are the entries next to e in a plane: its children, or its parents. The device tree's
// plane is all of it. In the plane of services there is the root and the device itself, and no
// service under it, as no driver is here; no other plane exists.
func (e *ioEntry) inPlane(plane string, parents bool) []*ioEntry {
	if plane != dtPlane && (plane != servicePlane || !e.root && !e.isDevice()) {
		return nil
	}
	if parents {
		if e.parent == nil {
			return nil
		}
		return []*ioEntry{e.parent}
	}
	if plane == servicePlane && !e.root {
		return nil
	}
	return e.children
}

// isPlane says whether there is a plane of that name: the device tree's, and that of services.
func isPlane(plane string) bool { return plane == dtPlane || plane == servicePlane }

// knownPlane says whether what the registry holds of a plane is the same as a device's: of the device
// tree's it is, and of any other it is for the root and the device, which have no more here than
// each other.
func (p *Process) knownPlane(e *ioEntry, plane, what string) bool {
	if plane == dtPlane || e.root {
		return true
	}
	return !p.unimplemented("I/O Registry "+what+" in the plane "+plane, nil)
}

// fromPath is the entry a path names: a plane, a colon, and from the plane's root the names of
// the entries on the way, each after a slash. Nil if there is none.
func (p *Process) fromPath(root *ioEntry, path string) *ioEntry {
	plane, rest, ok := strings.Cut(path, ":")
	if !ok || !strings.HasPrefix(rest, "/") || !isPlane(plane) {
		return nil
	}
	e := root.children[0]
	if rest == "/" {
		return e
	}
	for _, name := range strings.Split(rest[1:], "/") {
		if strings.Contains(name, "@") {
			// An entry's location is given to it by its driver, and is not in the tree.
			p.unimplemented("I/O Registry path with a location, "+path, nil)
			return nil
		}
		below := e.inPlane(plane, false)
		at := slices.IndexFunc(below, func(c *ioEntry) bool { return c.name == name })
		if name == "" || at < 0 {
			return nil
		}
		e = below[at]
	}
	return e
}

// registryRequest answers what is asked of the registry as a whole, over the port searches are
// sent to; false for a request that is not one of these, or on a machine with no device tree.
func (p *Process) registryRequest(id uint32, body []byte) (migReply, bool) {
	if id != 2809 && id != 2827 && id != 2806 {
		return migReply{}, false
	}
	root := p.m.registry() // read only now that it is asked for
	if root == nil {
		return migReply{}, false
	}
	r := ioReader{data: body, at: 32}
	switch id {
	case 2809: // io_registry_entry_from_path
		path := r.text()
		if r.failed {
			return migCode(migBadArguments), true
		}
		if path == powerPath || path == powerServicePath { // the kernel's own service for power (power.go)
			p.note("  I/O Registry entry at %s: found true", path)
			return powerRoot{}.found(p), true
		}
		e := p.fromPath(root, path)
		p.note("  I/O Registry entry at %s: found %t", path, e != nil)
		return p.migEntry(e), true
	case 2827: // io_registry_get_root_entry
		return p.migEntry(root), true
	case 2806: // io_registry_create_iterator
		plane := r.text()
		options := r.u32()
		if r.failed {
			return migCode(migBadArguments), true
		}
		return p.registryIterator(root, plane, options), true
	}
	return migReply{}, false
}

// registryIterator is a reply with an iterator over a plane from an entry.
func (p *Process) registryIterator(from *ioEntry, plane string, options uint32) migReply {
	if !isPlane(plane) {
		return migCode(ioReturnBadArgument)
	}
	return p.migObject(newWalk(from, plane, options))
}

func (e *ioEntry) request(p *Process, id uint32, body []byte) (migReply, bool) {
	r := ioReader{data: body, at: 32}
	if e.isDevice() {
		switch id {
		case 2810: // io_registry_entry_get_name: for its board, the first thing its tree says it is
			compatible, _ := property(e.properties, "compatible")
			name, _, _ := strings.Cut(string(compatible), "\x00")
			if name == "" {
				return migReply{}, false
			}
			return migName(name), true
		case 2800: // io_object_get_class
			return migName(deviceClass), true
		case 2801: // io_object_conforms_to
			class := r.text()
			if r.failed {
				return migCode(migBadArguments), true
			}
			conforms := uint32(0)
			if slices.Contains(deviceClasses, class) {
				conforms = 1
			}
			return migData(le32(conforms)), true
		}
	}
	switch id {
	case 2810: // io_registry_entry_get_name
		return migName(e.name), true
	case 2843: // io_registry_entry_get_name_in_plane
		plane := r.text()
		if r.failed {
			return migCode(migBadArguments), true
		}
		if plane != dtPlane && !e.root {
			return migReply{}, false
		}
		return migName(e.name), true
	case 2829: // io_registry_entry_in_plane: in a plane there is not, as in any
		return migData(le32(1)), true
	case 2813, 2814: // io_registry_entry_get_child_iterator, io_registry_entry_get_parent_iterator
		plane := r.text()
		if r.failed {
			return migCode(migBadArguments), true
		}
		if !isPlane(plane) {
			return migPort(0), true
		}
		if !p.knownPlane(e, plane, "entries") {
			return migReply{}, false
		}
		var next []ioFound
		for _, entry := range e.inPlane(plane, id == 2814) {
			next = append(next, entry)
		}
		return p.migObject(&ioIterator{services: next}), true
	case 2833: // io_registry_entry_create_iterator
		plane := r.text()
		options := r.u32()
		if r.failed {
			return migCode(migBadArguments), true
		}
		if !p.knownPlane(e, plane, "entries") {
			return migReply{}, false
		}
		return p.registryIterator(e, plane, options), true
	case 2889: // io_registry_entry_get_property_bin_buf
		plane, name := r.text(), r.text()
		options := r.u32()
		buf, room := r.u64(), r.u64()
		if r.failed {
			return migCode(migBadArguments), true
		}
		value, found := e.search(p, plane, name, options)
		p.note("  I/O Registry property %s of %s: found %t", name, e.name, found)
		if !found {
			return migCode(ioReturnNotFound), true
		}
		return p.migSerialized(append(le32(ioSerializeMagic), serializedData(value, true)...), buf, room), true
	case 2888: // io_registry_entry_get_properties_bin_buf
		buf, room := r.u64(), r.u64()
		if r.failed {
			return migCode(migBadArguments), true
		}
		properties := e.readable(p)
		out := le32(ioSerializeMagic, ioLast|ioDictionaryType<<24|uint32(len(properties)))
		for i, property := range properties {
			out = append(out, le32(ioSymbolType<<24|uint32(len(property.name)+1))...)
			out = xpcPadded(append(out, str(property.name)...))
			out = append(out, serializedData(property.value, i == len(properties)-1)...)
		}
		return p.migSerialized(out, buf, room), true
	}
	return migReply{}, false
}

// readable are an entry's properties, unless they are not all known: the root's are the
// kernel's own, and what the kernel shows of an entry it keeps to itself is not what the tree
// holds.
func (e *ioEntry) readable(p *Process) []ioProperty {
	if e.root || e.kernelOnly {
		p.unimplemented("I/O Registry properties of "+e.name, nil)
		return nil
	}
	return e.properties
}

// search is a property of an entry, or as the options ask of the first entry that has it among
// those below it in the plane, or above it.
func (e *ioEntry) search(p *Process, plane, name string, options uint32) ([]byte, bool) {
	if value, found := property(e.readable(p), name); found || options&ioRegistryRecursively == 0 {
		return value, found
	}
	if !p.knownPlane(e, plane, "search") {
		return nil, false
	}
	it := newWalk(e, plane, options)
	for next := it.step(); next != nil; next = it.step() {
		if value, found := property(next.readable(p), name); found {
			return value, true
		}
	}
	return nil, false
}

// serializedData is bytes in the kernel's binary serialization.
func serializedData(value []byte, last bool) []byte {
	word := ioSerializeData<<24 | uint32(len(value))
	if last {
		word |= ioLast
	}
	return xpcPadded(append(le32(word), value...))
}

// ioText is a string as a request or a reply carries it: where it starts, its length with the
// zero that ends it, and it.
func ioText(s string) []byte { return append(le32(0, uint32(len(s)+1)), xpcPadded(str(s))...) }

// migName is a reply of a name.
func migName(name string) migReply { return migData(ioText(name)) }

// migSerialized is a reply of serialized properties: in the caller's buffer if it gave one they
// fit, and otherwise in memory of their own.
func (p *Process) migSerialized(data []byte, buf, room uint64) migReply {
	if buf != 0 && uint64(len(data)) <= room {
		p.write(buf, data)
		return migMemory(nil, append(le64(uint64(len(data))), le32(0)...))
	}
	return migMemory(data, append(le64(0), le32(uint32(len(data)))...))
}

// text reads a string that travels with where it starts and its length before it.
func (r *ioReader) text() string {
	r.u32()
	text, _, _ := strings.Cut(string(r.bytes(int(r.u32()))), "\x00")
	return text
}

// ioWalk is an iterator that walks a plane from an entry, handing out entries one at a time: it
// goes into each entry it has handed out when told to, or by itself.
type ioWalk struct {
	level   []ioCursor
	from    *ioEntry
	plane   string
	options uint32
}

func newWalk(from *ioEntry, plane string, options uint32) *ioWalk {
	o := &ioWalk{from: from, plane: plane, options: options}
	o.reset()
	return o
}

// ioCursor is how far the iterator is among the entries next to one.
type ioCursor struct {
	entries []*ioEntry
	next    int
	current *ioEntry
}

func (*ioWalk) released() {}

func (o *ioWalk) reset() {
	o.level = nil
	o.enter(o.from)
}

// enter starts on the entries next to one, below those it is among.
func (o *ioWalk) enter(e *ioEntry) {
	o.level = append(o.level, ioCursor{entries: e.inPlane(o.plane, o.options&ioRegistryParents != 0)})
}

// exit goes back to the entries the last one entered is among; false at the top.
func (o *ioWalk) exit() bool {
	if len(o.level) < 2 {
		return false
	}
	o.level = o.level[:len(o.level)-1]
	return true
}

// step is the next entry: of the innermost entries, and with them at an end, for a walk that
// goes into entries by itself, of those around them.
func (o *ioWalk) step() *ioEntry {
	flat := func() *ioEntry {
		at := &o.level[len(o.level)-1]
		at.current = nil
		if at.next < len(at.entries) {
			at.current = at.entries[at.next]
			at.next++
		}
		return at.current
	}
	next := flat()
	if o.options&ioRegistryRecursively == 0 {
		return next
	}
	for next == nil && o.exit() {
		next = flat()
	}
	if next != nil { // once each: an entry is under one other, or above one
		o.enter(next)
	}
	return next
}

func (o *ioWalk) request(p *Process, id uint32, body []byte) (migReply, bool) {
	switch id {
	case 2802: // io_iterator_next
		return p.migEntry(o.step()), true
	case 2803: // io_iterator_reset
		o.reset()
		return migCode(0), true
	case 2834: // io_iterator_is_valid: nothing joins or leaves the registry
		return migData(le32(1)), true
	case 2807: // io_registry_iterator_enter_entry: the one it handed out last
		at := o.level[len(o.level)-1]
		if at.current == nil {
			return migReply{}, false
		}
		o.enter(at.current)
		return migCode(0), true
	case 2808: // io_registry_iterator_exit_entry
		if !o.exit() {
			return migCode(ioReturnNoDevice), true
		}
		return migCode(0), true
	}
	return migReply{}, false
}
