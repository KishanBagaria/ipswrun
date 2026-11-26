package emulator

import (
	"bytes"
	"slices"
	"testing"
)

// dtTestNode is a node of a device tree to flatten: its properties in order, each a name and a
// value, with "!" before the name of one the boot loader is to fill in.
type dtTestNode struct {
	properties []string
	children   []dtTestNode
}

func (n dtTestNode) flatten() []byte {
	out := le32(uint32(len(n.properties)/2), uint32(len(n.children)))
	for i := 0; i < len(n.properties); i += 2 {
		name, value := n.properties[i], n.properties[i+1]
		length := uint32(len(value))
		if name[0] == '!' {
			name, length = name[1:], length|1<<31
		}
		out = append(append(out, append([]byte(name), make([]byte, 32-len(name))...)...), le32(length)...)
		out = xpcPadded(append(out, value...))
	}
	for _, child := range n.children {
		out = append(out, child.flatten()...)
	}
	return out
}

var testDeviceTree = dtTestNode{
	properties: []string{"name", "device-tree\x00", "compatible", "J1AP\x00Test1,1\x00", "model", "Test1,1\x00",
		"!serial-number", "syscfg/SrNm\x00", "clock-frequency", "\x00\x00\x00\x00"},
	children: []dtTestNode{
		{properties: []string{"name", "chosen\x00", "chip-id", "\x00\x00\x00\x00"},
			children: []dtTestNode{{properties: []string{"name", "memory-map\x00", "kernel-only", "true\x00", "base", "\x01"}}}},
		{properties: []string{"name", "product\x00", "product-name", "Test Machine\x00", "empty", "", "zero", "\x00\x00\x00\x00"},
			children: []dtTestNode{{properties: []string{"name", "camera\x00"}}, {properties: []string{"name", "audio\x00", "model", "A1\x00"}}}},
		{properties: []string{"name", "options\x00"}},
	},
}

