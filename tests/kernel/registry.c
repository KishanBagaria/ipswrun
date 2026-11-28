// Reads the I/O Registry: the device's tree, as the kernel shows it in the plane IODeviceTree.
// ipswrun serves it from the restore image's device tree for the --device, so what this prints
// is what the image says of that device, and on that device itself it prints the same.
//
//   ipswrun --device Mac15,9 latest tests/kernel/registry.c
//
// It prints only what a restore image holds. What the boot loader fills in on a device (its
// serial number, what is in its NVRAM) is not in the image, and ipswrun leaves it out.
#include <CoreFoundation/CoreFoundation.h>
#include <mach/mach.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// IOKit.framework's, which the iOS SDK has no headers for.
typedef mach_port_t io_object_t;
typedef char io_name_t[128];
io_object_t IORegistryEntryFromPath(mach_port_t main, const char *path);
kern_return_t IORegistryEntryGetName(io_object_t entry, io_name_t name);
kern_return_t IORegistryEntryGetChildIterator(io_object_t entry, const char *plane, io_object_t *iterator);
io_object_t IOIteratorNext(io_object_t iterator);
CFTypeRef IORegistryEntryCreateCFProperty(io_object_t entry, CFStringRef key, CFAllocatorRef allocator, uint32_t options);
CFTypeRef IORegistryEntrySearchCFProperty(io_object_t entry, const char *plane, CFStringRef key, CFAllocatorRef allocator, uint32_t options);
kern_return_t IOObjectRelease(io_object_t object);
kern_return_t IOObjectGetClass(io_object_t object, io_name_t name);
CFMutableDictionaryRef IOServiceMatching(const char *name);
io_object_t IOServiceGetMatchingService(mach_port_t main, CFDictionaryRef matching);

// strings prints a property that is strings one after another, each ended by a zero.
static void strings(const char *what, CFTypeRef value) {
	printf("%s:", what);
	if (!value || CFGetTypeID(value) != CFDataGetTypeID()) {
		printf(" none\n");
		return;
	}
	const char *bytes = (const char *)CFDataGetBytePtr(value);
	size_t length = CFDataGetLength(value);
	for (size_t at = 0; at < length; at += strnlen(bytes + at, length - at) + 1)
		printf("%s %.*s", at ? "," : "", (int)(length - at), bytes + at);
	printf("\n");
}

static int by_name(const void *a, const void *b) { return strcmp(a, b); }

// children prints the names of the entries under one, sorted: the kernel keeps them in the
// order its drivers attached them, which no image says.
static void children(const char *what, io_object_t entry) {
	static io_name_t names[256];
	io_object_t iterator = 0, child;
	int count = 0;
	printf("%s:", what);
	if (IORegistryEntryGetChildIterator(entry, "IODeviceTree", &iterator) != KERN_SUCCESS || !iterator) {
		printf(" none\n");
		return;
	}
	while (count < 256 && (child = IOIteratorNext(iterator))) {
		IORegistryEntryGetName(child, names[count++]);
		IOObjectRelease(child);
	}
	qsort(names, count, sizeof names[0], by_name);
	for (int i = 0; i < count; i++)
		printf(" %s", names[i]);
	printf("\n");
}

int main(void) {
	io_object_t device = IORegistryEntryFromPath(MACH_PORT_NULL, "IODeviceTree:/");
	io_object_t product = IORegistryEntryFromPath(MACH_PORT_NULL, "IODeviceTree:/product");
	if (!device || !product) {
		printf("no device tree in the registry\n");
		return 1;
	}
	io_name_t name = "";
	IORegistryEntryGetName(device, name);
	printf("device: %s\n", name);
	strings("model", IORegistryEntryCreateCFProperty(device, CFSTR("model"), NULL, 0));
	strings("compatible", IORegistryEntryCreateCFProperty(device, CFSTR("compatible"), NULL, 0));
	strings("product-name", IORegistryEntryCreateCFProperty(product, CFSTR("product-name"), NULL, 0));
	// Recursively, towards the parents: the product has no model of its own, and the device has.
	strings("model, from the product up", IORegistryEntrySearchCFProperty(product, "IODeviceTree", CFSTR("model"), NULL, 3));
	strings("a property nothing has", IORegistryEntryCreateCFProperty(product, CFSTR("com.example.nonesuch"), NULL, 0));
	children("under the product", product);
	// The device is also a service, the one the kernel makes with no driver.
	io_object_t expert = IOServiceGetMatchingService(MACH_PORT_NULL, IOServiceMatching("IOPlatformExpertDevice"));
	io_name_t class = "";
	if (expert)
		IOObjectGetClass(expert, class);
	printf("the service of the class IOPlatformExpertDevice is the device: %d, of the class %s\n", expert == device, class);
	printf("the same entry twice is the same port: %d\n", IORegistryEntryFromPath(MACH_PORT_NULL, "IODeviceTree:/product") == product);
	printf("an entry that is not there: %d\n", IORegistryEntryFromPath(MACH_PORT_NULL, "IODeviceTree:/com.example.nonesuch") != 0);
	return 0;
}
