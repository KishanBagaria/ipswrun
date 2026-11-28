// A process stopped and set going again: by SIGSTOP, which nothing can catch, and by the stop a
// terminal's key asks for (SIGTSTP), which can be caught or ignored and which a group with
// nobody to see to it is not sent at all. What the parent is told of it (wait4 with WUNTRACED
// and WCONTINUED, and SIGCHLD), what a signal sent to a stopped process does, and that a call
// it was in the middle of goes on afterwards. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

static void say(const char *format, ...) {
	char line[600];
	va_list args;
	va_start(args, format);
	int n = vsnprintf(line, sizeof line, format, args);
	va_end(args);
	write(1, line, (size_t)n);
}

// The signals that came to a child, counted, and what the parent was last told of a child.
static volatile sig_atomic_t came[32];
static void count(int sig) { came[sig]++; }
static volatile sig_atomic_t told, told_code, told_status;
static void child_changed(int sig, siginfo_t *info, void *context) {
	(void)sig, (void)context;
	told++;
	told_code = info->si_code;
	told_status = info->si_status;
}

// waited says what wait4 answers of a child, asked the given way.
static const char *waited(pid_t child, int how) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	int status = 0;
	pid_t got;
	while ((got = waitpid(child, &status, how)) < 0 && errno == EINTR)
		;
	if (got < 0)
		snprintf(t, 96, "-1 (%s)", strerror(errno));
	else if (got == 0)
		snprintf(t, 96, "nothing");
	else if (WIFSTOPPED(status))
		snprintf(t, 96, "stopped by %d (%#x)", WSTOPSIG(status), status);
	else if (WIFCONTINUED(status))
		snprintf(t, 96, "going again (%#x)", status);
	else if (WIFSIGNALED(status))
		snprintf(t, 96, "ended by %d", WTERMSIG(status));
	else
		snprintf(t, 96, "ended with %d", WEXITSTATUS(status));
	return t;
}

// later is waited without waiting, after a moment for what was sent to have its effect.
static const char *later(pid_t child, int how) {
	usleep(60000);
	return waited(child, how | WNOHANG);
}

// news says what SIGCHLD told the parent since it was last asked.
static const char *news(void) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	usleep(20000);
	if (told == 0)
		snprintf(t, 64, "no SIGCHLD");
	else
		snprintf(t, 64, "SIGCHLD %d, code %d, status %d", (int)told, (int)told_code, (int)told_status);
	told = 0;
	return t;
}

// A child that counts what comes to it, and says so when asked down a pipe: 'r' to say, 'x' to
// end. How it takes SIGTSTP and SIGCONT is the caller's to say.
static int ask[2], answer[2];
static pid_t worker(void (*tstp)(int), void (*cont)(int), int alone) {
	pipe(ask);
	pipe(answer);
	pid_t child = fork();
	if (child != 0) {
		close(ask[0]);
		close(answer[1]);
		return child;
	}
	close(ask[1]);
	close(answer[0]);
	if (alone)
		setsid(); // a session, and so a group, with nobody of it to see to a stop
	else
		setpgid(0, 0); // a group of its own, the parent in another of the session
	struct sigaction act;
	memset(&act, 0, sizeof act);
	act.sa_flags = SA_RESTART;
	act.sa_handler = count;
	sigaction(SIGUSR1, &act, NULL);
	act.sa_handler = tstp;
	sigaction(SIGTSTP, &act, NULL);
	act.sa_handler = cont;
	sigaction(SIGCONT, &act, NULL);
	write(answer[1], "ready\n", 6);
	char c;
	ssize_t n;
	while ((n = read(ask[0], &c, 1)) == 1 && c != 'x') {
		char line[96];
		int len = snprintf(line, sizeof line, "SIGUSR1 %d, SIGTSTP %d, SIGCONT %d\n", (int)came[SIGUSR1], (int)came[SIGTSTP], (int)came[SIGCONT]);
		write(answer[1], line, (size_t)len);
	}
	_exit(n == 1 ? 0 : 3);
}

// heard asks the child what came to it; it answers only while it is going.
static const char *heard(void) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	write(ask[1], "r", 1);
	size_t at = 0;
	while (at < 95 && read(answer[0], t + at, 1) == 1 && t[at] != '\n')
		at++;
	t[at] = 0;
	return t;
}

static void ready(void) {
	char line[8];
	read(answer[0], line, 6);
}

static void done(pid_t child) {
	write(ask[1], "x", 1);
	say("asked to end: %s\n", waited(child, 0));
	close(ask[1]);
	close(answer[0]);
	news();
}

