// Jobs at a terminal: a session's leader with a terminal of its own and a job in a group of its
// own, as a shell has. The key that suspends stops the job in front; a job behind that reads the
// terminal is stopped (SIGTTIN), and one that writes to it where it is set so, or sets
// something of it, is too (SIGTTOU), until it is put in front and set going, when what it was
// doing goes on; one that catches the signal has its call broken off instead. It prints the
// same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>

static void say(const char *format, ...) {
	char line[600];
	va_list args;
	va_start(args, format);
	int n = vsnprintf(line, sizeof line, format, args);
	va_end(args);
	write(1, line, (size_t)n);
}

static const char *said(long r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%ld", r);
	return t;
}

// took reads what is there without waiting and says it.
static const char *took(int fd) {
	static char text[4][300];
	static int next;
	char *t = text[next++ % 4], bytes[100];
	int flags = fcntl(fd, F_GETFL);
	fcntl(fd, F_SETFL, flags | O_NONBLOCK);
	ssize_t n = read(fd, bytes, sizeof bytes);
	int e = errno;
	fcntl(fd, F_SETFL, flags);
	if (n < 0) {
		snprintf(t, 300, "-1 (%s)", strerror(e));
		return t;
	}
	int at = snprintf(t, 300, "%zd \"", n);
	for (ssize_t i = 0; i < n; i++) {
		unsigned char c = (unsigned char)bytes[i];
		if (c == '\r' || c == '\n')
			at += snprintf(t + at, 300 - (size_t)at, c == '\r' ? "\\r" : "\\n");
		else if (c < 32 || c == 127)
			at += snprintf(t + at, 300 - (size_t)at, "^%c", c ^ 64);
		else
			t[at++] = (char)c;
	}
	t[at++] = '"';
	t[at] = 0;
	return t;
}

