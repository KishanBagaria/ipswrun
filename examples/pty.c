// A pseudo-terminal: the two ends a terminal window, script or ssh has, the one the program at
// the keyboard's side holds and the one the program run in it reads and writes as its terminal,
// with the kernel's handling of a line between them. How one is made and named, what each end
// says it is, what it is set to at first, and what passes: a line at a time with its editing
// and its echo, a newline made a return and a newline on the way out, the end of input, and
// bytes as they are once it is told to leave them alone. It prints the same natively on a Mac
// as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/event.h>
#include <sys/ioctl.h>
#include <sys/select.h>
#include <sys/stat.h>
#include <sys/time.h>
#include <sys/wait.h>
#include <termios.h>
#include <unistd.h>

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

// shown is bytes as they can be printed: a return as \r, a newline as \n, others below a space
// as ^ and a letter.
static const char *shown(const char *bytes, long n) {
	static char text[4][256];
	static int next;
	char *t = text[next++ % 4], *at = t;
	for (long i = 0; i < n && at < t + 240; i++) {
		unsigned char c = (unsigned char)bytes[i];
		if (c == '\r')
			at += sprintf(at, "\\r");
		else if (c == '\n')
			at += sprintf(at, "\\n");
		else if (c < 32 || c == 127)
			at += sprintf(at, "^%c", c == 127 ? '?' : c + 64);
		else
			*at++ = (char)c;
	}
	*at = 0;
	return t;
}

static int ready(int fd, int what) {
	struct pollfd p = {fd, (short)what, 0};
	return poll(&p, 1, 0) < 0 ? -1 : p.revents;
}

// watched says what select and a kqueue find of an end: whether it can be read and written, and
// the event of each kind with its flags and its count.
static const char *watched(int fd) {
	static char text[4][200];
	static int next;
	char *t = text[next++ % 4];
	fd_set in, out;
	FD_ZERO(&in);
	FD_ZERO(&out);
	FD_SET(fd, &in);
	FD_SET(fd, &out);
	struct timeval none = {0, 0};
	int found = select(fd + 1, &in, &out, NULL, &none);
	int at = snprintf(t, 200, "select %d%s%s", found, found > 0 && FD_ISSET(fd, &in) ? " read" : "", found > 0 && FD_ISSET(fd, &out) ? " write" : "");
	int q = kqueue();
	struct kevent ask[2], got[2];
	EV_SET(&ask[0], fd, EVFILT_READ, EV_ADD, 0, 0, NULL);
	EV_SET(&ask[1], fd, EVFILT_WRITE, EV_ADD, 0, 0, NULL);
	struct timespec no = {0, 0};
	int n = kevent(q, ask, 2, got, 2, &no);
	if (n < 0)
		at += snprintf(t + at, 200 - at, ", kevent -1 (%s)", strerror(errno));
	for (int i = 0; i < n; i++)
		at += snprintf(t + at, 200 - at, ", %s flags %#x count %ld", got[i].filter == EVFILT_READ ? "read" : "write", got[i].flags, (long)got[i].data);
	close(q);
	return t;
}

// took reads what is there without waiting and says it.
static const char *took(int fd) {
	static char text[4][300];
	static int next;
	char *t = text[next++ % 4], bytes[200];
	int flags = fcntl(fd, F_GETFL);
	fcntl(fd, F_SETFL, flags | O_NONBLOCK);
	ssize_t n = read(fd, bytes, sizeof bytes);
	int e = errno;
	fcntl(fd, F_SETFL, flags);
	if (n < 0)
		snprintf(t, 300, "-1 (%s)", strerror(e));
	else
		snprintf(t, 300, "%zd \"%s\"", n, shown(bytes, n));
	return t;
}

static double now(void) {
	struct timeval v;
	gettimeofday(&v, NULL);
	return (double)v.tv_sec + v.tv_usec / 1e6;
}

