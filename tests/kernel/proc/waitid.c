// waitid: what a parent is told of a child that ended, was ended, was stopped or set going
// again, with each of the things it may ask for or leave out, and what it is refused. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static pid_t child, other;

// who names a process a siginfo speaks of without saying its number.
static const char *who(pid_t pid) {
	static char text[4][24];
	static int next;
	char *t = text[next++ % 4];
	if (pid == child)
		return "the child";
	if (pid == other)
		return "the other child";
	if (pid == getpid())
		return "the parent";
	snprintf(t, 24, pid == 0 ? "nobody" : pid == 77 ? "as it was" : "another (%s)", pid < 0 ? "negative" : "positive");
	return t;
}

// asked calls waitid and says what came of it. The siginfo starts with marks in it, to show
// what is written and what is left.
static void asked(const char *label, idtype_t type, id_t id, int options) {
	siginfo_t info;
	memset(&info, 0, sizeof info);
	info.si_pid = 77;
	info.si_signo = 77;
	info.si_code = 77;
	info.si_status = 77;
	info.si_uid = 77;
	errno = 0;
	int r = waitid(type, id, &info, options);
	if (r)
		printf("%s: %s\n", label, strerror(errno));
	else
		printf("%s: of %s, signal %d, code %d, status %#x, user %s, errno %d, address %s\n", label, who(info.si_pid), info.si_signo, info.si_code, info.si_status,
		       info.si_uid == getuid() ? "the parent's" : info.si_uid == 0 ? "0" : info.si_uid == 77 ? "as it was" : "another", info.si_errno, info.si_addr ? "set" : "none");
}

// started is a child that does what: ends with a status, waits to be ended, or sleeps a
// while and ends.
static pid_t started(int what, int status) {
	pid_t pid = fork();
	if (pid == 0) {
		if (what == 0)
			_exit(status);
		if (what == 1)
			exit(status);
		if (what == 3)
			setpgid(0, 0);
		for (;;)
			pause();
	}
	if (what == 3)
		setpgid(pid, pid);
	return pid;
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	asked("with no child", P_ALL, 0, WEXITED);
	asked("with no child and nothing asked for", P_ALL, 0, 0);

	child = started(0, 7);
	asked("asked of nothing", P_PID, child, 0);
	asked("asked of what there is not", P_PID, child, WEXITED | 0x100000);
	asked("a child of a number below zero", P_PID, (id_t)-1, WEXITED);
	asked("a group of a number below zero", P_PGID, (id_t)-1, WEXITED);
	asked("a child that is not one's own", P_PID, 1, WEXITED);
	asked("a child that ended with 7, to be left to be asked of again", P_PID, child, WEXITED | WNOWAIT);
	asked("the same again, asked of those stopped and not of those that ended, and not to wait", P_PID, child, WSTOPPED | WNOHANG);
	asked("the same again", P_PID, child, WEXITED);
	asked("and once more", P_PID, child, WEXITED);

	child = started(1, 0x12345);
	asked("a child that ended with 0x12345", P_ALL, 0, WEXITED);

	child = started(2, 0);
	asked("a child that goes on, not to wait", P_PID, child, WEXITED | WNOHANG);
	kill(child, SIGSTOP);
	asked("stopped", P_PID, child, WSTOPPED | WNOWAIT);
	asked("stopped, asked again", P_PID, child, WSTOPPED);
	asked("stopped, asked a third time and not to wait", P_PID, child, WSTOPPED | WNOHANG);
	kill(child, SIGCONT);
	asked("set going again", P_PID, child, WCONTINUED | WNOWAIT);
	asked("set going again, asked again", P_PID, child, WCONTINUED);
	asked("set going again, asked a third time and not to wait", P_PID, child, WCONTINUED | WNOHANG);
	kill(child, SIGTERM);
	asked("ended by SIGTERM, any kind of child asked of (9)", (idtype_t)9, 0, WEXITED | WSTOPPED | WCONTINUED);

	child = started(3, 0);
	other = started(2, 0);
	asked("of the group 0, of two children that go on", P_PGID, 0, WEXITED | WNOHANG);
	kill(other, SIGKILL);
	kill(child, SIGKILL);
	asked("a child in a group of its own, by the group, ended by SIGKILL", P_PGID, child, WEXITED);
	asked("that group again", P_PGID, child, WEXITED);
	asked("any child", P_ALL, 0, WEXITED);
	asked("any child again", P_ALL, 0, WEXITED | WNOHANG);
	return 0;
}
