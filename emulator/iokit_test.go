package emulator

import (
	"bytes"
	"testing"
)

// testDriver doubles the numbers it is given, reverses the bytes, and reads a byte of its
// client's memory when asked to by method 2.
type testDriver struct {
	opened, closed int
	client         Client
}

func (d *testDriver) Class() string { return "com_example_driver_Test" }

func (d *testDriver) Open(client Client, kind uint32) (UserClient, uint32) {
	if kind != 7 {
		return nil, ioReturnUnsupported
	}
	d.opened++
	d.client = client
	return d, 0
}

func (d *testDriver) Close() { d.closed++ }

func (d *testDriver) Call(call Call) Result {
	switch call.Selector {
	case 1:
		var out Result
		for _, n := range call.Scalars {
			out.Scalars = append(out.Scalars, 2*n)
		}
		for i := len(call.Input) - 1; i >= 0; i-- {
			out.Output = append(out.Output, call.Input[i])
		}
		return out
	case 2:
		data, err := d.client.Read(call.Scalars[0], 1)
		if err != nil {
			return Result{Code: ioReturnBadArgument}
		}
		return Result{Scalars: []uint64{uint64(data[0])}}
	}
	return Result{Code: ioReturnUnsupported}
}

// A process finds a driver by its class, opens it and calls its methods, in the messages
// IOKit.framework sends.
func TestIOKit(t *testing.T) {
	p := newProcess(t)
	driver := &testDriver{}
	p.m.cfg.Drivers = []Driver{driver}
	onMain(func() {
		const out, msg, in_, far = dataAddr + 0x40, dataAddr + 0x100, dataAddr + 0x2000, dataAddr + 0x8000
		p.trap(-16, 0x203, rightReceive, out)
		answers := p.u32(out)
		// ask sends a request to a port of the kernel's and returns the answer without its header.
		ask := func(to, id uint32, complex uint32, body []byte) []byte {
			request := append(header(complex|copySend|makeSendOnce<<8, uint32(24+len(body)), answers, id), body...)
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
		// search is a description of services of a class, as IOServiceMatching makes it.
		search := func(class string) []byte {
			d := le32(ioSerializeMagic, ioLast|ioDictionaryType<<24|1, ioSymbolType<<24|16)
			d = append(append(d, "IOProviderClass\x00"...), le32(ioLast|ioStringType<<24|uint32(len(class)))...)
			d = xpcPadded(append(d, class...))
			return append(append(bytes.Clone(ndr), le32(uint32(len(d)))...), d...)
		}
		main := le.Uint32(ask(uint32(machHostSelf(p, nil)), 205, 0, nil)[4:])
		if version := ask(main, 2877, 0, nil); le.Uint64(version[12:]) != ioServerVersion {
			t.Errorf("io_server_version: % x", version)
		}
		// A class no driver has: an iterator with nothing in it, and no single service.
		none := le.Uint32(ask(main, 2881, 0, search("com_example_driver_Other"))[4:])
		if next := ask(none, 2802, 0, nil); le.Uint32(next[4:]) != 0 {
			t.Errorf("next of an empty search: % x", next)
		}
		if one := ask(main, 2880, 0, search("com_example_driver_Other")); le.Uint32(one[8:]) != ioReturnNotFound {
			t.Errorf("the one service of an empty search: % x", one)
		}
		found := le.Uint32(ask(main, 2881, 0, search(driver.Class()))[4:])
		service := le.Uint32(ask(found, 2802, 0, nil)[4:])
		if next := ask(found, 2802, 0, nil); service == 0 || le.Uint32(next[4:]) != 0 {
			t.Errorf("the search found %#x, and then % x", service, next)
		}
		// open asks for a connection of a type: the task and no properties, as descriptors, first.
		open := func(kind uint32) (connection, code uint32) {
			body := append(le32(2), portDescriptor(0x203, copySend)...)
			body = append(append(body, make([]byte, 11)...), 1, 0, 0, 0, 0)
			body = append(append(append(body, ndr...), le32(kind)...), append(bytes.Clone(ndr), le32(0)...)...)
			answer := ask(service, 2862, bitsComplex, body)
			return le.Uint32(answer[4:]), le.Uint32(answer[24:])
		}
		if connection, code := open(1); connection != 0 || code != ioReturnUnsupported || driver.opened != 0 {
			t.Errorf("open of a type the driver refuses: %#x, %#x", connection, code)
		}
		connection, code := open(7)
		if connection == 0 || code != 0 || driver.opened != 1 || driver.client.PID != p.pid {
			t.Fatalf("open: %#x, %#x; the driver was opened %d times by pid %d", connection, code, driver.opened, driver.client.PID)
		}
		// method calls one: numbers and bytes in the message, or bytes left in memory at inputAt,
		// and room for what comes out in the message or at outputAt.
		method := func(selector uint32, scalars []uint64, input []byte, inputAt, outputAt, outputRoom uint64) []byte {
			body := append(bytes.Clone(ndr), le32(selector, uint32(len(scalars)))...)
			body = append(append(body, le64(scalars...)...), le32(uint32(len(input)))...)
			body = append(append(body, xpcPadded(bytes.Clone(input))...), le64(inputAt, 0)...)
			if inputAt != 0 {
				le.PutUint64(body[len(body)-8:], 5000)
			}
			body = append(append(body, le32(ioInbandMax, ioScalarMax)...), le64(outputAt, outputRoom)...)
			return ask(connection, 2865, 0, body)[8:] // after the NDR record
		}
		got := method(1, []uint64{3, 40}, []byte("abcde"), 0, 0, 0)
		if le.Uint32(got) != 0 || le.Uint32(got[4:]) != 5 || string(got[8:13]) != "edcba" || le.Uint32(got[16:]) != 2 || le.Uint64(got[20:]) != 6 || le.Uint64(got[28:]) != 80 {
			t.Errorf("method 1: % x", got)
		}
		// Too much to travel in a message goes in and comes out through the caller's memory.
		large := bytes.Repeat([]byte("0123456789"), 500)
		p.write(far, large)
		got = method(1, nil, nil, far, far+0x4000, 8192)
		back := p.read(far+0x4000, 5000)
		if le.Uint32(got) != 0 || le.Uint32(got[4:]) != 0 || le.Uint64(got[12:]) != 5000 || back[0] != '9' || back[4999] != '0' {
			t.Errorf("method 1 through memory: % x, %q…%q", got, back[:3], back[4997:])
		}
		// A driver reads its client's memory, and its refusals arrive as they are.
		if got = method(2, []uint64{far + 3}, nil, 0, 0, 0); le.Uint32(got) != 0 || le.Uint64(got[12:]) != '3' {
			t.Errorf("method 2: % x", got)
		}
		if got = method(2, []uint64{nowhere}, nil, 0, 0, 0); le.Uint32(got) != ioReturnBadArgument {
			t.Errorf("method 2 of memory the client does not have: % x", got)
		}
		if got = method(9, nil, nil, 0, 0, 0); le.Uint32(got) != ioReturnUnsupported {
			t.Errorf("a method the driver does not have: % x", got)
		}
		// The connection is closed once, by the last right to it going.
		p.trap(-18, 0x203, uint64(connection))
		if driver.closed != 1 {
			t.Errorf("after the connection's right went, the driver was closed %d times", driver.closed)
		}
		if x0, _, _ := p.trap(-31, msg, machSendMsg, 24); x0 == 0 {
			t.Errorf("a request to a connection that is gone was taken")
		}
	})
}

func TestIODictionary(t *testing.T) {
	d := le32(ioSerializeMagic, ioLast|ioDictionaryType<<24|3, ioSymbolType<<24|2)
	d = append(append(d, 'a', 0, 0, 0), le32(ioStringType<<24|3)...)
	d = append(append(d, 'x', 'y', 'z', 0), le32(ioSymbolType<<24|2)...)
	d = append(append(d, 'b', 0, 0, 0), le32(ioNumberType<<24|64)...)
	d = append(append(d, le64(77)...), le32(ioSymbolType<<24|2)...)
	d = append(append(d, 'c', 0, 0, 0), le32(ioLast|ioBooleanType<<24|1)...)
	got, ok := ioDictionary(d)
	if !ok || len(got) != 3 || got["a"] != "xyz" || got["b"] != uint64(77) || got["c"] != true {
		t.Errorf("read %v %v", got, ok)
	}
	for name, bad := range map[string][]byte{"cut short": d[:len(d)-6], "no magic": d[4:], "an array": le32(ioSerializeMagic, ioLast|0x02<<24)} {
		if got, ok := ioDictionary(bad); ok {
			t.Errorf("%s was read as %v", name, got)
		}
	}
}
