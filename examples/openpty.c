// A pseudo-terminal by the library's calls: openpty, which makes one and opens both ends, and
// forkpty, which also starts a copy of the program with the second end for its terminal (as
// login_tty makes it: a session of its own, the terminal its own, and its standard input, output
// and error). What each gives, and what the list of processes says of one that has a terminal.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>
#include <util.h>

static const char *shown(const char *bytes, long n) {
	static char text[600];
	int at = 0;
	for (long i = 0; i < n && at < 590; i++) {
		unsigned char c = (unsigned char)bytes[i];
		if (c == '\r' || c == '\n')
			at += snprintf(text + at, 600 - (size_t)at, c == '\r' ? "\\r" : "\\n");
		else
			text[at++] = (char)c;
	}
	text[at] = 0;
	return text;
}

int main(void) {
	int m = -1, s = -1;
	char name[128] = "";
	struct winsize given = {30, 100, 0, 0}, size = {0, 0, 0, 0};
	struct termios t;
	int r = openpty(&m, &s, name, NULL, &given);
	ioctl(s, TIOCGWINSZ, &size);
	const char *called = ttyname(s);
	struct stat st;
	memset(&st, 0, sizeof st);
	stat(name, &st);
	printf("openpty: %d; both ends terminals: %d %d; its name is the second end's: %d, the user's: %d, mode %o; its size %d by %d; its settings read: %d\n", r, isatty(m), isatty(s), called && !strcmp(called, name),
	       st.st_uid == getuid(), (unsigned)st.st_mode, size.ws_row, size.ws_col, tcgetattr(s, &t));
	printf("no group is in front, asked at the first end: %d\n", tcgetpgrp(m));
	close(m);
	close(s);

	// forkpty: the child says what it finds, at its terminal, and waits to be told to end.
	fflush(stdout);
	char second[128] = "";
	pid_t child = forkpty(&m, second, NULL, &given);
	if (child == 0) {
		char line[400];
		const char *own = ttyname(0);
		int tty = open("/dev/tty", O_RDWR);
		ioctl(0, TIOCGWINSZ, &size);
		int n = snprintf(line, sizeof line, "the child: its input, output and error are terminals %d %d %d, the same one %d; a session of its own %d; its group in front %d; /dev/tty opens %d; %d by %d\n", isatty(0), isatty(1),
		                 isatty(2), own && ttyname(2) && !strcmp(own, ttyname(2)), getsid(0) == getpid(), tcgetpgrp(0) == getpid(), tty >= 0, size.ws_row, size.ws_col);
		write(1, line, (size_t)n);
		char c;
		read(0, &c, 1);
		_exit(5);
	}
	char seen[600];
	long got = 0, n;
	while (got < (long)sizeof seen - 1 && (n = read(m, seen + got, sizeof seen - 1 - (size_t)got)) > 0) {
		got += n;
		if (memchr(seen, '\n', (size_t)got))
			break;
	}
	printf("forkpty: a child %d, a name given %d; the first end reads: %s\n", child > 0, !strncmp(second, "/dev/ttys", 9), shown(seen, got));

	// What the list of processes says of the child: its terminal, the group in front, and
	// that it has one.
	struct kinfo_proc k;
	size_t len = sizeof k;
	int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, child};
	memset(&k, 0, sizeof k);
	r = sysctl(mib, 4, &k, &len, NULL, 0);
	stat(second, &st);
	printf("the list of processes says of it: %d; its terminal is that device %d; the group in front is its own %d; marked as having a terminal %d, as leading its session %d\n", r, k.kp_eproc.e_tdev == st.st_rdev,
	       k.kp_eproc.e_tpgid == child, !!(k.kp_proc.p_flag & P_CONTROLT), !!(k.kp_eproc.e_flag & EPROC_SLEADER));
	len = sizeof k;
	mib[3] = getpid();
	memset(&k, 0, sizeof k);
	sysctl(mib, 4, &k, &len, NULL, 0);
	printf("and of one whose terminal it is not: its group in front %d\n", k.kp_eproc.e_tdev == st.st_rdev ? -2 : 0);
	// What is typed is echoed, and a session's leader that has had /dev/tty open does not end
	// until what its terminal showed has been read: so it is read before the child is waited for.
	write(m, "x\n", 2);
	got = 0;
	while (got < (long)sizeof seen - 1 && (n = read(m, seen + got, sizeof seen - 1 - (size_t)got)) > 0) {
		got += n;
		if (memchr(seen, '\n', (size_t)got))
			break;
	}
	int status = 0;
	waitpid(child, &status, 0);
	printf("told to end, the first end having read %s: %d, with %d\n", shown(seen, got), WIFEXITED(status), WEXITSTATUS(status));
	return 0;
}
