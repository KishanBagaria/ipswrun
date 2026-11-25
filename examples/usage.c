// What a process has used, as getrusage reports it: time on the CPU, which work adds to and
// waiting does not, memory, and the counts a Mac's kernel keeps at zero. The numbers themselves
// are each machine's own, so this prints what holds of them on any: the same natively on a Mac
// as under ipswrun.
#include <errno.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/time.h>
#include <time.h>
#include <unistd.h>

static double seconds(struct timeval t) { return t.tv_sec + t.tv_usec / 1e6; }

static struct rusage now(void) {
	struct rusage r;
	memset(&r, 0xAA, sizeof r);
	if (getrusage(RUSAGE_SELF, &r))
		perror("getrusage");
	return r;
}

static double cpu(struct rusage r) { return seconds(r.ru_utime) + seconds(r.ru_stime); }

static void *spin(void *arg) {
	volatile unsigned long x = 0;
	for (unsigned long i = 0; i < 20000000UL; i++)
		x += i;
	return NULL;
}

int main(void) {
	struct timeval began, ended;
	gettimeofday(&began, NULL);
	struct rusage start = now();
	printf("at the start: time on the CPU already: %d; memory, in whole pages: %d; faults: %d\n", cpu(start) > 0, start.ru_maxrss > 0 && start.ru_maxrss % 4096 == 0,
	       start.ru_minflt + start.ru_majflt > 0);
	printf("what a Mac keeps at zero: %ld %ld %ld %ld %ld %ld %ld %ld\n", start.ru_ixrss, start.ru_idrss, start.ru_isrss, start.ru_nswap, start.ru_inblock, start.ru_oublock,
	       start.ru_msgsnd, start.ru_msgrcv);
	spin(NULL);
	struct rusage spun = now();
	printf("work adds to the time as the user: %d, and takes none from the system's: %d\n", seconds(spun.ru_utime) > seconds(start.ru_utime),
	       seconds(spun.ru_stime) >= seconds(start.ru_stime));
	usleep(300000);
	struct rusage slept = now();
	printf("waiting 0.3 seconds adds less than 0.1 of them: %d\n", cpu(slept) - cpu(spun) < 0.1);
	for (int i = 0; i < 20000; i++)
		getppid();
	struct rusage called = now();
	printf("system calls add to the system's time: %d\n", seconds(called.ru_stime) > seconds(slept.ru_stime));
	pthread_t thread;
	pthread_create(&thread, NULL, spin, NULL);
	pthread_join(thread, NULL);
	struct rusage threaded = now();
	printf("another thread's work is the process's: %d\n", seconds(threaded.ru_utime) > seconds(called.ru_utime));
	gettimeofday(&ended, NULL);
	double wall = seconds(ended) - seconds(began) + 1e-3;
	printf("nothing goes down: %d\n", seconds(threaded.ru_utime) >= seconds(start.ru_utime) && seconds(threaded.ru_stime) >= seconds(start.ru_stime) &&
	                                    threaded.ru_maxrss >= start.ru_maxrss && threaded.ru_minflt >= start.ru_minflt && threaded.ru_nvcsw >= start.ru_nvcsw);
	printf("the time used since the start is no more than has passed, for each thread: %d\n", cpu(threaded) - cpu(start) <= 2 * wall);
	printf("clock() counts the same time: %d\n", clock() > 0 && clock() / (double)CLOCKS_PER_SEC <= cpu(now()) + 1e-3);
	struct rusage children;
	memset(&children, 0xAA, sizeof children);
	int rc = getrusage(RUSAGE_CHILDREN, &children);
	static const struct rusage none;
	printf("of the children it has none of: %d, all zeros: %d\n", rc, memcmp(&children, &none, sizeof none) == 0);
	rc = getrusage(1, &children);
	printf("of anything else: %d (%s)\n", rc, strerror(errno));
	rc = getrusage(RUSAGE_SELF, (struct rusage *)1);
	printf("to nowhere: %d (%s)\n", rc, strerror(errno));
	return 0;
}
