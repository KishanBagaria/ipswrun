// Memory copied from one place in a task to another by the kernel (mach_vm_copy), as a daemon
// that keeps large buffers copies them: what it copies, and what it is told of memory that is
// not there or may not be read or written. It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

// The calls of <mach/mach_vm.h>, which an iPhone's SDK leaves out and its library has.
typedef uint64_t mach_vm_address_t, mach_vm_size_t;
extern kern_return_t mach_vm_allocate(vm_map_t task, mach_vm_address_t *address, mach_vm_size_t size, int flags);
extern kern_return_t mach_vm_deallocate(vm_map_t task, mach_vm_address_t address, mach_vm_size_t size);
extern kern_return_t mach_vm_protect(vm_map_t task, mach_vm_address_t address, mach_vm_size_t size, boolean_t maximum, vm_prot_t protection);
extern kern_return_t mach_vm_copy(vm_map_t task, mach_vm_address_t from, mach_vm_size_t size, mach_vm_address_t to);

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_t task = mach_task_self();
	mach_vm_size_t page = (mach_vm_size_t)getpagesize();
	mach_vm_address_t from = 0, to = 0, gone = 0;
	mach_vm_allocate(task, &from, 3 * page, VM_FLAGS_ANYWHERE);
	mach_vm_allocate(task, &to, 3 * page, VM_FLAGS_ANYWHERE);
	mach_vm_allocate(task, &gone, page, VM_FLAGS_ANYWHERE);
	mach_vm_deallocate(task, gone, page);
	char *source = (char *)from, *dest = (char *)to;
	memset(source, 'a', 3 * page);
	memcpy(source + page - 3, "edge of a page", 14);
	memset(dest, 'z', 3 * page);

	kern_return_t whole = mach_vm_copy(task, from, 2 * page, to);
	printf("two pages copied: %d, as the first has them %d, and the third left as it was %d\n", whole, !memcmp(dest, source, 2 * page), dest[2 * page] == 'z');
	kern_return_t part = mach_vm_copy(task, from + page - 3, 14, to + 2 * page + 5);
	printf("14 bytes from across the edge of a page, to a place within one: %d, \"%.14s\", with what is beside them left %d\n", part, dest + 2 * page + 5, dest[2 * page + 4] == 'z' && dest[2 * page + 19] == 'z');
	memcpy(source, "0123456789", 10);
	kern_return_t over = mach_vm_copy(task, from, 8, from + 2);
	printf("8 bytes to two bytes on in the same place: %d, \"%.10s\"\n", over, source);
	printf("of no bytes: %d; of no bytes from memory that is not there: %d\n", mach_vm_copy(task, from, 0, to), mach_vm_copy(task, gone, 0, to));
	printf("from memory that is not there: %d; to memory that is not there: %d; a length that runs past what is there: %d\n", mach_vm_copy(task, gone, page, to), mach_vm_copy(task, from, page, gone),
	       mach_vm_copy(task, from, 16 * 1024 * page, to));
	mach_vm_protect(task, to, page, 0, VM_PROT_READ);
	mach_vm_protect(task, from, page, 0, VM_PROT_NONE);
	printf("to memory that may only be read: %d; from memory that may not be read: %d\n", mach_vm_copy(task, from + page, page, to), mach_vm_copy(task, from, page, to + page));
	printf("asked of the host: %d\n", mach_vm_copy(mach_host_self(), from + page, page, to + page));
	return 0;
}