// The order handlers ran in, for the cases below.
static char order[64];
static volatile int ran;
static void note(int sig) {
	came[sig]++;
	if (ran < 60)
		order[ran++] = sig == SIGUSR1 ? '1' : sig == SIGUSR2 ? '2' : sig == SIGCONT ? 'c' : sig == SIGTSTP ? 't' : sig == SIGHUP ? 'h' : '?';
}
#define count note
#define at ran

static void handle(int sig, int flags) {
	struct sigaction act;
	memset(&act, 0, sizeof act);
	act.sa_handler = note;
	act.sa_flags = flags;
	sigaction(sig, &act, NULL);
}

static const char *told_of(pid_t child, int how) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	int status = 0;
	pid_t got;
	while ((got = waitpid(child, &status, how)) < 0 && errno == EINTR)
		;
	if (got < 0)
		snprintf(t, 96, "-1 (%s)", strerror(errno));
	else if (got == 0)
		snprintf(t, 96, "nothing");
	else if (WIFSTOPPED(status))
		snprintf(t, 96, "stopped by %d", WSTOPSIG(status));
	else if (WIFCONTINUED(status))
		snprintf(t, 96, "going again");
	else if (WIFSIGNALED(status))
		snprintf(t, 96, "ended by %d", WTERMSIG(status));
	else
		snprintf(t, 96, "ended with %d", WEXITSTATUS(status));
	return t;
}

