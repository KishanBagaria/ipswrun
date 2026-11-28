// The kernel's ring for memory to be given back later, which malloc keeps from macOS 26 on:
// stretches of memory entered in it are taken by the kernel when it is asked to flush, the
// kind to be unmapped by whole pages and the kind to be used again left where it is, and the
// kernel writes back only how far it has got. The ring is the one malloc has asked for, a
// process having no more than one. It prints the same natively on a Mac as under ipswrun.
#include <dlfcn.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

static uint64_t before[24];

// changed says which words of the ring's header are not what they were, and what they are
// where that is a small number.
static void changed(const char *what, uint64_t *header) {
	printf("%s:", what);
	for (int i = 0; i < 24; i++)
		if (header[i] != before[i]) {
			if (header[i] < 1 << 22)
				printf(" [%d]=%llu", i, (unsigned long long)header[i]);
			else if ((uint32_t)header[i] < 1 << 22 && header[i] >> 32 < 1 << 22)
				printf(" [%d]=%u,%u", i, (uint32_t)header[i], (uint32_t)(header[i] >> 32));
			else
				printf(" [%d]", i);
		}
	printf("\n");
	memcpy(before, header, sizeof before);
}

static int mapped(char *at) { return msync(at, 16384, MS_ASYNC) == 0; }

// ahead says of a time the kernel answered with whether it is ten seconds from now.
static const char *ahead(uint64_t answered, uint64_t now) {
	if (answered == 77)
		return "none";
	int64_t seconds = (int64_t)(answered - now) / 24000;
	return seconds > 9900 && seconds < 10100 ? "ten seconds from now" : "another time";
}

int main(void) {
	static char out[1 << 16];
	setvbuf(stdout, out, _IOFBF, sizeof out); // nothing is to ask malloc for memory between two looks at its ring
	kern_return_t (*query)(task_t, mach_vm_address_t *, mach_vm_size_t *) = dlsym(RTLD_DEFAULT, "mach_vm_deferred_reclamation_buffer_query");
	kern_return_t (*flush)(task_t, uint32_t, uint64_t *, uint64_t *) = dlsym(RTLD_DEFAULT, "mach_vm_deferred_reclamation_buffer_flush");
	kern_return_t (*resize)(task_t, uint32_t, uint64_t *, uint64_t *) = dlsym(RTLD_DEFAULT, "mach_vm_deferred_reclamation_buffer_resize");
	kern_return_t (*account)(task_t, uint64_t *, uint64_t *) = dlsym(RTLD_DEFAULT, "mach_vm_reclaim_update_kernel_accounting_trap");
	int (*allocate)(void **, uint32_t, uint32_t) = dlsym(RTLD_DEFAULT, "mach_vm_reclaim_ring_allocate");
	int (*capacity)(void *, uint32_t *) = dlsym(RTLD_DEFAULT, "mach_vm_reclaim_ring_capacity");
	int (*enter)(void *, mach_vm_address_t, mach_vm_size_t, uint8_t, uint64_t *, bool *) = dlsym(RTLD_DEFAULT, "mach_vm_reclaim_try_enter");
	int (*state)(void *, uint64_t, uint8_t, uint32_t *) = dlsym(RTLD_DEFAULT, "mach_vm_reclaim_query_state");
	mach_vm_address_t where = 0;
	mach_vm_size_t size = 0;
	if (!query || !flush || !resize || !account || !allocate || !capacity || !enter || !state || query(mach_task_self(), &where, &size) || !where) {
		printf("malloc has no ring here\n");
		return 0;
	}
	void *ring = (void *)where, *second = NULL;
	uint64_t *header = (uint64_t *)where;
	uint32_t entries = 0, is = 0;
	int r = capacity(ring, &entries);
	printf("malloc's ring: its mapping is whole pages %d, and room for more entries than it has %d; its length %#x, and header and entries fill whole pages: with 112 bytes of header %d, with 128 %d; a second ring: %#x\n", size % 16384 == 0,
	       size > 128 + 16ull * entries, r, (112 + 16 * entries) % 16384 == 0, (128 + 16 * entries) % 16384 == 0, allocate(&second, entries, entries));
	memcpy(before, header, sizeof before);

	// Six stretches of one to six pages, to be unmapped and to be used again by turns, the fifth
	// starting and ending inside its pages.
	char *stretch[6];
	uint64_t id[6], bytes = 77, when = 77, now;
	bool tell = false;
	for (int i = 0; i < 6; i++) {
		stretch[i] = mmap(NULL, (i + 1) * 16384, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0);
		stretch[i][0] = 'r';
		id[i] = UINT64_MAX;
	}
	for (int i = 0; i < 6; i++)
		enter(ring, (mach_vm_address_t)stretch[i] + (i == 4 ? 100 : 0), (i + 1) * 16384 - (i == 4 ? 300 : 0), i % 2 ? 1 : 2, &id[i], &tell);
	changed("six entered, and the header", header);

	now = mach_absolute_time();
	r = account(mach_task_self(), &bytes, &when);
	printf("the kernel told of it: %#x, took %llu bytes, will look again %s", r, (unsigned long long)bytes, ahead(when, now));
	uint64_t first = when;
	bytes = when = 77;
	r = account(mach_task_self(), &bytes, &when);
	printf("; told again at once: %#x, the same time %d\n", r, when == first);
	changed("  the header", header);
	printf("told by a name that is not the task's: %#x; with nowhere to answer: %#x\n", account(mach_host_self(), &bytes, &when), account(mach_task_self(), NULL, NULL));

	static const uint32_t counts[] = {0, 1, 2, 100, 3};
	for (unsigned i = 0; i < sizeof counts / sizeof *counts; i++) {
		bytes = when = 77;
		now = mach_absolute_time();
		r = flush(mach_task_self(), counts[i], &bytes, &when);
		printf("flush of %u: %#x, %llu bytes, and %s; still mapped:", counts[i], r, (unsigned long long)bytes, ahead(when, now));
		for (int n = 0; n < 6; n++)
			printf(" %d", mapped(stretch[n]));
		printf("; the fifth's last page %d\n", mapped(stretch[4] + 4 * 16384));
		changed("  the header", header);
	}
	printf("what was to be used again reads as it did: %d %d %d; each is said to be:", stretch[1][0] == 'r', stretch[3][0] == 'r', stretch[5][0] == 'r');
	for (int i = 0; i < 6; i++) {
		r = state(ring, id[i], i % 2 ? 1 : 2, &is);
		printf(" %u", r ? 0 : is);
	}
	printf("\n");

	static const uint32_t lengths[] = {0, 0, 4195320 + 2048};
	for (unsigned i = 0; i < sizeof lengths / sizeof *lengths; i++) {
		uint32_t length = i == 1 ? entries : lengths[i];
		bytes = when = 77;
		now = mach_absolute_time();
		r = resize(mach_task_self(), length, &bytes, &when);
		printf("a new length of %s: %#x, %llu bytes, and %s\n", i == 0 ? "none" : i == 1 ? "what it has" : "more than its mapping has room for", r, (unsigned long long)bytes, ahead(when, now));
	}
	changed("  the header", header);
	fflush(stdout);
	if (fork() == 0) {
		mach_vm_address_t its = 0;
		mach_vm_size_t large = 0;
		r = query(mach_task_self(), &its, &large);
		printf("in a copy of the process: %#x, its ring where the first's is %d and as large %d, with the header as it was %d\n", r, its == where, large == size, !memcmp(before, header, sizeof before));
		fflush(stdout);
		_exit(0);
	}
	wait(NULL);
	return 0;
}
