package emulator

import (
	"slices"
	"strings"
)

// The kernel's own service for the system's power (IOPMrootDomain), which every Mac and iPhone
// has and which a daemon that must know before the machine sleeps registers with as it starts
// (diskarbitrationd gives up without it). It is here as measured of a user's program on macOS
// 27.2.1 (examples/power.c): found by its path in the plane of power, by where it is among the
// services and by its class; a service and an entry of the registry; openable; and taking the
// registration of a program that wants to be told of the system's power or of the service's own
// state, which it never is, as this machine does not sleep and the service does not change. Its properties are a machine's state (what woke it, whether its
// lid is shut) and are not given: asking for them, and any request not named here, stops the
// run. The platform's own entry it hangs from among the services (AppleARMPE) is not here, so
// a walk of that plane stops as before.
const (
	powerClass       = "IOPMrootDomain"
	powerPath        = "IOPower:/IOPowerConnection/IOPMrootDomain"
	powerServicePath = "IOService:/AppleARMPE/IOPMrootDomain"
	powerPlane       = "IOPower"
)

// powerClasses are what the power root is, from the kernel's source: a service, so an entry of
// the registry and an object.
var powerClasses = []string{powerClass, "IOService", "IORegistryEntry", "OSObject"}

// powerAbsent are properties a program asks the power root for that it does not have: measured
// as not there on macOS 27.2.1, and set nowhere in the kernel's source. The first is what a
// library asks before it says an object's class (another name for it); the rest are powerd's.
var powerAbsent = []string{"IOClassNameOverride", "IOPMBatteryPercentageFactors", "DarkWakeServices", "IOPlatformFeatureDefaults", "Power Status",
	// What names the last time the machine slept and woke, which one that has not slept does not
	// have (measured in a virtual machine, macOS 27.2; the source sets it at a sleep).
	"SleepWakeUUID",
	// The battery's state in the form it had long ago, which the portable measured does not
	// have either.
	"IOBatteryInfo",
	// The services that keep the machine from sleeping when idle and from sleeping at all, by
	// name and with their identifiers, which the kernel's source makes up when asked and gives
	// only if there is one: a driver says so of itself, and there is none here. From the
	// source, not measured (a Mac always has some).
	"IdleSleepPreventers", "SystemSleepPreventers", "IdleSleepPreventersWithID", "SystemSleepPreventersWithID"}

// powerEmpty are properties the kernel's source has the power root start with as dictionaries
// that hold nothing: the features its drivers then publish, of which a machine with no driver
// has none, and the platform's overrides of the power settings, which are none on the two Macs
// measured.
var powerEmpty = []string{"Supported Features", "SystemPowerProfileOverrideDict"}

type powerRoot struct{}

func (powerRoot) released() {}

// found is a reply that hands over a right to the power root, the same port each time.
func (powerRoot) found(p *Process) migReply {
	if p.m.ioPower == nil || p.m.ioPower.dead {
		p.m.ioPower = &port{m: p.m, object: powerRoot{}}
	}
	return migPort(p.sendRight(p.m.ioPower))
}

