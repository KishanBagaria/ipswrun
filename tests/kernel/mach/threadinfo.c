// What the kernel says of a thread (thread_info): whether it runs or waits, the time it has
// used, how it is scheduled, and the name it gave itself. It prints the same natively on a Mac
// as under ipswrun.
#include <mach/mach.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static pthread_mutex_t lock = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t told = PTHREAD_COND_INITIALIZER;
static mach_port_t other;
static int done;

static void *waits(void *unused) {
	pthread_setname_np("one that waits");
	pthread_mutex_lock(&lock);
	other = mach_thread_self();
	pthread_cond_broadcast(&told);
	while (!done)
		pthread_cond_wait(&told, &lock);
	pthread_mutex_unlock(&lock);
	return NULL;
}

static long micros(time_value_t t) { return t.seconds * 1000000L + t.microseconds; }

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	mach_port_t self = mach_thread_self();
	struct thread_basic_info basic, later;
	mach_msg_type_number_t count = THREAD_BASIC_INFO_COUNT;
	kern_return_t r = thread_info(self, THREAD_BASIC_INFO, (thread_info_t)&basic, &count);
	printf("of itself, in short: %d, %u words; its state %d, flags %#x, suspended %d times, asleep for %d; a share of the processor from 0 to 1000 %d\n", r, count, basic.run_state, basic.flags,
	       basic.suspend_count, basic.sleep_time, basic.cpu_usage >= 0 && basic.cpu_usage <= 1000);
	volatile unsigned long spun = 0;
	for (unsigned long i = 0; i < 30000000; i++)
		spun += i;
	count = THREAD_BASIC_INFO_COUNT;
	thread_info(self, THREAD_BASIC_INFO, (thread_info_t)&later, &count);
	printf("after work: the time it has used as the user is more %d, and in the kernel no less %d\n", micros(later.user_time) > micros(basic.user_time), micros(later.system_time) >= micros(basic.system_time));

	pthread_setname_np("the first thread");
	struct thread_extended_info more;
	count = THREAD_EXTENDED_INFO_COUNT;
	r = thread_info(self, THREAD_EXTENDED_INFO, (thread_info_t)&more, &count);
	printf("at length: %d, %u words; its name \"%s\"; its state %d, flags %#x; its times in billionths, no less than the short ones in millionths %d; scheduled by sharing time %d, at priority %d, now %d, of at most %d\n", r, count,
	       more.pth_name, more.pth_run_state, more.pth_flags, more.pth_user_time / 1000 >= (uint64_t)micros(later.user_time) && more.pth_user_time % 1000 == 0, more.pth_policy == 1, more.pth_priority, more.pth_curpri,
	       more.pth_maxpriority);

	// Another thread, which waits.
	pthread_t thread;
	pthread_mutex_lock(&lock);
	pthread_create(&thread, NULL, waits, NULL);
	while (!other)
		pthread_cond_wait(&told, &lock);
	pthread_mutex_unlock(&lock);
	usleep(100000); // it waits again by now
	count = THREAD_BASIC_INFO_COUNT;
	r = thread_info(other, THREAD_BASIC_INFO, (thread_info_t)&basic, &count);
	printf("of one that waits, in short: %d; its state %d, flags %#x, suspended %d times", r, basic.run_state, basic.flags, basic.suspend_count);
	count = THREAD_EXTENDED_INFO_COUNT;
	r = thread_info(other, THREAD_EXTENDED_INFO, (thread_info_t)&more, &count);
	printf("; at length: %d, its name \"%s\", its state %d\n", r, more.pth_name, more.pth_run_state);
	pthread_mutex_lock(&lock);
	done = 1;
	pthread_cond_broadcast(&told);
	pthread_mutex_unlock(&lock);
	pthread_join(thread, NULL);

	// What is refused.
	mach_msg_type_number_t small = THREAD_BASIC_INFO_COUNT - 1, less = THREAD_EXTENDED_INFO_COUNT - 1, room = 64;
	integer_t words[64];
	printf("into room for one word less: in short %d, at length %d; a kind there is not: %d; asked of the task: %d\n", thread_info(self, THREAD_BASIC_INFO, (thread_info_t)&basic, &small),
	       thread_info(self, THREAD_EXTENDED_INFO, (thread_info_t)&more, &less), thread_info(self, 99, words, &room), thread_info(mach_task_self(), THREAD_BASIC_INFO, (thread_info_t)&basic, &count));
	return 0;
}
