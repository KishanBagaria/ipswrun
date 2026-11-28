// What becomes of a signal that waits, blocked, when what is done about it is changed: to be
// ignored, it is gone, and is not taken when a handler is set again; so too one whose usual
// end is to be ignored, set back to its usual end; one that would end the process waits on.
// This is how timeout(1) takes back the signal it sent its own group. It prints the same
// natively on a Mac as under ipswrun.
#include <pthread.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static volatile sig_atomic_t taken;
static void take(int sig) { taken++; }

static int waits(int sig) {
	sigset_t pending;
	sigpending(&pending);
	return sigismember(&pending, sig);
}

// tried blocks a signal with a handler set, sends it by how, changes what is done about it to
// between, sets the handler again and lets the signal in.
static void tried(const char *label, int sig, void (*between)(int), int to_thread) {
	sigset_t set, old;
	sigemptyset(&set);
	sigaddset(&set, sig);
	sigprocmask(SIG_BLOCK, &set, &old);
	signal(sig, take);
	taken = 0;
	if (to_thread)
		pthread_kill(pthread_self(), sig);
	else
		kill(getpid(), sig);
	int before = waits(sig);
	if (between)
		signal(sig, between);
	int after = waits(sig);
	signal(sig, take);
	int again = waits(sig);
	sigprocmask(SIG_SETMASK, &old, NULL);
	printf("%s: waits %d; after the change %d; with a handler again %d; taken when let in: %d\n", label, before, after, again, (int)taken);
	signal(sig, SIG_DFL);
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	tried("SIGUSR1 sent to the process, nothing changed", SIGUSR1, NULL, 0);
	tried("SIGUSR1 sent to the process, then ignored", SIGUSR1, SIG_IGN, 0);
	tried("SIGUSR1 sent to the thread, then ignored", SIGUSR1, SIG_IGN, 1);
	tried("SIGUSR1 sent to the process, then its usual end (to end the process)", SIGUSR1, SIG_DFL, 0);
	tried("SIGTERM sent to the process, then ignored", SIGTERM, SIG_IGN, 0);
	tried("SIGURG sent to the process, then its usual end (to be ignored)", SIGURG, SIG_DFL, 0);
	tried("SIGCHLD sent to the thread, then its usual end (to be ignored)", SIGCHLD, SIG_DFL, 1);
	tried("SIGCONT sent to the process, then ignored", SIGCONT, SIG_IGN, 0);
	tried("SIGCONT sent to the process, then its usual end", SIGCONT, SIG_DFL, 0);
	tried("SIGTSTP sent to the process, then ignored", SIGTSTP, SIG_IGN, 0);

	// One signal waits twice over, for the process and for the thread: both go.
	sigset_t set, old;
	sigemptyset(&set);
	sigaddset(&set, SIGUSR2);
	sigprocmask(SIG_BLOCK, &set, &old);
	signal(SIGUSR2, take);
	taken = 0;
	kill(getpid(), SIGUSR2);
	pthread_kill(pthread_self(), SIGUSR2);
	signal(SIGUSR2, SIG_IGN);
	signal(SIGUSR2, take);
	int left = waits(SIGUSR2);
	sigprocmask(SIG_SETMASK, &old, NULL);
	printf("SIGUSR2 sent to the process and to the thread, then ignored: waits %d; taken when let in: %d\n", left, (int)taken);
	return 0;
}
