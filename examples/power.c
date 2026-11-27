// The kernel's own service for the system's power (IOPMrootDomain), which a daemon that must
// know before the machine sleeps registers with as it starts: that it is there, by its path in
// the plane of power and by its class, where it is among the services, that it can be opened,
// and that a program can register to be told of the system's power and take that back. No
// message is waited for: a machine is not put to sleep to see one. It prints the same natively
// on a Mac as under ipswrun.
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/IOMessage.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <stdio.h>

static void changed(void *ref, io_service_t service, natural_t type, void *argument) {
	(void)ref, (void)service, (void)argument;
	printf("told of the system's power: %#x\n", type);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	// No port names the default one to search from, on every release (its constant's name changed).
	io_registry_entry_t root = IORegistryEntryFromPath(MACH_PORT_NULL, "IOPower:/IOPowerConnection/IOPMrootDomain");
	printf("the power root by its path: found %d\n", root != 0);
	io_name_t name = "", class = "";
	char path[512] = "";
	kern_return_t named = IORegistryEntryGetName(root, name), classed = IOObjectGetClass(root, class);
	printf("its name: %#x \"%s\"; its class: %#x \"%s\"\n", named, name, classed, class);
	printf("it is a service %d, an entry of the registry %d, a connection %d\n", IOObjectConformsTo(root, "IOService"), IOObjectConformsTo(root, "IORegistryEntry"), IOObjectConformsTo(root, "IOUserClient"));
	kern_return_t pathed = IORegistryEntryGetPath(root, "IOService", path);
	printf("its path among the services: %#x \"%s\"\n", pathed, path);
	path[0] = 0;
	pathed = IORegistryEntryGetPath(root, "IOPower", path);
	printf("its path in the plane of power: %#x \"%s\"\n", pathed, path);
	printf("in the plane of services %d, of power %d, of the device tree %d\n", IORegistryEntryInPlane(root, "IOService"), IORegistryEntryInPlane(root, "IOPower"), IORegistryEntryInPlane(root, "IODeviceTree"));
	io_service_t byClass = IOServiceGetMatchingService(MACH_PORT_NULL, IOServiceMatching("IOPMrootDomain"));
	printf("found by its class: %d, and it is the same entry %d\n", byClass != 0, IOObjectIsEqualTo(byClass, root));

	io_connect_t connection = 0;
	kern_return_t opened = IOServiceOpen(root, mach_task_self(), 0, &connection);
	printf("opened: %#x, a connection %d\n", opened, connection != 0);
	IONotificationPortRef port = NULL;
	io_object_t notifier = 0;
	io_connect_t registered = IORegisterForSystemPower(NULL, &port, changed, &notifier);
	printf("registered for the system's power: a connection %d, a port %d, a notifier %d\n", registered != 0, port != NULL, notifier != 0);
	printf("that taken back: %#x\n", IODeregisterForSystemPower(&notifier));
	// Interest of the other kinds a daemon registers, and of a kind there is not.
	const char *kinds[] = {"IOGeneralInterest", "IOBusyInterest", "IOAppPowerStateInterest", "IOPriorityPowerStateInterest", "IOPMSystemCapabilityInterest", "IOConsoleSecurityInterest", "IONoSuchInterest"};
	IONotificationPortRef told = IONotificationPortCreate(MACH_PORT_NULL);
	for (unsigned i = 0; i < sizeof kinds / sizeof *kinds; i++) {
		io_object_t interest = 0;
		kern_return_t r = IOServiceAddInterestNotification(told, root, kinds[i], changed, NULL, &interest);
		printf("interest %s: %#x, an object %d", kinds[i], r, interest != 0);
		printf(", let go: %#x\n", interest ? IOObjectRelease(interest) : 0);
	}
	kern_return_t first = IOServiceClose(registered), second = IOServiceClose(connection);
	printf("closed: %#x %#x\n", first, second);
	return 0;
}