func (o powerRoot) request(p *Process, id uint32, body []byte) (migReply, bool) {
	r := ioReader{data: body, at: 32}
	switch id {
	case 2810: // io_registry_entry_get_name
		return migName(powerClass), true
	case 2800: // io_object_get_class
		return migName(powerClass), true
	case 2801: // io_object_conforms_to
		class := r.text()
		if r.failed {
			return migCode(migBadArguments), true
		}
		return migData(le32(uint32(truth(slices.Contains(powerClasses, class))))), true
	case 2826: // io_registry_entry_get_path: where it is in a plane
		switch plane := r.text(); {
		case r.failed:
			return migCode(migBadArguments), true
		case plane == servicePlane:
			return migName(powerServicePath), true
		case plane == powerPlane:
			return migName(powerPath), true
		}
	case 2829: // io_registry_entry_in_plane
		switch plane := r.text(); {
		case r.failed:
			return migCode(migBadArguments), true
		case plane == servicePlane, plane == powerPlane:
			return migData(le32(1)), true
		case plane == dtPlane:
			return migData(le32(0)), true
		}
	case 2889: // io_registry_entry_get_property_bin_buf
		_, name := r.text(), r.text()
		options := r.u32()
		buf, room := r.u64(), r.u64()
		if r.failed {
			return migCode(migBadArguments), true
		}
		if options&1 != 0 { // kIORegistryIterateRecursively: the entries above or below are asked too
			p.unimplemented("I/O Registry property "+name+" of the power root, looked for in other entries too", nil)
			return migReply{}, false
		}
		if slices.Contains(powerAbsent, name) {
			return migCode(ioReturnNotFound), true
		}
		if slices.Contains(powerEmpty, name) {
			return p.migSerialized(le32(ioSerializeMagic, ioLast|ioDictionaryType<<24), buf, room), true
		}
		if (name == "AppleClamshellState" || name == "AppleClamshellCausesSleep") && p.m.lidless() {
			// Whether the lid is shut, and whether shutting it sleeps the machine: the kernel's
			// source has the power root say so only once a lid's driver has told it of one, which
			// a Mac that is a box or a screen has none of. From the source, not measured.
			return migCode(ioReturnNotFound), true
		}
		p.unimplemented("I/O Registry property "+name+" of the power root, whose properties are a machine's state", nil)
	case 2862: // io_service_open_extended
		given := false // properties to open it with, which the kernel's source has it refuse
		at, ok := descriptors(body, func(at int, kind byte) { given = given || kind == 1 && le.Uint64(body[at:]) != 0 })
		open := ioReader{data: body, at: at + 8}
		kind := open.u32()
		if !ok || open.failed {
			return migCode(migBadArguments), true
		}
		if given {
			return migCode(ioReturnUnsupported), true
		}
		if kind != 0 && kind != 7 { // the two that were measured
			p.unimplemented("opening the power root with a type that was not measured", []uint64{uint64(kind)})
			return migReply{}, false
		}
		p.note("  IOKit open of the power root, type %d", kind)
		return migPortData(p.kernelRight(powerConnection{}), le32(0)), true
	case 2869: // io_service_add_interest_notification_64
		// A port to be told on comes first, as a descriptor; then the kind of interest, and what
		// the program wants handed back with each message. The answer is an object that stands
		// for the registration. Nothing is ever sent to the port: the system's power does not
		// change here.
		at, ok := descriptors(body, func(int, byte) {})
		interest := ioReader{data: body, at: at + 8}
		kind := interest.text()
		if !ok || interest.failed {
			return migCode(migBadArguments), true
		}
		// The kinds there are, as measured: of the system's power (which IORegisterForSystemPower
		// asks) and of it before others, of what the system can do, of the service's own state, of its
		// being busy, and of the console's being locked. None
		// of those changes here. Any other is one the kernel does not have.
		if !slices.Contains([]string{"IOAppPowerStateInterest", "IOPriorityPowerStateInterest", "IOGeneralInterest", "IOBusyInterest", "IOPMSystemCapabilityInterest", "IOConsoleSecurityInterest"}, kind) {
			return migCode(ioReturnUnsupported), true
		}
		p.note("  IOKit interest in the power root: %s", kind)
		return p.migObject(powerInterest{}), true
	}
	return migReply{}, false
}

// powerInterest stands for a program's registration to be told of the system's power.
type powerInterest struct{}

func (powerInterest) released() {}

func (powerInterest) request(*Process, uint32, []byte) (migReply, bool) { return migReply{}, false }

// powerConnection is an open connection to the power root. Closing it is all that is asked of
// one here: what a program does through it (sleeping the machine, saying it may sleep) stops.
type powerConnection struct{}

func (powerConnection) released() {}

func (powerConnection) request(p *Process, id uint32, body []byte) (migReply, bool) {
	if id == 2816 { // io_service_close
		return migCode(0), true
	}
	return migReply{}, false
}

// lidless says whether the device is a Mac of a kind that has no lid: a Mac mini, an iMac or a
// Mac Pro, by what its tree says its model is. Of any other it says no, without knowing: newer
// Macs' models (Mac14,2) do not say what they are.
func (m *Machine) lidless() bool {
	device := m.device()
	if device == nil {
		return false
	}
	model, _ := property(device.properties, "model")
	for _, kind := range []string{"Macmini", "iMac", "MacPro"} {
		if strings.HasPrefix(string(model), kind) {
			return true
		}
	}
	return false
}
