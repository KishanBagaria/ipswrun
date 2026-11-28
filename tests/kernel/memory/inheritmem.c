// What a copy of the process has of a stretch of memory, as the process says (minherit): a copy
// of it, which is how memory is unless said otherwise; the very memory, so that what either
// writes the other reads; or nothing at all. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <mach/mach.h>
#include <setjmp.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/wait.h>
#include <unistd.h>

#define PAGE 16384

extern kern_return_t mach_vm_region(vm_map_read_t, mach_vm_address_t *, mach_vm_size_t *, vm_region_flavor_t, vm_region_info_t, mach_msg_type_number_t *, mach_port_t *);

static sigjmp_buf back;
static void fault(int sig) { siglongjmp(back, sig); }

// reads says what a byte of memory holds, or which signal reading it raises.
static const char *reads(char *at) {
	static char text[4][24];
	static int next;
	char *t = text[next++ % 4];
	int sig = sigsetjmp(back, 1);
	if (sig == 0)
		snprintf(t, 24, "%c", *at);
	else
		snprintf(t, 24, "signal %d", sig);
	return t;
}

static int inherited(char *at) {
	mach_vm_address_t address = (mach_vm_address_t)at;
	mach_vm_size_t size = 0;
	vm_region_basic_info_data_64_t info;
	mach_msg_type_number_t count = VM_REGION_BASIC_INFO_COUNT_64;
	mach_port_t object;
	return mach_vm_region(mach_task_self(), &address, &size, VM_REGION_BASIC_INFO_64, (vm_region_info_t)&info, &count, &object) ? -1 : (int)info.inheritance;
}

static const char *said(int r) { return r ? strerror(errno) : "done"; }

int main(void) {
	signal(SIGSEGV, fault);
	signal(SIGBUS, fault);
	// Five pages apart from anything else: the first and the last as memory is, the second to be
	// shared, the third not to be had, the fourth said to be copied.
	char *m = mmap(NULL, 7 * PAGE, PROT_NONE, MAP_ANON | MAP_PRIVATE, -1, 0) + PAGE;
	mprotect(m, 5 * PAGE, PROT_READ | PROT_WRITE);
	memset(m, 'p', 5 * PAGE);
	const char *r1 = said(minherit(m + PAGE, PAGE, VM_INHERIT_SHARE)), *r2 = said(minherit(m + 2 * PAGE, PAGE, VM_INHERIT_NONE)), *r3 = said(minherit(m + 3 * PAGE, PAGE, VM_INHERIT_COPY));
	printf("a page to be shared with a copy of the process: %s; one it is not to have: %s; one it is to have a copy of: %s; they are said to be inherited as %d %d %d %d %d, and what they hold is kept: %s %s %s\n", r1, r2, r3, inherited(m),
	       inherited(m + PAGE), inherited(m + 2 * PAGE), inherited(m + 3 * PAGE), inherited(m + 4 * PAGE), reads(m + PAGE), reads(m + 2 * PAGE), reads(m + 3 * PAGE));
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		printf("the copy reads them: %s, %s, %s, %s; they are inherited there as %d %d %d", reads(m), reads(m + PAGE), reads(m + 2 * PAGE), reads(m + 3 * PAGE), inherited(m), inherited(m + PAGE), inherited(m + 3 * PAGE));
		m[0] = m[PAGE] = m[3 * PAGE] = 'c';
		fflush(stdout);
		if (fork() == 0) { // a copy of the copy shares what the copy shares
			m[PAGE + 1] = 'g';
			_exit(0);
		}
		wait(NULL);
		printf("; after a copy of its own wrote to the shared page it reads %s\n", reads(m + PAGE + 1));
		fflush(stdout);
		_exit(0);
	}
	wait(NULL);
	printf("after the copy wrote to all it had, the first process reads: %s, %s%s, %s, %s\n", reads(m), reads(m + PAGE), reads(m + PAGE + 1), reads(m + 2 * PAGE), reads(m + 3 * PAGE));
	m[PAGE + 2] = 'P';

	// Said again, and what is refused.
	r1 = said(minherit(m + PAGE, PAGE, VM_INHERIT_COPY));
	fflush(stdout);
	if (fork() == 0) {
		m[PAGE] = 'x';
		_exit(0);
	}
	wait(NULL);
	printf("the shared page said to be copied after all: %s, inherited as %d; a copy writes to it and the first process reads %s\n", r1, inherited(m + PAGE), reads(m + PAGE));
	r1 = said(minherit(m + 100, 10, VM_INHERIT_SHARE));
	int whole = inherited(m), next = inherited(m + PAGE);
	r2 = said(minherit(m, PAGE, 3));
	r3 = said(minherit(m, PAGE, 99));
	const char *r4 = said(minherit(m, 0, VM_INHERIT_SHARE)), *r5 = said(minherit(m + 6 * PAGE, PAGE, VM_INHERIT_SHARE));
	munmap(m + 4 * PAGE, PAGE);
	printf("ten bytes inside a page: %s, and the page is inherited as %d, the next as %d; a way there is not, 3: %s, 99: %s; of no length: %s; of a page that may do nothing: %s; of one that is not there: %s\n", r1, whole, next, r2, r3, r4, r5,
	       said(minherit(m + 4 * PAGE, PAGE, VM_INHERIT_SHARE)));
	return 0;
}