// rest is the rest of it, each case in children of its own: a stop that waited blocked, SIGCONT
// to one that blocks it, what a stopped process is sent and a call it waits in, what waits of
// SIGCONT when a stop is sent, the order handlers run in, which children wait4 means and which
// of several it tells of first, a stopped group left with nobody, and what the list of
// processes says of one stopped.
static void rest(void) {
	sigset_t set;
	pid_t c;
	int p[2];
	// 1
	if ((c = fork()) == 0) { setpgid(0, 0); sigemptyset(&set); sigaddset(&set, SIGTSTP); handle(SIGTSTP, 0); sigprocmask(SIG_BLOCK, &set, 0); raise(SIGTSTP); signal(SIGTSTP, SIG_DFL); sigprocmask(SIG_UNBLOCK, &set, 0); _exit(7); }
	say("a blocked SIGTSTP let through with nothing catching it: %s", told_of(c, WUNTRACED)); kill(c, SIGCONT); say("; then %s\n", told_of(c, 0));
	// 2
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); sigemptyset(&set); sigaddset(&set, SIGCONT); handle(SIGCONT, 0); sigprocmask(SIG_BLOCK, &set, 0); write(p[1], "g", 1); usleep(300000); sigset_t pend; sigpending(&pend); say("in the child SIGCONT waits %d, came %d\n", sigismember(&pend, SIGCONT), (int)came[SIGCONT]); _exit(0); }
	char g; read(p[0], &g, 1); kill(c, SIGCONT); usleep(100000); say("SIGCONT sent to one going that blocks it: with WCONTINUED %s\n", told_of(c, WCONTINUED | WNOHANG)); told_of(c, 0);
	// 3
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); handle(SIGUSR1, 0); write(p[1], "g", 1); struct timespec t = {1, 0}, rem = {0, 0}; int r = nanosleep(&t, &rem); say("a sleep of a second, stopped and sent a caught signal at once, set going after 1.5: %d%s, SIGUSR1 came %d\n", r, r ? " (interrupted)" : "", (int)came[SIGUSR1]); _exit(0); }
	read(p[0], &g, 1); usleep(100000); kill(c, SIGSTOP); told_of(c, WUNTRACED); kill(c, SIGUSR1); usleep(1500000); kill(c, SIGCONT); told_of(c, 0);
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); handle(SIGUSR1, 0); write(p[1], "g", 1); struct timespec t = {1, 0}, rem = {0, 0}; int r = nanosleep(&t, &rem); say("the same, set going after 0.3: %d%s\n", r, r ? " (interrupted)" : ""); _exit(0); }
	read(p[0], &g, 1); usleep(100000); kill(c, SIGSTOP); told_of(c, WUNTRACED); kill(c, SIGUSR1); usleep(300000); kill(c, SIGCONT); told_of(c, 0);
	// 4
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); sigemptyset(&set); sigaddset(&set, SIGCONT); handle(SIGCONT, 0); handle(SIGTSTP, 0); sigprocmask(SIG_BLOCK, &set, 0); write(p[1], "g", 1); usleep(300000); sigprocmask(SIG_UNBLOCK, &set, 0); say("SIGCONT blocked then a caught SIGTSTP sent: SIGCONT came %d, SIGTSTP %d\n", (int)came[SIGCONT], (int)came[SIGTSTP]); _exit(0); }
	read(p[0], &g, 1); kill(c, SIGCONT); kill(c, SIGTSTP); told_of(c, 0);
	// 5
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); handle(SIGUSR1, SA_RESTART); handle(SIGUSR2, SA_RESTART); handle(SIGCONT, SA_RESTART); write(p[1], "g", 1); usleep(400000); order[at] = 0; say("sent stopped USR1 USR1 USR2 then CONT: handlers ran %s\n", order); _exit(0); }
	read(p[0], &g, 1); kill(c, SIGSTOP); told_of(c, WUNTRACED); kill(c, SIGUSR1); kill(c, SIGUSR1); kill(c, SIGUSR2); kill(c, SIGCONT); told_of(c, 0);
	pipe(p);
	if ((c = fork()) == 0) { setpgid(0, 0); handle(SIGUSR1, 0); handle(SIGUSR2, 0); sigemptyset(&set); sigaddset(&set, SIGUSR1); sigaddset(&set, SIGUSR2); sigprocmask(SIG_BLOCK, &set, 0); raise(SIGUSR2); raise(SIGUSR1); raise(SIGUSR1); sigprocmask(SIG_UNBLOCK, &set, 0); order[at] = 0; say("blocked, sent itself USR2 USR1 USR1, unblocked: handlers ran %s\n", order); _exit(0); }
	told_of(c, 0);
	// 6
	if ((c = fork()) == 0) { setpgid(0, 0); usleep(200000); _exit(0); }
	usleep(50000); say("wait4 of 0 with the only child in another group: %s", told_of(0, WNOHANG)); say("; of its group: %s\n", told_of(-c, 0));
	// 7
	pid_t a = fork(); if (a == 0) { setpgid(0, 0); _exit(0); }
	pid_t b = fork(); if (b == 0) { setpgid(0, 0); raise(SIGSTOP); _exit(0); }
	usleep(150000); int st = 0; pid_t got = waitpid(-1, &st, WUNTRACED);
	say("an older child ended and a newer stopped: wait4 of any gives the %s first\n", got == a ? "older, ended" : got == b ? "newer, stopped" : "?"); kill(b, SIGCONT); waitpid(a, 0, 0); waitpid(b, 0, 0);
	a = fork(); if (a == 0) { setpgid(0, 0); raise(SIGSTOP); _exit(0); }
	b = fork(); if (b == 0) { setpgid(0, 0); _exit(0); }
	usleep(150000); got = waitpid(-1, &st, WUNTRACED);
	say("an older child stopped and a newer ended: the %s first\n", got == a ? "older, stopped" : got == b ? "newer, ended" : "?"); kill(a, SIGCONT); waitpid(a, 0, 0); waitpid(b, 0, 0);
	// 8
	pipe(p);
	pid_t mid = fork();
	if (mid == 0) { setpgid(0, 0); c = fork(); if (c == 0) { setpgid(0, 0); handle(SIGHUP, 0); handle(SIGCONT, 0); raise(SIGSTOP); order[at] = 0; say("stopped, and its parent ended: it goes on, having been sent %s\n", order); write(p[1], "g", 1); _exit(0); } waitpid(c, 0, WUNTRACED); _exit(0); }
	waitpid(mid, 0, 0); close(p[1]); read(p[0], &g, 1);
	// 9
	if ((c = fork()) == 0) { setpgid(0, 0); raise(SIGSTOP); _exit(0); }
	told_of(c, WUNTRACED); struct kinfo_proc k; size_t n = sizeof k; int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, c};
	int r = sysctl(mib, 4, &k, &n, 0, 0); say("kern.proc of one stopped: %d, p_stat %d", r, k.kp_proc.p_stat); mib[3] = getpid(); n = sizeof k; sysctl(mib, 4, &k, &n, 0, 0); say("; of itself %d\n", k.kp_proc.p_stat);
	kill(c, SIGCONT); told_of(c, 0);
}
#undef count
#undef at

