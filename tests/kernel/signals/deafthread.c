// A thread that has told the kernel it takes no more signals (__disable_threadsignal, as every
// thread does as it ends): a signal for it alone is refused, and one for the process goes to
// another thread. And what the kernel says of whether the system's files may be changed
// (csrctl), which a program asks before it tries. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

extern int __disable_threadsignal(int);
static pthread_t taker;
static volatile sig_atomic_t taken, by_main, deaf, done;
static pthread_t first;

static void took(int sig) {
	(void)sig;
	taken++;
	by_main += pthread_equal(pthread_self(), first) != 0;
}

static void *thread(void *arg) {
	(void)arg;
	int r = __disable_threadsignal(1);
	deaf = 1 + (r != 0);
	while (!done)
		usleep(1000);
	return NULL;
}

#pragma clang diagnostic ignored "-Wdeprecated-declarations"
static const char *csr(uint32_t op, void *buf, size_t size) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	errno = 0;
	int r = syscall(483, op, buf, size);
	snprintf(t, 48, "%s", r ? strerror(errno) : "yes");
	return t;
}

int main(void) {
	first = pthread_self();
	struct sigaction act = {.sa_handler = took};
	sigaction(SIGUSR1, &act, NULL);
	pthread_create(&taker, NULL, thread, NULL);
	while (!deaf)
		usleep(1000);
	int to_it = pthread_kill(taker, SIGUSR1);
	usleep(20000);
	int none = taken;
	sigset_t mask, old;
	sigemptyset(&mask);
	sigaddset(&mask, SIGUSR1);
	pthread_sigmask(SIG_BLOCK, &mask, &old); // the first thread will not have it: only the other could
	kill(getpid(), SIGUSR1);
	usleep(20000);
	int while_blocked = taken;
	pthread_sigmask(SIG_SETMASK, &old, NULL);
	usleep(20000);
	printf("a thread that takes no more signals said so: %d; a signal for it alone: %s, taken %d times; one for the process while the first thread blocks it: taken %d times; unblocked: %d, by the first thread %d\n", deaf == 1,
	       to_it ? strerror(to_it) : "sent", none, while_blocked, (int)taken, (int)by_main);
	done = 1;
	pthread_join(taker, NULL);

	uint32_t config = 77, flag, nothing = 0;
	uint64_t wide = 0;
	const char *r1 = csr(1, &config, 4), *r2 = csr(1, &wide, 8), *r3 = csr(1, NULL, 4);
	printf("how the system is protected, asked: %s, and it is said to be wholly %d; into 8 bytes: %s; into nowhere: %s\n", r1, config == 0, r2, r3);
	printf("whether each thing the protection forbids is allowed:");
	for (int bit = 0; bit < 13; bit++) {
		flag = 1u << bit;
		printf(" %s%s", csr(0, &flag, 4), bit < 12 ? "," : "");
	}
	printf("; nothing at all: %s; asked with 8 bytes: %s; a question there is not: %s\n", csr(0, &nothing, 4), csr(0, &nothing, 8), csr(7, &nothing, 4));
	printf("a call made by its number: getpid the same %d\n", syscall(20) == getpid());
	return 0;
}
