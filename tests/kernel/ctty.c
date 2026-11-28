// A terminal as a session's own. A child leaves for a session of its own and takes the second
// end of a pseudo-terminal for its terminal, as a login or a terminal window's shell does: by
// opening it, or by asking (TIOCSCTTY). Then /dev/tty is that terminal, the terminal has a group
// in front, the keys that interrupt and quit signal that group and let go what was typed, a
// change of size is told to it, and when the first end closes the session's leader is told the
// terminal has hung up. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>

// say writes a whole line at once, so that the lines of several processes do not mix.
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

// The signals that came, counted by a handler.
static volatile sig_atomic_t came[32];
static void count(int sig) { came[sig]++; }

// awaited waits for a signal to have come more often than before, for a second at most, and
// says whether it did.
static int awaited(int sig, int before) {
	for (int i = 0; i < 200 && came[sig] == before; i++)
		usleep(5000);
	return came[sig] - before;
}

// Each side tells the other to go on by a byte down a pipe.
static void tell(int fd) { write(fd, "g", 1); }
static void wait_for(int fd) {
	char c;
	while (read(fd, &c, 1) < 0 && errno == EINTR)
		;
}

// who says which of the processes a group's number is.
static const char *who(pid_t group, pid_t parent, pid_t child, pid_t grandchild) {
	return group == child ? "the leader's" : group == grandchild ? "the other's" : group == parent ? "the first process's" : group < 0 ? said(group) : "another";
}

static void counting(void) {
	struct sigaction act;
	memset(&act, 0, sizeof act);
	act.sa_handler = count;
	for (int sig = 1; sig < 32; sig++)
		if (sig != SIGKILL && sig != SIGSTOP && sig != SIGCHLD && sig != SIGPIPE)
			sigaction(sig, &act, NULL);
}