int main(void) {
	signal(SIGPIPE, SIG_IGN);
	setpgid(0, 0);
	struct sigaction act;
	memset(&act, 0, sizeof act);
	act.sa_flags = SA_SIGINFO | SA_RESTART;
	act.sa_sigaction = child_changed;
	sigaction(SIGCHLD, &act, NULL);

	// SIGSTOP, and what a stopped process does with what is sent to it.
	pid_t child = worker(SIG_DFL, count, 0);
	ready();
	say("SIGSTOP sent: %d; waited for with WUNTRACED: %s; %s\n", kill(child, SIGSTOP), waited(child, WUNTRACED), news());
	say("asked again: %s; without WUNTRACED: %s\n", later(child, WUNTRACED), later(child, 0));
	say("stopped, it is sent nothing (0): %d, SIGUSR1: %d, SIGSTOP again: %d; then %s, %s\n", kill(child, 0), kill(child, SIGUSR1), kill(child, SIGSTOP), later(child, WUNTRACED), news());
	say("SIGCONT sent: %d; waited for with WCONTINUED: %s; %s\n", kill(child, SIGCONT), waited(child, WCONTINUED), news());
	say("asked again: %s; what came to it: %s\n", later(child, WCONTINUED), heard());
	say("SIGCONT sent to it going: %d; then with WCONTINUED: %s; %s; what came to it: %s\n", kill(child, SIGCONT), later(child, WCONTINUED), news(), heard());

	// A call it was in the middle of goes on: it waits on the pipe all the while.
	kill(child, SIGSTOP);
	waited(child, WUNTRACED);
	write(ask[1], "r", 1);
	say("asked something while stopped: it answers %s", later(child, WNOHANG | WUNTRACED));
	kill(child, SIGCONT);
	char line[96];
	size_t at = 0;
	while (at < 95 && read(answer[0], line + at, 1) == 1 && line[at] != '\n')
		at++;
	line[at] = 0;
	say("; set going, it answers: %s\n", line);
	news();

	// Stopped and then killed.
	kill(child, SIGSTOP);
	waited(child, WUNTRACED);
	news();
	say("SIGTERM sent to it stopped: %d; then %s", kill(child, SIGTERM), later(child, WUNTRACED));
	say("; SIGKILL: %d; then %s; %s\n", kill(child, SIGKILL), waited(child, WUNTRACED), news());
	close(ask[1]);
	close(answer[0]);

	// The stop a terminal asks for: taken by default, caught, ignored.
	child = worker(SIG_DFL, SIG_DFL, 0);
	ready();
	say("SIGTSTP sent to one that takes it as it is: %d; %s; %s\n", kill(child, SIGTSTP), waited(child, WUNTRACED), news());
	say("SIGTTIN sent to it stopped: %d; then %s", kill(child, SIGTTIN), later(child, WUNTRACED));
	say("; SIGCONT, which it takes as it is: %d; %s; %s; what came to it: %s\n", kill(child, SIGCONT), waited(child, WCONTINUED), news(), heard());
	say("SIGTTIN: %d; %s", kill(child, SIGTTIN), waited(child, WUNTRACED));
	kill(child, SIGCONT);
	waited(child, WCONTINUED);
	say("; SIGTTOU: %d; %s\n", kill(child, SIGTTOU), waited(child, WUNTRACED));
	kill(child, SIGCONT);
	waited(child, WCONTINUED);
	news();
	done(child);
	child = worker(count, count, 0);
	ready();
	say("SIGTSTP sent to one that catches it: %d; %s; %s; what came to it: %s\n", kill(child, SIGTSTP), later(child, WUNTRACED), news(), heard());
	done(child);
	child = worker(SIG_IGN, count, 0);
	ready();
	say("to one that ignores it: %d; %s; what came to it: %s\n", kill(child, SIGTSTP), later(child, WUNTRACED), heard());
	done(child);

	// A group with nobody to see to it is not stopped by the terminal's signals, and is by SIGSTOP.
	child = worker(SIG_DFL, count, 1);
	ready();
	say("SIGTSTP sent to one in a session of its own: %d; %s; SIGTTIN: %d; %s", kill(child, SIGTSTP), later(child, WUNTRACED), kill(child, SIGTTIN), later(child, WUNTRACED));
	say("; SIGSTOP: %d; %s\n", kill(child, SIGSTOP), waited(child, WUNTRACED));
	kill(child, SIGCONT);
	waited(child, WCONTINUED);
	news();
	done(child);

	// A parent that does not want to be told of stops.
	act.sa_flags = SA_SIGINFO | SA_RESTART | SA_NOCLDSTOP;
	sigaction(SIGCHLD, &act, NULL);
	child = worker(SIG_DFL, count, 0);
	ready();
	say("with SA_NOCLDSTOP, SIGSTOP sent: %d; %s; %s", kill(child, SIGSTOP), waited(child, WUNTRACED), news());
	say("; SIGCONT: %d; %s; %s\n", kill(child, SIGCONT), waited(child, WCONTINUED), news());
	done(child);

	// One that stops itself.
	pid_t self = fork();
	if (self == 0) {
		setpgid(0, 0);
		raise(SIGSTOP);
		_exit(7);
	}
	say("one that sends itself SIGSTOP: %s", waited(self, WUNTRACED));
	say("; set going: %d, and waited for: %s\n", kill(self, SIGCONT), waited(self, 0));
	signal(SIGCHLD, SIG_DFL);
	rest();
	return 0;
}