static const char *waited(pid_t child, int how) {
	static char text[4][64];
	static int next;
	char *t = text[next++ % 4];
	int status = 0;
	pid_t got;
	while ((got = waitpid(child, &status, how)) < 0 && errno == EINTR)
		;
	if (got < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else if (got == 0)
		snprintf(t, 64, "nothing");
	else if (WIFSTOPPED(status))
		snprintf(t, 64, "stopped by %d", WSTOPSIG(status));
	else if (WIFCONTINUED(status))
		snprintf(t, 64, "going again");
	else if (WIFSIGNALED(status))
		snprintf(t, 64, "ended by %d", WTERMSIG(status));
	else
		snprintf(t, 64, "ended with %d", WEXITSTATUS(status));
	return t;
}

static volatile sig_atomic_t came[32];
static void count(int sig) { came[sig]++; }

// The job does what it is told down a pipe and says how it went: read the terminal ('r'), write
// to it ('w'), set its settings to what they are ('s'), catch the signals that would stop it
// ('c'), end ('x').
static void job(int s, int ask, int answer) {
	char c, line[96];
	while (read(ask, &c, 1) == 1 && c != 'x') {
		long r = 0;
		char bytes[16];
		struct termios t;
		switch (c) {
		case 'r':
			r = read(s, bytes, sizeof bytes);
			break;
		case 'w':
			r = write(s, "job\n", 4);
			break;
		case 's':
			tcgetattr(s, &t);
			r = tcsetattr(s, TCSANOW, &t);
			break;
		case 'c':
			signal(SIGTTIN, count);
			signal(SIGTTOU, count);
			signal(SIGTSTP, count);
			break;
		}
		int n = snprintf(line, sizeof line, "%s, with SIGTTIN %d, SIGTTOU %d, SIGTSTP %d\n", said(r), (int)came[SIGTTIN], (int)came[SIGTTOU], (int)came[SIGTSTP]);
		write(answer, line, (size_t)n);
	}
	_exit(0);
}

static int ask[2], answer[2];
static void tell(char what) { write(ask[1], &what, 1); }
static const char *heard(void) {
	static char text[4][96];
	static int next;
	char *t = text[next++ % 4];
	size_t at = 0;
	while (at < 95 && read(answer[0], t + at, 1) == 1 && t[at] != '\n')
		at++;
	t[at] = 0;
	return t;
}

// later is waited without waiting, after a moment.
static const char *later(pid_t child, int how) {
	usleep(80000);
	return waited(child, how | WNOHANG);
}

static void leader(void) {
	int m = posix_openpt(O_RDWR | O_NOCTTY);
	grantpt(m);
	unlockpt(m);
	setsid();
	int s = open(ptsname(m), O_RDWR);
	signal(SIGTTOU, SIG_IGN); // as a shell does, to put groups in front from behind
	signal(SIGTSTP, count);
	pipe(ask);
	pipe(answer);
	pid_t j = fork();
	if (j == 0) {
		setpgid(0, 0);
		close(m);
		close(ask[1]);
		close(answer[0]);
		signal(SIGTTOU, SIG_DFL);
		signal(SIGTSTP, SIG_DFL);
		job(s, ask[0], answer[1]);
	}
	setpgid(j, j);
	close(ask[0]);
	close(answer[1]);
	pid_t me = getpid();

	// The key that suspends.
	int before = came[SIGTSTP];
	write(m, "ab\x1a", 3);
	usleep(80000);
	say("the key that suspends typed with the leader's group in front, which catches it: SIGTSTP came %d; the first end sees %s", (int)came[SIGTSTP] - before, took(m));
	int n = -1;
	ioctl(s, FIONREAD, &n);
	write(m, "\n", 1);
	say("; then a newline, and the second end reads %s\n", took(s));
	took(m);
	tcsetpgrp(s, j);
	write(m, "cd\x1a", 3);
	say("with the job in front: the job is %s; the first end sees %s", waited(j, WUNTRACED), took(m));
	kill(j, SIGCONT);
	say("; set going: %s\n", waited(j, WCONTINUED));
	write(m, "x\n", 2);
	tell('r');
	say("in front it reads what is typed: %s\n", heard());
	took(m);

	// Behind, it is stopped by reading; in front and set going, the read goes on.
	tcsetpgrp(s, me);
	tell('r');
	say("behind, it reads: the job is %s", waited(j, WUNTRACED));
	kill(j, SIGCONT);
	say("; set going still behind: %s, then %s", waited(j, WCONTINUED), later(j, WUNTRACED));
	tcsetpgrp(s, j);
	kill(j, SIGCONT);
	waited(j, WCONTINUED);
	write(m, "y\n", 2);
	say("; put in front and set going, with a line typed: %s\n", heard());
	took(m);

	// Writing from behind stops it only where the terminal is set so.
	tcsetpgrp(s, me);
	tell('w');
	say("behind, it writes: %s; the first end sees %s\n", heard(), took(m));
	struct termios t;
	tcgetattr(s, &t);
	t.c_lflag |= TOSTOP;
	tcsetattr(s, TCSANOW, &t);
	tell('w');
	say("with the terminal set to stop that: the job is %s", waited(j, WUNTRACED));
	tcsetpgrp(s, j);
	kill(j, SIGCONT);
	waited(j, WCONTINUED);
	say("; put in front and set going: %s; the first end sees %s\n", heard(), took(m));
	tcsetpgrp(s, me);
	t.c_lflag &= ~(tcflag_t)TOSTOP;
	tcsetattr(s, TCSANOW, &t);

	// Setting something of it from behind.
	tell('s');
	say("behind, it sets the terminal's settings: the job is %s", waited(j, WUNTRACED));
	tcsetpgrp(s, j);
	kill(j, SIGCONT);
	waited(j, WCONTINUED);
	say("; put in front and set going: %s\n", heard());
	tcsetpgrp(s, me);

	// One that catches the signals is not stopped: its call is broken off.
	tell('c');
	heard();
	tell('r');
	say("catching the signals, behind, it reads: %s; the job is %s\n", heard(), later(j, WUNTRACED));
	tell('s');
	say("it sets the settings: %s\n", heard());
	tcsetpgrp(s, j);
	write(m, "\x1a", 1);
	usleep(80000);
	tell('s');
	say("in front, the key that suspends typed: %s; the job is %s\n", heard(), later(j, WUNTRACED));
	tell('x');
	say("told to end: %s\n", waited(j, 0));
	_exit(0);
}

int main(void) {
	signal(SIGPIPE, SIG_IGN);
	pid_t child = fork();
	if (child == 0)
		leader();
	say("the leader: %s\n", waited(child, 0));
	return 0;
}