// rest is the rest of it, on a terminal of its own: a terminal that is nobody's, one taken by
// being opened, what a group behind may do of it where the signals that would stop it are
// ignored or blocked, and the leader of the session ending.
static void rest(void) {
	int m = posix_openpt(O_RDWR | O_NOCTTY);
	grantpt(m);
	unlockpt(m);
	char name[128];
	strlcpy(name, ptsname(m), sizeof name);
	int s = open(name, O_RDWR | O_NOCTTY);
	int group = -5;
	say("a terminal that is nobody's: the group in front asked at the first end: %s, %d", said(ioctl(m, TIOCGPGRP, &group)), group);
	say("; the interrupt typed after \"ab\": %s, and the first end sees %s\n", said(write(m, "ab\x03", 3)), took(m));
	say("a group put in front by one whose terminal it is not: %s; at the first end: %s\n", said(tcsetpgrp(s, getpgrp())), said(tcsetpgrp(m, getpgrp())));
	close(s);
	int up[2], down[2];
	pipe(up);
	pipe(down);
	pid_t leader = fork();
	if (leader == 0) {
		setsid();
		int k = open(name, O_RDWR);
		say("opened without O_NOCTTY by the leader of a session: its group is in front %d; /dev/tty opens %d\n", tcgetpgrp(k) == getpid(), open("/dev/tty", O_RDWR) >= 0);
		counting();
		pid_t behind = fork();
		if (behind == 0) {
			setpgid(0, 0);
			char c;
			signal(SIGTTIN, SIG_IGN);
			say("from behind, with SIGTTIN ignored a read: %s", said(read(k, &c, 1)));
			say("; a write: %s", said(write(k, "w", 1)));
			struct termios t;
			tcgetattr(k, &t);
			signal(SIGTTOU, SIG_IGN);
			t.c_lflag |= TOSTOP;
			say("; with SIGTTOU ignored, set to stop a write from behind: %s", said(tcsetattr(k, TCSANOW, &t)));
			say(", and a write: %s", said(write(k, "v", 1)));
			sigset_t set;
			sigemptyset(&set);
			sigaddset(&set, SIGTTOU);
			sigaddset(&set, SIGTTIN);
			signal(SIGTTOU, count);
			signal(SIGTTIN, count);
			sigprocmask(SIG_BLOCK, &set, NULL);
			say("; with both blocked a write: %s", said(write(k, "u", 1)));
			say(", a read: %s", said(read(k, &c, 1)));
			say("; it is told the group in front: %d\n", tcgetpgrp(k) == getppid());
			t.c_lflag &= ~(tcflag_t)TOSTOP;
			tcsetattr(k, TCSANOW, &t);
			_exit(0);
		}
		waitpid(behind, NULL, 0);
		// The leader ends with one of its group still having the terminal open.
		int gone[2], set[2];
		pipe(gone);
		pipe(set);
		pid_t left = fork();
		if (left == 0) {
			int hups = came[SIGHUP], conts = came[SIGCONT];
			close(gone[1]);
			tell(set[1]);
			wait_for(gone[0]); // which ends when the leader has
			int hup = awaited(SIGHUP, hups);
			char c;
			say("the leader ended: to one of its group SIGHUP came %d, SIGCONT %d; it reads %s", hup, came[SIGCONT] - conts, said(read(k, &c, 1)));
			say(", writes %s", said(write(k, "x", 1)));
			say("; the group in front: %s", said(tcgetpgrp(k)));
			struct termios t;
			say("; its settings: %s", said(tcgetattr(k, &t)));
			say("; /dev/tty opened: %s\n", said(open("/dev/tty", O_RDWR)));
			tell(up[1]);
			_exit(0);
		}
		close(gone[0]);
		wait_for(set[0]);
		tell(up[1]);
		wait_for(down[0]);
		_exit(0);
	}
	wait_for(up[0]);
	say("the first end sees %s\n", took(m));
	tell(down[1]);
	waitpid(leader, NULL, 0);
	wait_for(up[0]);
	say("asked at the first end then, the group in front: %s, %d\n", said(ioctl(m, TIOCGPGRP, &group)), group);
	close(m);

	// The leader of a session ends with nobody else at its terminal, as the shell in a
	// terminal window does: the first end reads what it wrote and then the end.
	m = posix_openpt(O_RDWR | O_NOCTTY);
	grantpt(m);
	unlockpt(m);
	strlcpy(name, ptsname(m), sizeof name);
	leader = fork();
	if (leader == 0) {
		close(m);
		setsid();
		s = open(name, O_RDWR);
		write(s, "bye\n", 4);
		_exit(0);
	}
	char seen[32];
	ssize_t n = read(m, seen, sizeof seen);
	say("a leader wrote at its terminal and ended: the first end reads %zd", n);
	say(", then %s", said(read(m, seen, sizeof seen)));
	waitpid(leader, NULL, 0);
	struct pollfd asked = {m, POLLIN | POLLOUT, 0};
	poll(&asked, 1, 0);
	say("; it can be read and written %#x; typed at: %s\n", asked.revents, said(write(m, "x", 1)));
}

// The cases of a terminal let go and of /dev/tty, each by a session's leader with a terminal of
// its own that nobody else has.
static int first;
static char second[128];

static const char *whose(long group) { return group == getpid() ? "the leader's" : said(group); }

static int polled(int fd, int what) {
	struct pollfd asked = {fd, (short)what, 0};
	return poll(&asked, 1, 0) < 0 ? -1 : asked.revents;
}

// The second end closed everywhere, with /dev/tty never opened: the terminal is no session's.
static void let_go(void) {
	int s = open(second, O_RDWR | O_NOCTTY);
	ioctl(s, TIOCSCTTY, 0);
	close(s);
	say("the second end closed, /dev/tty never opened: the group in front, asked at the first end: %s", whose(tcgetpgrp(first)));
	say("; /dev/tty opened: %s", said(open("/dev/tty", O_RDWR)));
	s = open(second, O_RDWR);
	say("; opened again without O_NOCTTY: the group in front %s", whose(tcgetpgrp(s)));
	say(", its session %s", whose(tcgetsid(s)));
	int before = came[SIGINT];
	write(first, "\3", 1);
	say("; the interrupt typed: SIGINT came %d", awaited(SIGINT, before));
	say("; asked for again: %s", said(ioctl(s, TIOCSCTTY, 0)));
	say(", and the group in front is %s\n", whose(tcgetpgrp(s)));
	close(s);
	pid_t after = fork();
	if (after == 0) {
		say("to a copy made after it was let go again, /dev/tty opened: %s\n", said(open("/dev/tty", O_RDWR)));
		_exit(0);
	}
	waitpid(after, NULL, 0);
}

