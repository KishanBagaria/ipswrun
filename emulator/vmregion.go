package emulator

import "slices"

// migVMRegion is mach_vm_region (4816): the region an address is in, or the next one after it
// (KERN_INVALID_ADDRESS where there is none), and of it what it may do and at most, how a copy
// of the process has it, how far into what it maps it starts, and how it was said it would be
// read. Of its kinds only the basic one in 64 bits (9) is answered; one there is not, and room
// for fewer than its nine words, are KERN_INVALID_ARGUMENT. As measured on macOS 27.2.1
// (tests/kernel/memory/vmregion.c):
//
//   - at most everything, but what was opened to read only and mapped to be shared: to read
//     and to run;
//   - a copy of the process gets its own (1), and shares what was mapped to be shared (0);
//   - a file's region starts where in the file it was mapped; memory of no file starts as far
//     into its mapping as it lies, until it is first touched, and from then where the region
//     touched began;
//   - the name of what it maps is no port, and it is never said to be shared or wired.
//
// A Mac's kernel makes one region of neighbours that are alike, and here each mapping is its
// own: a program that maps side by side finds more regions here.
func migVMRegion(p *Process, body []byte) migReply {
	addr, flavor, count := le.Uint64(body[32:])&tbiMask, le.Uint32(body[40:]), le.Uint32(body[44:])
	switch {
	case flavor == 10 || flavor == 11 || flavor == 12 || flavor == 13: // the other kinds there are, which were not measured
		return migReply{none: true}
	case flavor != 9 || count < 9:
		return migCode(kernInvalidArg)
	}
	var at *region
	for _, r := range p.regions {
		if r.end > addr && (at == nil || r.start < at.start) {
			at = r
		}
	}
	if at == nil {
		return migCode(kernInvalidAddress)
	}
	most, inherit := uint32(protAll), uint32(1)
	if at.readOnly {
		most = 5
	}
	if at.shared != nil {
		inherit = 0
	}
	into := at.into
	if at.shared != nil {
		into = at.off
	}
	info := slices.Concat(le32(at.prot(), most, inherit, 0, 0), le64(into), le32(at.behavior, 0))
	return migReply{
		body:    slices.Concat(le32(1), portDescriptor(0, moveSend), ndr, le64(at.start, at.end-at.start), le32(9), info[:36]),
		complex: true,
	}
}