// rest is the rest of it, on a terminal of its own: an end opened one way only, the end of
// input as a byte that waits, what the editing of a line shows once the program has written,
// the keys of the extended editing, lines turned off with something typed, and what closing
// either end lets go.
static void rest(void) {
	struct termios t, d;
	struct winsize set = {24, 80, 0, 0}, size;
	int n = -1;
	char b[64], c;
	int m = posix_openpt(O_RDWR | O_NOCTTY);
	grantpt(m);
	unlockpt(m);
	char name[128];
	ioctl(m, TIOCPTYGNAME, name);
	int s = open(name, O_RDWR | O_NOCTTY);
	tcgetattr(s, &d);
	int one = open(name, O_RDONLY | O_NOCTTY);
	printf("the second end opened to read only: %d, written: %s", one >= 0, said(write(one, "q", 1)));
	close(one);
	one = open(name, O_WRONLY | O_NOCTTY);
	printf("; to write only: %d, read: %s\n", one >= 0, said(read(one, b, 1)));
	close(one);

	// The end of input waits with what it ended.
	write(m, "part\x04", 5);
	ioctl(s, FIONREAD, &n);
	printf("some and the end of input typed: %d wait; a read of 4: %s", n, said(read(s, b, 4)));
	printf(", and the next: %s\n", took(s));
	write(m, "\x04", 1);
	ioctl(s, FIONREAD, &n);
	printf("the end alone: %d waits, the second can be read %#x, reads %s\n", n, ready(s, POLLIN), took(s));
	write(m, "abcdef\n", 7);
	printf("part of a line read: %s", said(read(s, b, 3)));
	ioctl(s, FIONREAD, &n);
	printf("; %d wait, and the rest is %s\n", n, took(s));
	took(m);

	// What waits at the first end is let go when the settings are set there.
	write(s, "out", 3);
	tcgetattr(m, &t);
	printf("settings set at the first end with something for it to read: %s; it reads %s\n", said(tcsetattr(m, TCSANOW, &t)), took(m));

	// Editing once the second end has written between.
	write(m, "ab", 2);
	write(s, "x", 1);
	write(m, "\x7f", 1);
	printf("a letter rubbed out after the second wrote: the first sees %s", took(m));
	write(m, "\n", 1);
	printf("; the second reads %s\n", took(s));
	took(m);
	write(m, "ab", 2);
	write(s, "x", 1);
	write(m, "\x15", 1);
	printf("a line killed after the second wrote: the first sees %s", took(m));
	write(m, "z\n", 2);
	printf("; the second reads %s\n", took(s));
	took(m);
	write(m, "a\x01\x7f\x7f\x7f" "b\n", 7);
	printf("a control typed and rubbed out, and more than there is: the first sees %s; the second reads %s\n", took(m), took(s));

	// The keys of the extended editing.
	write(m, "\x16", 1);
	printf("the key that takes the next as it is: the first sees %s", took(m));
	write(m, "\x03\n", 2);
	printf("; then the interrupt and a newline: %s; the second reads %s\n", took(m), took(s));
	write(m, "one two \x17\n", 10);
	printf("a word rubbed out: the first sees %s; the second reads %s\n", took(m), took(s));
	write(m, "ab\x12", 3);
	printf("the line asked for again: the first sees %s", took(m));
	write(m, "\n", 1);
	printf("; the second reads %s\n", took(s));
	took(m);

	// Lines off.
	t = d;
	t.c_lflag &= ~(tcflag_t)(ICANON | ECHO);
	tcsetattr(s, TCSANOW, &t);
	write(m, "a\x16" "b\x7f\x04", 5);
	printf("with no lines and no echo, the extended keys still on: the second reads %s; the first sees %s\n", took(s), took(m));
	tcsetattr(s, TCSANOW, &d);
	write(m, "l1\nl2", 5);
	t = d;
	t.c_lflag &= ~(tcflag_t)ICANON;
	printf("lines turned off with a line and part of one typed: %s", said(tcsetattr(s, TCSANOW, &t)));
	printf("; the second reads %s\n", took(s));
	tcsetattr(s, TCSANOW, &d);
	took(m);

	// Opened again.
	close(s);
	printf("the size set with the second end closed: %s", said(ioctl(m, TIOCSWINSZ, &set)));
	s = open(name, O_RDWR | O_NOCTTY);
	ioctl(s, TIOCGWINSZ, &size);
	printf("; opened again it is %d by %d\n", size.ws_row, size.ws_col);
	set.ws_row = 30;
	ioctl(s, TIOCSWINSZ, &set);
	t = d;
	t.c_lflag &= ~(tcflag_t)ECHO;
	tcsetattr(s, TCSANOW, &t);
	int again = open(name, O_RDWR | O_NOCTTY);
	tcgetattr(again, &t);
	ioctl(again, TIOCGWINSZ, &size);
	printf("opened a second time while open: the echo still off %d, %d rows", !(t.c_lflag & ECHO), size.ws_row);
	close(again);
	close(s);
	s = open(name, O_RDWR | O_NOCTTY);
	tcgetattr(s, &t);
	printf("; closed and opened: the echo on %d\n", !!(t.c_lflag & ECHO));

	// What closing lets go. The last close of the second end waits a while for what it
	// wrote to be read, and so does the end of a process that had it open.
	double at = now();
	write(s, "kept?", 5);
	close(s);
	double took_ = now() - at;
	printf("the second closed with what it wrote unread: the close took half a second or more %d, under two %d; the first reads %s\n", took_ >= 0.5, took_ < 2, took(m));
	pid_t child = fork();
	if (child == 0) {
		int k = open(name, O_RDWR | O_NOCTTY);
		write(k, "hi\n", 3);
		_exit(0);
	}
	waitpid(child, NULL, 0);
	printf("a child wrote and ended, and was waited for: the first reads %s\n", took(m));
	child = fork();
	if (child == 0) {
		int k = open(name, O_RDWR | O_NOCTTY);
		write(k, "hi\n", 3);
		_exit(0);
	}
	char seen[16];
	ssize_t got = read(m, seen, sizeof seen);
	waitpid(child, NULL, 0);
	printf("a child wrote and ended while the first end read: %zd \"%s\"\n", got, shown(seen, got));
	s = open(name, O_RDWR | O_NOCTTY);
	write(m, "left\n", 5);
	took(m);
	close(m);
	ioctl(s, FIONREAD, &n);
	printf("the first closed with a line typed: %d wait, and the second reads %s\n", n, said(read(s, &c, 1)));
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGHUP, SIG_IGN);
	int m = posix_openpt(O_RDWR | O_NOCTTY);
	// The other end is root's, and not to be opened, until it is granted and then unlocked.
	char early[128] = "";
	struct stat se;
	memset(&se, 0, sizeof se);
	ioctl(m, TIOCPTYGNAME, early);
	printf("made: %d; the other end at first: %s, mode %o, root's %d, of group %d", m >= 0, said(stat(early, &se)), (unsigned)se.st_mode, se.st_uid == 0, (int)se.st_gid);
	printf("; opened: %s\n", said(open(early, O_RDWR | O_NOCTTY | O_NONBLOCK)));
	printf("granted: %s", said(grantpt(m)));
	stat(early, &se);
	printf("; then the user's %d, mode %o; opened before it is unlocked: %s", se.st_uid == getuid(), (unsigned)se.st_mode, said(open(early, O_RDWR | O_NOCTTY | O_NONBLOCK)));
	printf("; unlocked: %s", said(unlockpt(m)));
	const char *name = ptsname(m);
	printf("; the other end is named as a terminal of /dev %d\n", name && !strncmp(name, "/dev/ttys", 9));
	struct termios none;
	struct winsize nosize = {1, 1, 0, 0};
	int nothing;
	printf("asked of the first end before the other is opened: its settings %s", said(tcgetattr(m, &none)));
	printf(", its size %s", said(ioctl(m, TIOCGWINSZ, &nosize)));
	printf(", to set it %s", said(ioctl(m, TIOCSWINSZ, &nosize)));
	printf(", what waits %s\n", said(ioctl(m, FIONREAD, &nothing)));
	// Typing at it then waits for the other end, so it is done without waiting.
	printf("before the other end is opened, the first: can be read %#x, written %#x; reads %s", ready(m, POLLIN), ready(m, POLLOUT), took(m));
	fcntl(m, F_SETFL, O_RDWR | O_NONBLOCK);
	printf("; typed at, not waiting: %s; watched: %s\n", said(write(m, "x\n", 2)), watched(m));
	fcntl(m, F_SETFL, O_RDWR);
	int s = open(name, O_RDWR | O_NOCTTY);
	struct stat sm, ss;
	fstat(m, &sm);
	fstat(s, &ss);
	const char *called = ttyname(s);
	printf("the first end: mode %o, root's %d, of group %d; its number in its kind the second's: %d, its kind another: %d\n", (unsigned)sm.st_mode, sm.st_uid == 0, (int)sm.st_gid, minor(sm.st_rdev) == minor(ss.st_rdev),
	       major(sm.st_rdev) != major(ss.st_rdev));
	printf("opened: %d; each a terminal: %d %d; each a device of characters: %d %d; the second's name its own: %d; a file is one: %d\n", s >= 0, isatty(m), isatty(s), S_ISCHR(sm.st_mode), S_ISCHR(ss.st_mode),
	       called && !strcmp(called, name), isatty(open("/dev/null", O_RDONLY)));

	// What it is set to at first.
	struct termios t;
	printf("its settings read: %s", said(tcgetattr(s, &t)));
	printf("; input %#lx, output %#lx, control %#lx, local %#lx; speeds %lu %lu\n", (unsigned long)t.c_iflag, (unsigned long)t.c_oflag, (unsigned long)t.c_cflag, (unsigned long)t.c_lflag, (unsigned long)cfgetispeed(&t),
	       (unsigned long)cfgetospeed(&t));
	printf("its characters: interrupt ^%c, quit ^%c, erase %#x, kill ^%c, end ^%c, suspend ^%c, least %d, time %d\n", t.c_cc[VINTR] + 64, t.c_cc[VQUIT] + 64, t.c_cc[VERASE], t.c_cc[VKILL] + 64, t.c_cc[VEOF] + 64, t.c_cc[VSUSP] + 64,
	       t.c_cc[VMIN], t.c_cc[VTIME]);
	struct winsize size = {7, 7, 7, 7};
	printf("its size: %s, %d by %d", said(ioctl(s, TIOCGWINSZ, &size)), size.ws_row, size.ws_col);
	struct winsize set = {24, 80, 0, 0};
	printf("; set to 24 by 80 at the first end: %s", said(ioctl(m, TIOCSWINSZ, &set)));
	ioctl(s, TIOCGWINSZ, &size);
	printf(", and the second says %d by %d\n", size.ws_row, size.ws_col);

	// A line at a time.
	printf("nothing typed: the second can be read %#x, written %#x; the first read %#x, written %#x\n", ready(s, POLLIN), ready(s, POLLOUT), ready(m, POLLIN), ready(m, POLLOUT));
	printf("watched, nothing typed: the second: %s; the first: %s\n", watched(s), watched(m));
	printf("\"ab\" typed: %s; the second can be read %#x, reads %s", said(write(m, "ab", 2)), ready(s, POLLIN), took(s));
	printf("; the first sees %s\n", took(m));
	int waiting = -1;
	printf("\"c\" and a newline typed: %s; the second can be read %#x, has %s %d waiting", said(write(m, "c\n", 2)), ready(s, POLLIN), said(ioctl(s, FIONREAD, &waiting)), waiting);
	printf("; watched, the second: %s; the first: %s\n", watched(s), watched(m));
	printf("then the second reads %s; the first sees %s\n", took(s), took(m));
	write(m, "one\ntwo\n", 8);
	printf("two lines typed at once: the second reads %s", took(s));
	printf(", then %s; the first sees %s\n", took(s), took(m));
	write(m, "abX\x7f" "d\n", 6);
	printf("a letter typed and rubbed out: the second reads %s; the first sees %s\n", took(s), took(m));
	write(m, "gone\x15kept\n", 10);
	printf("a line killed and typed again: the second reads %s; the first sees %s\n", took(s), took(m));
	write(m, "part\x04", 5);
	printf("some and the end of input: the second reads %s", took(s));
	write(m, "\x04", 1);
	printf("; the end alone: %s; the first sees %s\n", took(s), took(m));
	write(m, "a\rb\n", 4);
	printf("a return typed: the second reads %s", took(s));
	printf(", then %s; the first sees %s\n", took(s), took(m));

	// On the way out.
	printf("\"out\" and a newline written by the second: %s; the first sees %s\n", said(write(s, "out\n", 4)), took(m));
	printf("a tab and a bell: %s; the first sees %s\n", said(write(s, "\t\a", 2)), took(m));

	// More than the terminal holds on the way out: a write that does not wait takes what there
	// is room for, and one that waits ends when the first end has read it all.
	static char much[3000];
	memset(much, 'z', sizeof much);
	much[1499] = '\n';
	int was = fcntl(s, F_GETFL);
	fcntl(s, F_SETFL, was | O_NONBLOCK);
	ssize_t fit = write(s, much, sizeof much);
	printf("3000 bytes written by the second, not waiting: %s", said(fit));
	printf(", again: %s; watched: %s", said(write(s, much, 1)), watched(s));
	fcntl(s, F_SETFL, was);
	int held = -1, more;
	ioctl(m, FIONREAD, &held);
	long drained = 0, n;
	char sink[4096];
	fcntl(m, F_SETFL, O_RDWR | O_NONBLOCK);
	n = read(m, sink, sizeof sink);
	printf("; the first has %d, reads %ld at once", held, n);
	for (drained = n > 0 ? n : 0; (n = read(m, sink, sizeof sink)) > 0;)
		drained += n;
	fcntl(m, F_SETFL, O_RDWR);
	printf(", %ld in all\n", drained);
	// How much may wait and the second still be writable; and a newline where there is room
	// for one byte more.
	write(s, much, 256);
	printf("with 256 waiting the second: %s", watched(s));
	write(s, much, 1);
	printf("; with 257: %s, can be written %#x\n", watched(s), ready(s, POLLOUT));
	while (read(m, sink, sizeof sink) == sizeof sink)
		;
	fcntl(s, F_SETFL, was | O_NONBLOCK);
	much[1023] = '\n';
	fit = write(s, much, 1100);
	printf("a newline as the 1024th byte, not waiting: %s", said(fit));
	n = read(m, sink, sizeof sink);
	printf("; the first reads %ld, ending %s", n, n > 2 ? shown(sink + n - 2, 2) : "");
	printf("; the newline again: %s, and the first sees %s\n", said(write(s, "\n", 1)), took(m));
	fcntl(s, F_SETFL, was);
	much[1023] = 'z';
	pid_t reader = fork();
	if (reader == 0) {
		long all = 0;
		while (all < 3001 && (n = read(m, sink, sizeof sink)) > 0)
			all += n;
		_exit(all == 3001 ? 0 : 1);
	}
	fit = write(s, much, sizeof much);
	waitpid(reader, &more, 0);
	printf("3000 bytes written, waiting, with the first end reading: %s; it read them all, with the return added: %d\n", said(fit), WIFEXITED(more) && WEXITSTATUS(more) == 0);

	// Left alone.
	struct termios raw = t;
	cfmakeraw(&raw);
	printf("told to leave bytes alone: %s", said(tcsetattr(s, TCSANOW, &raw)));
	tcgetattr(m, &raw);
	printf("; the first end says of it input %#lx, output %#lx, local %#lx, least %d\n", (unsigned long)raw.c_iflag, (unsigned long)raw.c_oflag, (unsigned long)raw.c_lflag, raw.c_cc[VMIN]);
	write(m, "x\x7f\x03\r", 4);
	printf("typed then: the second can be read %#x, reads %s; the first sees %s\n", ready(s, POLLIN), took(s), took(m));
	write(s, "raw\n", 4);
	printf("written by the second: the first sees %s\n", took(m));
	printf("set back: %s\n", said(tcsetattr(s, TCSAFLUSH, &t)));

	// An end closed.
	write(m, "left\n", 5);
	close(s);
	printf("the second closed with a line unread: the first sees %s, can be read %#x, written %#x", took(m), ready(m, POLLIN), ready(m, POLLOUT));
	printf("; typed at: %s; watched: %s\n", said(write(m, "x", 1)), watched(m));
	s = open(name, O_RDWR | O_NOCTTY);
	printf("opened again: %d; reads %s\n", s >= 0, took(s));
	// A read that waits is woken by what is typed, here by a child a moment later.
	pid_t child = fork();
	if (child == 0) {
		usleep(50000);
		write(m, "late\n", 5);
		_exit(0);
	}
	char line[16];
	ssize_t got = read(s, line, sizeof line);
	waitpid(child, NULL, 0);
	printf("a read that waited: %zd \"%s\"; the first sees %s\n", got, shown(line, got), took(m));
	close(m);
	char c;
	printf("the first closed: the second reads %s, writes %s, can be read %#x; its settings %s\n", said(read(s, &c, 1)), said(write(s, "x", 1)), ready(s, POLLIN), said(tcgetattr(s, &t)));
	printf("watched then: %s\n", watched(s));
	printf("its name opened again, not waiting: %d\n", open(name, O_RDWR | O_NOCTTY | O_NONBLOCK) >= 0);
	rest();
	return 0;
}