// /dev/tty opened once keeps the terminal open for the session.
static void kept(void) {
	int s = open(second, O_RDWR | O_NOCTTY);
	ioctl(s, TIOCSCTTY, 0);
	int own = open("/dev/tty", O_RDWR);
	close(own);
	close(s);
	say("/dev/tty opened and closed, then the second end closed: the first end polls %#x", polled(first, POLLIN));
	say("; the group in front is %s", whose(tcgetpgrp(first)));
	say("; typed at: %s", said(write(first, "a\n", 2)));
	own = open("/dev/tty", O_RDWR);
	char bytes[8];
	fcntl(own, F_SETFL, O_RDWR | O_NONBLOCK);
	say("; /dev/tty opened again reads %s", said(read(own, bytes, sizeof bytes)));
	fcntl(first, F_SETFL, O_RDWR | O_NONBLOCK);
	say("; the first end reads %s", said(read(first, bytes, sizeof bytes)));
	int before = came[SIGHUP];
	close(first);
	say("; the first end closed: SIGHUP came %d\n", awaited(SIGHUP, before));
}

// /dev/tty is the terminal of whoever uses it, looked for at each use.
static void looked_up(void) {
	int s = open(second, O_RDWR | O_NOCTTY);
	ioctl(s, TIOCSCTTY, 0);
	int own = open("/dev/tty", O_RDWR);
	pid_t left = fork();
	if (left == 0) {
		setsid();
		struct winsize size;
		char c;
		say("/dev/tty in the hands of one that left for a session of its own: written %s", said(write(own, "x", 1)));
		say(", read %s", said(read(own, &c, 1)));
		say(", its size asked %s", said(ioctl(own, TIOCGWINSZ, &size)));
		say(", polled %#x\n", polled(own, POLLIN | POLLOUT));
		_exit(0);
	}
	waitpid(left, NULL, 0);
	struct kevent watch;
	EV_SET(&watch, own, EVFILT_READ, EV_ADD, 0, 0, 0);
	say("a kqueue asked to watch /dev/tty: %s; polled by its own session: %#x\n", said(kevent(kqueue(), &watch, 1, NULL, 0, NULL)), polled(own, POLLIN | POLLOUT));
}

// /dev/tty in the hands of one whose session's leader has ended.
static void outlived(void) {
	int gone[2];
	pipe(gone);
	int s = open(second, O_RDWR | O_NOCTTY);
	ioctl(s, TIOCSCTTY, 0);
	int own = open("/dev/tty", O_RDWR);
	if (fork() == 0) {
		close(gone[1]);
		char c;
		read(gone[0], &c, 1); // which ends when the leader has
		usleep(50000);
		struct winsize size;
		say("/dev/tty after the session's leader ended: read %s", said(read(own, &c, 1)));
		say(", written %s", said(write(own, "x", 1)));
		say(", its size asked %s", said(ioctl(own, TIOCGWINSZ, &size)));
		say(", polled %#x\n", polled(own, POLLIN | POLLOUT));
		_exit(0);
	}
}

// One of the leader's group that came of it before the terminal was taken has no terminal.
static void came_before(void) {
	int s = open(second, O_RDWR | O_NOCTTY);
	int to[2], from[2];
	pipe(to);
	pipe(from);
	pid_t early = fork();
	if (early == 0) {
		wait_for(to[0]);
		int ints = came[SIGINT], sizes = came[SIGWINCH];
		tell(from[1]);
		int got = awaited(SIGINT, ints);
		say("to one of the group that was there before the terminal was taken: SIGINT came %d, SIGWINCH %d", got, came[SIGWINCH] - sizes);
		say("; it asks for the group in front: %s", said(tcgetpgrp(s)));
		say("; /dev/tty opened: %s\n", said(open("/dev/tty", O_RDWR)));
		_exit(0);
	}
	ioctl(s, TIOCSCTTY, 0);
	tell(to[1]);
	wait_for(from[0]);
	struct winsize size = {5, 5, 0, 0};
	ioctl(first, TIOCSWINSZ, &size);
	int before = came[SIGINT];
	write(first, "\3", 1);
	int mine = awaited(SIGINT, before);
	waitpid(early, NULL, 0);
	say("and to the leader SIGINT came %d\n", mine);
}

