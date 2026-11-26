// A Mach semaphore: it is made with a count, each wait takes one from it or waits for one,
// and each signal gives one back. It prints the same natively on a Mac as under ipswrun.
#include <mach/mach.h>
#include <mach/semaphore.h>
#include <pthread.h>
#include <stdio.h>
#include <unistd.h>

static semaphore_t gate;

static void *later(void *arg) {
	usleep(20000);
	semaphore_signal(gate);
	return NULL;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	semaphore_t two;
	mach_timespec_t none = {0, 0};
	printf("made with a count of 2: %#x\n", semaphore_create(mach_task_self(), &two, SYNC_POLICY_FIFO, 2));
	printf("waits: %#x, %#x", semaphore_wait(two), semaphore_timedwait(two, none));
	printf(", then with nothing left and no time to wait: %#x\n", semaphore_timedwait(two, none));
	printf("signalled: %#x; a wait: %#x; another: %#x\n", semaphore_signal(two), semaphore_timedwait(two, none), semaphore_timedwait(two, none));
	printf("made with none: %#x", semaphore_create(mach_task_self(), &gate, SYNC_POLICY_FIFO, 0));
	printf("; a wait with no time: %#x", semaphore_timedwait(gate, none));
	pthread_t thread;
	pthread_create(&thread, NULL, later, NULL);
	printf("; a wait that another thread ends: %#x\n", semaphore_wait(gate));
	pthread_join(thread, NULL);
	printf("made with a count below none: %#x\n", semaphore_create(mach_task_self(), &two, SYNC_POLICY_FIFO, -1));
	printf("destroyed: %#x; a wait on it: %#x\n", semaphore_destroy(mach_task_self(), gate), semaphore_timedwait(gate, none));
	return 0;
}