// What the registry answers is what a Mac's kernel answered the same requests with (macOS
// 27.2.1 on a Mac15,9), in the messages IOKit.framework sends.
func TestRegistry(t *testing.T) {
	p := newProcess(t)
	reads := 0
	p.m.cfg.DeviceTree = func() ([]byte, error) {
		reads++
		return testDeviceTree.flatten(), nil
	}
	onMain(func() {
		const out, msg, in_, buf = dataAddr + 0x40, dataAddr + 0x100, dataAddr + 0x2000, dataAddr + 0x6000
		p.trap(-16, 0x203, rightReceive, out)
		answers := p.u32(out)
		// ask sends a request to a port of the kernel's and returns the answer without its header.
		ask := func(to, id uint32, body []byte) []byte {
			t.Helper()
			request := append(header(copySend|makeSendOnce<<8, uint32(24+len(body)), answers, id), body...)
			le.PutUint32(request[8:], to)
			p.write(msg, request)
			if x0, _, stopped := p.trap(-31, msg, machSendMsg, uint64(len(request))); x0 != 0 || stopped != "" {
				t.Fatalf("request %d to %#x: %#x %s", id, to, x0, stopped)
			}
			p.write(in_, make([]byte, 8192))
			if x0, _, _ := p.trap(-31, in_, machRcvMsg|machRcvTimeout, 0, 8192, uint64(answers), 0); x0 != 0 || p.u32(in_+20) != id+100 {
				t.Fatalf("answer to request %d: %#x, id %d", id, x0, p.u32(in_+20))
			}
			return p.read(in_+24, uint64(p.u32(in_+4)-24))
		}
		text := ioText
		with := func(parts ...[]byte) []byte { return bytes.Join(append([][]byte{ndr}, parts...), nil) }
		port := func(answer []byte) uint32 { return le.Uint32(answer[4:]) }
		main := port(ask(uint32(machHostSelf(p, nil)), 205, nil))
		entry := func(path string) uint32 { return port(ask(main, 2809, with(text(path)))) }
		name := func(of uint32) string {
			answer := ask(of, 2810, nil)
			return string(answer[20 : 20+le.Uint32(answer[16:])-1])
		}
		// names are the entries an iterator hands out, to its end.
		names := func(iterator uint32) (all []string) {
			for {
				next := port(ask(iterator, 2802, nil))
				if next == 0 {
					return all
				}
				all = append(all, name(next))
			}
		}
		same := func(got []string, want ...string) bool { return slices.Equal(got, want) }

		// Paths: a plane and the names from its root. The plane of services has the device
		// and nothing under it.
		product := entry("IODeviceTree:/product")
		if product == 0 || entry("IODeviceTree:/product") != product || reads != 1 {
			t.Fatalf("the product entry is %#x, then %#x; the tree was read %d times", product, entry("IODeviceTree:/product"), reads)
		}
		for _, none := range []string{"IODeviceTree:/nonesuch", "IODeviceTree:product", "/product", "IODeviceTree:/product/", "Nonplane:/product", "IOService:/product"} {
			if got := entry(none); got != 0 {
				t.Errorf("an entry at %s: %#x", none, got)
			}
		}
		device := entry("IODeviceTree:/")
		if device == 0 || entry("IOService:/") != device || name(device) != "J1AP" || name(product) != "product" {
			t.Errorf("the device is %#x in its tree and %#x among the services, named %q; the product is named %q", device, entry("IOService:/"), name(device), name(product))
		}
		if answer := ask(device, 2843, with(text(dtPlane))); string(answer[20:31]) != "device-tree" {
			t.Errorf("the device's name in the tree: % x", answer)
		}
		root := port(ask(main, 2827, nil))
		if root == 0 || name(root) != "Root" {
			t.Errorf("the root is %#x, named %q", root, name(root))
		}
		if in := ask(product, 2829, with(text("Nonplane"))); le.Uint32(in[8:]) != 0 || le.Uint32(in[12:]) != 1 {
			t.Errorf("in a plane there is not: % x", in)
		}

		// A property comes serialized, into the caller's buffer if it fits there and into
		// memory of its own if not. What the boot loader fills in is not there to read.
		property := func(of uint32, name string, options uint32, room uint64) (code uint32, value []byte) {
			answer := ask(of, 2889, with(text(dtPlane), text(name), le32(options), le64(buf, room)))
			if len(answer) == 12 {
				return le.Uint32(answer[8:]), nil
			}
			data := p.read(buf, le.Uint64(answer[28:]))
			if n := le.Uint32(answer[36:]); n != 0 {
				data = p.read(le.Uint64(answer[4:]), uint64(n))
			}
			if le.Uint32(data) != ioSerializeMagic || le.Uint32(data[4:])>>24 != ioLast>>24|ioSerializeData {
				t.Errorf("the property %s is serialized as % x", name, data)
			}
			return 0, data[8 : 8+le.Uint32(data[4:])&0xFFFFFF]
		}
		for _, room := range []uint64{4096, 8} {
			if code, value := property(product, "product-name", 0, room); code != 0 || string(value) != "Test Machine\x00" {
				t.Errorf("product-name with room for %d: %#x %q", room, code, value)
			}
		}
		if code, value := property(product, "empty", 0, 4096); code != 0 || len(value) != 0 {
			t.Errorf("a property that is empty: %#x %q", code, value)
		}
		for of, name := range map[uint32]string{product: "zero", device: "serial-number", entry("IODeviceTree:/chosen"): "chip-id", entry("IODeviceTree:/options"): "boot-args"} {
			if code, _ := property(of, name, 0, 4096); code != ioReturnNotFound {
				t.Errorf("the property %s, which the image does not say: %#x", name, code)
			}
		}
		// A search goes on from an entry to those below it, or above.
		audio := entry("IODeviceTree:/product/audio")
		if code, value := property(product, "model", ioRegistryRecursively, 4096); code != 0 || string(value) != "A1\x00" {
			t.Errorf("model below the product: %#x %q", code, value)
		}
		if code, value := property(entry("IODeviceTree:/product/camera"), "model", ioRegistryRecursively|ioRegistryParents, 4096); code != 0 || string(value) != "Test1,1\x00" {
			t.Errorf("model above the camera: %#x %q", code, value)
		}
		if code, _ := property(audio, "product-name", ioRegistryRecursively, 4096); code != ioReturnNotFound {
			t.Errorf("product-name below the audio entry: %#x", code)
		}
		all := ask(audio, 2888, with(le64(buf, 4096)))
		want := le32(ioSerializeMagic, ioLast|ioDictionaryType<<24|2, ioSymbolType<<24|5)
		want = append(append(want, "name\x00\x00\x00\x00"...), le32(ioSerializeData<<24|6)...)
		want = append(append(want, "audio\x00\x00\x00"...), le32(ioSymbolType<<24|6)...)
		want = append(append(want, "model\x00\x00\x00"...), le32(ioLast|ioSerializeData<<24|3)...)
		want = append(want, "A1\x00\x00"...)
		if got := p.read(buf, le.Uint64(all[28:])); !bytes.Equal(got, want) {
			t.Errorf("the audio entry's properties:\n% x, not\n% x", got, want)
		}

		// Iterators: the entries next to one, and walks of a plane.
		iterator := func(of, id uint32, plane string, options ...uint32) uint32 {
			return port(ask(of, id, with(text(plane), le32(options...))))
		}
		if got := names(iterator(product, 2813, dtPlane)); !same(got, "camera", "audio") {
			t.Errorf("the product's children: %v", got)
		}
		if got := names(iterator(product, 2814, dtPlane)); !same(got, "J1AP") {
			t.Errorf("the product's parents: %v", got)
		}
		if leaf, above := iterator(audio, 2813, dtPlane), iterator(root, 2814, dtPlane); leaf == 0 || above == 0 || names(leaf) != nil || names(above) != nil {
			t.Errorf("the children of a leaf are %#x and the parents of the root %#x: iterators with nothing in them", leaf, above)
		}
		if got := iterator(product, 2813, "Nonplane"); got != 0 {
			t.Errorf("children in a plane there is not: %#x", got)
		}
		if got := names(iterator(main, 2806, dtPlane, 0)); !same(got, "J1AP") {
			t.Errorf("the tree's plane from its root: %v", got)
		}
		walk := iterator(main, 2806, dtPlane, ioRegistryRecursively)
		if got := names(walk); !same(got, "J1AP", "chosen", "memory-map", "product", "camera", "audio", "options") {
			t.Errorf("a walk of the tree's plane: %v", got)
		}
		if valid := ask(walk, 2834, nil); le.Uint32(valid[12:]) != 1 {
			t.Errorf("the walk is valid: % x", valid)
		}
		ask(walk, 2803, nil)
		if got := names(walk); len(got) != 7 {
			t.Errorf("the walk again after a reset: %v", got)
		}
		if got := names(iterator(audio, 2833, dtPlane, ioRegistryRecursively|ioRegistryParents)); !same(got, "product", "J1AP", "Root") {
			t.Errorf("a walk up from the audio entry: %v", got)
		}
		if got := names(iterator(root, 2833, servicePlane, ioRegistryRecursively)); !same(got, "J1AP") {
			t.Errorf("a walk of the services: %v", got)
		}
		if bad := ask(main, 2806, with(text("Nonplane"), le32(0))); le.Uint32(bad[8:]) != ioReturnBadArgument {
			t.Errorf("an iterator over a plane there is not: % x", bad)
		}
		// Told to, an iterator goes into the entry it handed out last, and back out of it.
		steps := iterator(main, 2806, dtPlane, 0)
		port(ask(steps, 2802, nil))
		if code := ask(steps, 2807, nil); le.Uint32(code[8:]) != 0 {
			t.Errorf("into the device: % x", code)
		}
		if got := names(steps); !same(got, "chosen", "product", "options") {
			t.Errorf("the device's entries, once in it: %v", got)
		}
		if out, top := ask(steps, 2808, nil), ask(steps, 2808, nil); le.Uint32(out[8:]) != 0 || le.Uint32(top[8:]) != ioReturnNoDevice {
			t.Errorf("out of the device: % x, and out of the top: % x", out, top)
		}

		// The device is the one service the kernel makes with no driver: a search for its
		// class finds it, and it says what class it is.
		search := le32(ioSerializeMagic, ioLast|ioDictionaryType<<24|1, ioSymbolType<<24|16)
		search = append(append(search, "IOProviderClass\x00"...), le32(ioLast|ioStringType<<24|uint32(len(deviceClass)))...)
		search = xpcPadded(append(search, deviceClass...))
		if found := port(ask(main, 2880, with(le32(uint32(len(search))), search))); found != device {
			t.Errorf("a search for %s found %#x, and the device is %#x", deviceClass, found, device)
		}
		if class := ask(device, 2800, nil); string(class[20:20+len(deviceClass)]) != deviceClass {
			t.Errorf("the device's class: % x", class)
		}
		for class, want := range map[string]uint32{deviceClass: 1, "IOService": 1, "IOPlatformDevice": 0} {
			if conforms := ask(device, 2801, with(text(class))); le.Uint32(conforms[12:]) != want {
				t.Errorf("the device is a %s: % x", class, conforms)
			}
		}

		// To be told of services arriving is to be handed those there already: none.
		notification := append(le32(1), portDescriptor(answers, makeSend)...)
		notification = append(notification, with(text("IOServiceFirstMatch"), le32(8), le32(ioSerializeMagic, ioLast|ioDictionaryType<<24), le32(0))...)
		request := append(header(bitsComplex|copySend|makeSendOnce<<8, uint32(24+len(notification)), answers, 2884), notification...)
		le.PutUint32(request[8:], main)
		p.write(msg, request)
		p.trap(-31, msg, machSendMsg, uint64(len(request)))
		p.trap(-31, in_, machRcvMsg|machRcvTimeout, 0, 8192, uint64(answers), 0)
		if arrivals := p.u32(in_ + 28); p.u32(in_+20) != 2984 || arrivals == 0 || port(ask(arrivals, 2802, nil)) != 0 {
			t.Errorf("a notification of services arriving: id %d, iterator %#x", p.u32(in_+20), arrivals)
		}

		// What is not known of a device is not answered: the class of an entry, or what the
		// kernel shows of an entry it keeps to itself.
		request = header(copySend|makeSendOnce<<8, 24, answers, 2800)
		le.PutUint32(request[8:], product)
		p.write(msg, request)
		if x0, _, _ := p.trap(-31, msg, machSendMsg, 24); x0 == 0 {
			t.Errorf("the class of an entry was answered")
		}
		request = append(header(copySend|makeSendOnce<<8, 48, answers, 2888), with(le64(buf, 4096))...)
		le.PutUint32(request[8:], entry("IODeviceTree:/chosen/memory-map"))
		p.write(msg, request)
		if _, _, stopped := p.trap(-31, msg, machSendMsg, 48); stopped == "" {
			t.Errorf("the properties of an entry the kernel keeps to itself were answered")
		}
	})
}

func TestParseDeviceTree(t *testing.T) {
	flat := testDeviceTree.flatten()
	tree, err := parseDeviceTree(flat)
	if err != nil || tree.name != "device-tree" || len(tree.children) != 3 || len(tree.properties) != 3 {
		t.Fatalf("read %+v, %v", tree, err)
	}
	if chosen := tree.children[0]; chosen.parent != tree || !chosen.children[0].kernelOnly || len(chosen.properties) != 1 {
		t.Errorf("chosen: %+v", chosen)
	}
	for _, cut := range []int{4, 40, len(flat) - 3} {
		if tree, err := parseDeviceTree(flat[:cut]); err == nil {
			t.Errorf("a tree cut short at %d was read as %+v", cut, tree)
		}
	}
}