static void cases(void) {
	void (*each[])(void) = {let_go, kept, looked_up, outlived, came_before};
	for (int i = 0; i < 5; i++) {
		pid_t leader = fork();
		if (leader == 0) {
			first = posix_openpt(O_RDWR | O_NOCTTY);
			grantpt(first);
			unlockpt(first);
			strlcpy(second, ptsname(first), sizeof second);
			setsid();
			counting();
			each[i]();
			_exit(0);
		}
		waitpid(leader, NULL, 0);
		if (each[i] == outlived)
			usleep(300000); // for the one it left behind to say its piece
	}
}

int main(void) {
	signal(SIGPIPE, SIG_IGN);
	int m = posix_openpt(O_RDWR | O_NOCTTY);
	grantpt(m);
	unlockpt(m);
	char name[128];
	strlcpy(name, ptsname(m), sizeof name);
	int down[2], up[2];
	pipe(down);
	pipe(up);
	pid_t parent = getpgrp(); // the group the first process is in

	// One that does not lead a session cannot take a terminal for its own.
	int s = open(name, O_RDWR | O_NOCTTY);
	pid_t tried = fork();
	if (tried == 0) {
		setpgid(0, 0);
		say("asked for by one that leads no session: %s\n", said(ioctl(s, TIOCSCTTY, 0)));
		_exit(0);
	}
	waitpid(tried, NULL, 0);
	close(s);

	pid_t child = fork();
	if (child == 0) {
		close(down[1]);
		close(up[0]);
		pid_t me = getpid();
		struct sigaction act;
		memset(&act, 0, sizeof act);
		act.sa_handler = count;
		for (int sig = 1; sig < 32; sig++)
			if (sig != SIGKILL && sig != SIGSTOP && sig != SIGCHLD && sig != SIGPIPE)
				sigaction(sig, &act, NULL);
		say("a session of its own: %d; then /dev/tty opened: %s\n", setsid() == me, said(open("/dev/tty", O_RDWR)));

		// Opened with O_NOCTTY it is not the session's; asked for, it is, and again is no error.
		s = open(name, O_RDWR | O_NOCTTY);
		say("opened with O_NOCTTY: the group in front %s", said(tcgetpgrp(s)));
		say("; /dev/tty opened: %s\n", said(open("/dev/tty", O_RDWR)));
		say("asked for: %s", said(ioctl(s, TIOCSCTTY, 0)));
		say("; again: %s; the group in front is %s", said(ioctl(s, TIOCSCTTY, 0)), who(tcgetpgrp(s), parent, me, 0));
		say("; asked at the first end: %s\n", who(tcgetpgrp(m), parent, me, 0));
		close(m); // the first process's is then the only one
		int own = open("/dev/tty", O_RDWR);
		struct stat st;
		memset(&st, 0, sizeof st);
		fstat(own, &st);
		say("/dev/tty opened: %d; a terminal %d, a device of characters %d; its session is the leader's: %d\n", own >= 0, isatty(own), S_ISCHR(st.st_mode), tcgetsid(own) == me);
		say("written through /dev/tty: %s\n", said(write(own, "tty\n", 4)));
		tell(up[1]);
		wait_for(down[0]);

		// The keys that interrupt and quit.
		int before = came[SIGINT];
		tell(up[1]);
		say("the interrupt typed after \"abc\": SIGINT came %d; what was typed is read as %s\n", awaited(SIGINT, before), took(s));
		tell(up[1]);
		wait_for(down[0]);
		before = came[SIGQUIT];
		tell(up[1]);
		say("the quit typed: SIGQUIT came %d\n", awaited(SIGQUIT, before));
		tell(up[1]);
		wait_for(down[0]);

		// A change of size.
		before = came[SIGWINCH];
		tell(up[1]);
		say("the size changed at the first end: SIGWINCH came %d", awaited(SIGWINCH, before));
		tell(up[1]);
		wait_for(down[0]);
		before = came[SIGWINCH];
		struct winsize size = {10, 20, 0, 0};
		ioctl(s, TIOCSWINSZ, &size);
		say("; at the second: %d", awaited(SIGWINCH, before));
		before = came[SIGWINCH];
		ioctl(s, TIOCSWINSZ, &size);
		usleep(50000);
		say("; set to what it is: %d\n", came[SIGWINCH] - before);

		// Another group put in front: it gets the keys, and the leader's does not.
		int to[2], from[2];
		pipe(to);
		pipe(from);
		pid_t other = fork();
		if (other == 0) {
			setpgid(0, 0);
			tell(from[1]);
			wait_for(to[0]);
			int had = came[SIGINT];
			tell(from[1]);
			say("the interrupt typed with the other group in front: it came to the other %d", awaited(SIGINT, had));
			say("; the other asks which group is in front: %s\n", who(tcgetpgrp(s), parent, me, getpid()));
			tell(from[1]);
			wait_for(to[0]);
			// Behind again, it is told when the terminal hangs up only if it is stopped,
			// which it is not: it waits to be told to end.
			had = came[SIGHUP];
			tell(from[1]);
			wait_for(to[0]);
			say("to the other, behind: SIGHUP %d\n", came[SIGHUP] - had);
			_exit(0);
		}
		wait_for(from[0]);
		say("a group that is not there put in front: %s", said(tcsetpgrp(s, 99999)));
		say("; the first process's, of another session: %s", said(tcsetpgrp(s, parent)));
		say("; the other's: %s; in front is %s\n", said(tcsetpgrp(s, other)), who(tcgetpgrp(s), parent, me, other));
		before = came[SIGINT];
		tell(to[1]);
		wait_for(from[0]);
		tell(up[1]);
		wait_for(from[0]);
		say("and to the leader %d\n", came[SIGINT] - before);
		// Back in front: from behind, setting it is a write to the terminal, which the
		// leader has to be let do (SIGTTOU is caught here, so it is not stopped).
		before = came[SIGTTOU];
		long back = tcsetpgrp(s, me);
		say("the leader puts its own group back from behind: %s; SIGTTOU came %d\n", said(back), came[SIGTTOU] - before);
		signal(SIGTTOU, SIG_IGN);
		say("with SIGTTOU ignored: %s; in front is %s\n", said(tcsetpgrp(s, me)), who(tcgetpgrp(s), parent, me, other));
		tell(to[1]);
		wait_for(from[0]);
		tell(up[1]);
		wait_for(down[0]);

		// The first end closes.
		int hups = came[SIGHUP], conts = came[SIGCONT];
		tell(up[1]);
		say("the first end closed: SIGHUP came to the leader %d, SIGCONT %d", awaited(SIGHUP, hups), came[SIGCONT] - conts);
		char c;
		say("; it reads %s", said(read(s, &c, 1)));
		say(", writes %s", said(write(s, "x", 1)));
		say("; the group in front is still %s", who(tcgetpgrp(s), parent, me, other));
		int hung = open("/dev/tty", O_RDWR);
		say("; /dev/tty opened: %d", hung >= 0);
		say(", written: %s\n", said(write(hung, "x", 1)));
		int total = 0;
		for (int sig = 1; sig < 32; sig++)
			if (sig != SIGINT && sig != SIGQUIT && sig != SIGWINCH && sig != SIGHUP && sig != SIGTTOU && sig != SIGCONT)
				total += came[sig];
		say("other signals that came to the leader: %d\n", total);
		tell(to[1]);
		waitpid(other, NULL, 0);
		tell(up[1]);
		_exit(0);
	}
	close(down[0]);
	close(up[1]);
	wait_for(up[0]);
	say("the first end sees %s; asked there for the group in front: it is %s\n", took(m), who(tcgetpgrp(m), parent, child, 0));
	tell(down[1]);

	wait_for(up[0]);
	write(m, "abc\x03", 4);
	wait_for(up[0]);
	say("and the first end sees %s\n", took(m));
	tell(down[1]);
	wait_for(up[0]);
	write(m, "\x1c", 1);
	wait_for(up[0]);
	say("and the first end sees %s\n", took(m));
	tell(down[1]);

	wait_for(up[0]);
	struct winsize size = {24, 80, 0, 0};
	ioctl(m, TIOCSWINSZ, &size);
	wait_for(up[0]);
	tell(down[1]);

	wait_for(up[0]);
	write(m, "\x03", 1);
	wait_for(up[0]);
	took(m);
	tell(down[1]);

	wait_for(up[0]);
	close(m);
	wait_for(up[0]);
	int status = 0;
	waitpid(child, &status, 0);
	say("the leader ended: %d, with %d\n", WIFEXITED(status), WEXITSTATUS(status));
	rest();
	cases();
	return 0;
}
