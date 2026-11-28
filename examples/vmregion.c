// mach_vm_region says of an address which stretch of memory it is in, or the next one after
// it, and how that stretch may be used: what it may do now and at most, whether a copy of the
// process shares it, and where in what it maps it starts. It prints the same natively on a Mac
// as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach/mach.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#define PAGE 16384

// The request is the kernel's own (routine 4816 of its memory requests), declared here since
// an iPhone's headers leave it out.
extern kern_return_t mach_vm_region(vm_map_read_t, mach_vm_address_t *, mach_vm_size_t *, vm_region_flavor_t, vm_region_info_t, mach_msg_type_number_t *, mach_port_t *);

static void of(const char *what, char *base, long at) {
	mach_vm_address_t address = (mach_vm_address_t)base + at;
	mach_vm_size_t size = 0;
	vm_region_basic_info_data_64_t info;
	memset(&info, 0x55, sizeof info);
	mach_msg_type_number_t count = VM_REGION_BASIC_INFO_COUNT_64;
	mach_port_t object = 77;
	kern_return_t r = mach_vm_region(mach_task_self(), &address, &size, VM_REGION_BASIC_INFO_64, (vm_region_info_t)&info, &count, &object);
	if (r) {
		printf("%s: %#x\n", what, r);
		return;
	}
	printf("%s: from page %lld for %llu pages; may %d, at most %d; inherited %u; shared %u; starts %llu pages into what it maps; behaviour %d, wired %u, reserved %u; %u words, object %u\n", what,
	       ((long long)address - (long long)(mach_vm_address_t)base) / PAGE, (unsigned long long)size / PAGE, info.protection, info.max_protection, info.inheritance, info.shared, (unsigned long long)info.offset / PAGE, info.behavior,
	       info.user_wired_count, info.reserved, count, object);
}

int main(void) {
	// Nine pages with nothing around them that could be taken for part of them: the first and
	// the last may do nothing, and the seven between are cut in stretches that differ.
	char *m = mmap(NULL, 9 * PAGE, PROT_NONE, MAP_ANON | MAP_PRIVATE, -1, 0);
	mprotect(m + PAGE, 3 * PAGE, PROT_READ | PROT_WRITE);
	mprotect(m + 4 * PAGE, 2 * PAGE, PROT_READ);
	mprotect(m + 6 * PAGE, PAGE, PROT_READ | PROT_WRITE);
	m[PAGE] = 1;
	of("the first page, which may do nothing", m, 0);
	of("three pages to read and write, asked at their first", m, PAGE);
	of("asked inside their second", m, 2 * PAGE + 100);
	of("two pages to read", m, 4 * PAGE);
	of("one to read and write after them", m, 6 * PAGE);
	munmap(m + 6 * PAGE, PAGE);
	of("that one unmapped, asked where it was", m, 6 * PAGE);
	of("their second alone, set apart", (mprotect(m + 2 * PAGE, PAGE, PROT_READ), m), 2 * PAGE);
	of("the one after", m, 3 * PAGE);
	madvise(m + 3 * PAGE, PAGE, MADV_SEQUENTIAL);
	of("the same, to be read in order", m, 3 * PAGE);

	// A shared mapping of no file, and a file mapped for oneself and to be shared from its
	// second page.
	char *s = mmap(NULL, 2 * PAGE, PROT_READ | PROT_WRITE, MAP_ANON | MAP_SHARED, -1, 0);
	of("two pages shared, of no file", s, 0);
	char path[512];
	snprintf(path, sizeof path, "%s/vmregion.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	int fd = open(path, O_CREAT | O_RDWR, 0600);
	ftruncate(fd, 4 * PAGE);
	char *own = mmap(NULL, 2 * PAGE, PROT_READ, MAP_PRIVATE, fd, PAGE), *both = mmap(NULL, 2 * PAGE, PROT_READ | PROT_WRITE, MAP_SHARED, fd, PAGE);
	of("a file mapped for oneself from its second page, to read", own, 0);
	of("the file mapped to be shared from its second page", both, PAGE);
	int reads = open(path, O_RDONLY);
	char *only = mmap(NULL, PAGE, PROT_READ, MAP_SHARED, reads, 0);
	of("the file opened to read only and mapped to be shared", only, 0);
	unlink(path);

	// Memory of no file that is shared is a copy of the process's too, and what is for oneself is not.
	char *ours = mmap(NULL, 3 * PAGE, PROT_READ | PROT_WRITE, MAP_ANON | MAP_SHARED, -1, 0), *mine = mmap(NULL, PAGE, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
	ours[0] = mine[0] = 'a';
	fflush(stdout);
	if (fork() == 0) {
		ours[0] = mine[0] = 'c';
		ours[2 * PAGE + 5] = 'd';
		if (fork() == 0) {
			ours[PAGE] = 'g';
			_exit(0);
		}
		wait(NULL);
		_exit(0);
	}
	wait(NULL);
	printf("shared memory of no file after a copy of the process and a copy of that wrote to it: %c %c %c; memory for oneself: %c", ours[0], ours[2 * PAGE + 5], ours[PAGE], mine[0]);
	munmap(ours + PAGE, PAGE);
	if (fork() == 0) {
		ours[0] = 'z';
		_exit(0);
	}
	wait(NULL);
	printf("; its middle unmapped and a copy having written: %c; of no length: %s\n", ours[0], mmap(NULL, 0, PROT_READ, MAP_ANON | MAP_SHARED, -1, 0) == MAP_FAILED ? strerror(errno) : "mapped");
	// What is refused.
	mach_vm_address_t address = (mach_vm_address_t)m + PAGE;
	mach_vm_size_t size;
	vm_region_basic_info_data_64_t info;
	mach_port_t object;
	mach_msg_type_number_t count = VM_REGION_BASIC_INFO_COUNT_64 - 1;
	kern_return_t r1 = mach_vm_region(mach_task_self(), &address, &size, VM_REGION_BASIC_INFO_64, (vm_region_info_t)&info, &count, &object);
	count = VM_REGION_BASIC_INFO_COUNT_64;
	kern_return_t r2 = mach_vm_region(mach_task_self(), &address, &size, 99, (vm_region_info_t)&info, &count, &object);
	count = VM_REGION_BASIC_INFO_COUNT_64;
	address = ~0ull - 5 * PAGE;
	kern_return_t r3 = mach_vm_region(mach_task_self(), &address, &size, VM_REGION_BASIC_INFO_64, (vm_region_info_t)&info, &count, &object);
	int words[16];
	count = 16;
	address = (mach_vm_address_t)m + PAGE;
	kern_return_t r5 = mach_vm_region(mach_task_self(), &address, &size, VM_REGION_BASIC_INFO_64, (vm_region_info_t)words, &count, &object);
	printf("with room for one word too few: %#x; of a kind there is not: %#x; past all memory: %#x; with room for more: %#x and %u words\n", r1, r2, r3, r5, count);
	return 0;
}
