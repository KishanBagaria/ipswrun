// What a task says of itself (task_info), as ps asks for its memory and time columns: the memory
// it has and has touched, the time its threads have used (those that still run in one record,
// those that have ended in another), and its priority. It prints the same natively on a Mac as
// under ipswrun.
#include <mach/mach.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <unistd.h>

static long micros(time_value_t t) { return t.seconds * 1000000L + t.microseconds; }

static long used(void) {
	struct rusage r;
	getrusage(RUSAGE_SELF, &r);
	return r.ru_utime.tv_sec * 1000000L + r.ru_utime.tv_usec + r.ru_stime.tv_sec * 1000000L + r.ru_stime.tv_usec;
}

static void *work(void *unused) {
	volatile unsigned long x = 0;
	long until = used() + 30000;
	while (used() < until)
		for (int i = 0; i < 100000; i++)
			x += i;
	return NULL;
}

static struct mach_task_basic_info basic(void) {
	struct mach_task_basic_info b;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
	memset(&b, 0, sizeof b);
	task_info(mach_task_self(), MACH_TASK_BASIC_INFO, (task_info_t)&b, &count);
	return b;
}

static long running(void) {
	struct task_thread_times_info t = {{0}};
	mach_msg_type_number_t count = TASK_THREAD_TIMES_INFO_COUNT;
	return task_info(mach_task_self(), TASK_THREAD_TIMES_INFO, (task_info_t)&t, &count) ? -1 : micros(t.user_time) + micros(t.system_time);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	struct mach_task_basic_info b;
	mach_msg_type_number_t count = MACH_TASK_BASIC_INFO_COUNT;
	struct rusage r, after;
	getrusage(RUSAGE_SELF, &r);
	kern_return_t answered = task_info(mach_task_self(), MACH_TASK_BASIC_INFO, (task_info_t)&b, &count);
	getrusage(RUSAGE_SELF, &after); // the most is asked on either side, for it may grow between
	printf("the basic record: %d, %d words", answered, count);
	printf("; memory mapped more than touched %d, touched some %d in whole pages %d, at the most no less %d and what getrusage says %d; a timesharing task %d, suspended %d\n",
	       b.virtual_size > b.resident_size, b.resident_size > 0, b.resident_size % (unsigned)getpagesize() == 0, b.resident_size_max >= b.resident_size, b.resident_size_max >= (unsigned long)r.ru_maxrss && b.resident_size_max <= (unsigned long)after.ru_maxrss,
	       b.policy == POLICY_TIMESHARE, b.suspend_count);
	long before = used(), live = running();
	printf("the time of threads that have ended, with none ended: %ld; of those that run: what getrusage said just before or more %d\n", micros(b.user_time) + micros(b.system_time), live >= before);
	work(NULL);
	long later = running();
	printf("after work on this thread: of those that run 30 ms more or so %d, of those ended still %ld\n", later - live >= 30000, micros(basic().user_time) + micros(basic().system_time));

	pthread_t second;
	pthread_create(&second, NULL, work, NULL);
	pthread_join(second, NULL);
	long ended = 0;
	for (int i = 0; i < 200 && ended < 30000; i++, usleep(10000)) // a Mac's kernel counts it in a moment after
		ended = micros(basic().user_time) + micros(basic().system_time);
	printf("after a second thread worked and ended: of those ended 30 ms or more %d; of those that run less than all the process used %d\n", ended >= 30000, running() < used());

	char *memory = malloc(32 << 20);
	memset(memory, 1, 32 << 20);
	struct mach_task_basic_info more = basic();
	printf("after touching 32 MB: that much more touched %d, and no less mapped %d\n", more.resident_size >= b.resident_size + (32 << 20), more.virtual_size >= b.virtual_size);

	struct task_basic_info_64 older;
	count = TASK_BASIC_INFO_64_COUNT;
	printf("the older record, as ps asks: %d, %d words, the same memory mapped %d\n", task_info(mach_task_self(), TASK_BASIC_INFO_64, (task_info_t)&older, &count), count,
	       older.virtual_size == basic().virtual_size);
	integer_t priority[8] = {0};
	count = 8;
	printf("its priority: %d, %d word, %d", task_info(mach_task_self(), TASK_SCHED_TIMESHARE_INFO, priority, &count), count, priority[0]);
	setpriority(PRIO_PROCESS, 0, 5);
	count = 8;
	task_info(mach_task_self(), TASK_SCHED_TIMESHARE_INFO, priority, &count);
	printf("; made nicer by 5: %d\n", priority[0]);
	count = MACH_TASK_BASIC_INFO_COUNT - 1;
	printf("with room for a word less than the basic record: %d", task_info(mach_task_self(), MACH_TASK_BASIC_INFO, (task_info_t)&b, &count));
	count = 64;
	integer_t room[64];
	printf("; a flavour there is not: %d\n", task_info(mach_task_self(), 99, room, &count));
	return 0;
}
