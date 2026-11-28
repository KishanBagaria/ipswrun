// A named pipe (mkfifo), as a script that feeds one program's output to another by a name has
// it: what opening each end says and waits for, with and without the other end and with and
// without waiting, and that it is then a pipe between whoever opened it: what is written is
// read, the end comes when the writers have gone, and it starts again with the next. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/stat.h>
#include <sys/wait.h>
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

static int readable(int fd) {
	struct pollfd p = {fd, POLLIN, 0};
	int n = poll(&p, 1, 0);
	return n < 0 ? -1 : p.revents;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	char path[512], got[64];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/fifo.%d", tmp, getpid());
	printf("made: %s; made again: %s", said(mkfifo(path, 0644)), said(mkfifo(path, 0644)));
	struct stat st;
	stat(path, &st);
	printf("; a named pipe %d, mode %o, size %lld\n", S_ISFIFO(st.st_mode), st.st_mode & 07777, (long long)st.st_size);

	// Without waiting, and with nobody at the other end.
	int w = open(path, O_WRONLY | O_NONBLOCK);
	printf("opened to write, not waiting, with no reader: %s\n", said(w));
	int r = open(path, O_RDONLY | O_NONBLOCK);
	printf("opened to read, not waiting, with no writer: %d; read from: %s; poll says %#x\n", r >= 0, said(read(r, got, sizeof got)), readable(r));
	w = open(path, O_WRONLY | O_NONBLOCK);
	printf("opened to write, not waiting, with a reader: %d; the reader's read then: %s; poll says %#x\n", w >= 0, said(read(r, got, sizeof got)), readable(r));
	printf("5 written: %s; poll says %#x", said(write(w, "hello", 5)), readable(r));
	fstat(r, &st);
	printf("; the open end a named pipe %d, holding %lld", S_ISFIFO(st.st_mode), (long long)st.st_size);
	memset(got, 0, sizeof got);
	printf("; read: %s \"%s\"; moved in: %s\n", said(read(r, got, sizeof got)), got, said(lseek(r, 0, SEEK_CUR)));
	int both = open(path, O_RDWR);
	printf("opened to read and write, waiting: %d; a second writer: %d\n", both >= 0, open(path, O_WRONLY) >= 0);
	close(both);

	// The writers gone, and one come again.
	write(w, "last", 4);
	close(w);
	int extra = 5;
	for (int fd = 3; fd < 16; fd++)
		if (fd != r && fcntl(fd, F_GETFL) != -1 && (fcntl(fd, F_GETFL) & O_ACCMODE) == O_WRONLY)
			close(fd), extra = fd;
	(void)extra;
	memset(got, 0, sizeof got);
	printf("written and every writer closed: read %s \"%s\"", said(read(r, got, sizeof got)), got);
	printf("; again %s; poll says %#x\n", said(read(r, got, sizeof got)), readable(r));
	w = open(path, O_WRONLY | O_NONBLOCK);
	write(w, "more", 4);
	memset(got, 0, sizeof got);
	printf("a writer come again: read %s \"%s\"\n", said(read(r, got, sizeof got)), got);

	// The reader gone.
	close(r);
	printf("the reader closed: written %s", said(write(w, "x", 1)));
	printf("; another writer opened, not waiting: %s\n", said(open(path, O_WRONLY | O_NONBLOCK)));
	write(w, "x", 1);
	close(w);
	r = open(path, O_RDONLY | O_NONBLOCK);
	printf("all closed with something written first, and opened to read again: read %s\n", said(read(r, got, sizeof got)));
	close(r);

	// How much one holds, and what an end opened both ways says of itself.
	static char much[100000];
	int rw = open(path, O_RDWR | O_NONBLOCK);
	struct stat named, open_;
	stat(path, &named);
	fstat(rw, &open_);
	printf("opened both ways, not waiting: its flags say %#x; the same file as its name %d, with %d names\n", fcntl(rw, F_GETFL) & (O_ACCMODE | O_NONBLOCK), named.st_ino == open_.st_ino, (int)open_.st_nlink);
	printf("100000 written without waiting: %s", said(write(rw, much, sizeof much)));
	int waiting = -1;
	struct pollfd each = {rw, POLLIN | POLLOUT, 0};
	poll(&each, 1, 0);
	printf("; said to be there to read: %s, %d; poll for both says %#x", said(ioctl(rw, FIONREAD, &waiting)), waiting, each.revents);
	printf("; one byte more: %s\n", said(write(rw, much, 1)));
	printf("192 read: %s; then 1000 written: %s", said(read(rw, much, 192)), said(write(rw, much, 1000)));
	printf("; 400 more read: %s; then 1000 written: %s", said(read(rw, much, 400)), said(write(rw, much, 1000)));
	each.revents = 0;
	poll(&each, 1, 0);
	printf("; poll for both says %#x\n", each.revents);
	close(rw);
	printf("made anew by its name, only if it is not there: %s\n", said(open(path, O_CREAT | O_EXCL | O_RDONLY | O_NONBLOCK, 0644)));

	// Waiting: each end opened waits for the other, in another process.
	int ends[2];
	pipe(ends);
	pid_t child = fork();
	if (child == 0) {
		int mine = open(path, O_WRONLY); // waits for a reader
		write(ends[1], "o", 1);
		write(mine, "from the writer", 15);
		close(mine);
		_exit(0);
	}
	usleep(200000);
	struct pollfd opened = {ends[0], POLLIN, 0};
	printf("another process opens to write and waits: it has opened %d", poll(&opened, 1, 0));
	r = open(path, O_RDONLY);
	char c;
	read(ends[0], &c, 1);
	memset(got, 0, sizeof got);
	ssize_t n = 0, k;
	while ((k = read(r, got + n, sizeof got - 1 - n)) > 0)
		n += k;
	printf("; once this one opens to read, it has, and wrote %zd \"%s\", then the end %zd\n", n, got, k);
	waitpid(child, NULL, 0);
	close(r);
	pipe(ends);
	child = fork();
	if (child == 0) {
		int mine = open(path, O_RDONLY); // waits for a writer
		char text[32] = "";
		ssize_t m = read(mine, text, sizeof text - 1);
		char told[160];
		write(ends[1], told, snprintf(told, sizeof told, "another process that waited to read was given %zd \"%s\"\n", m, text));
		_exit(0);
	}
	usleep(200000);
	w = open(path, O_WRONLY);
	write(w, "to the reader", 13);
	char line[160];
	n = read(ends[0], line, sizeof line);
	fwrite(line, 1, n > 0 ? n : 0, stdout);
	waitpid(child, NULL, 0);
	close(w);

	// The name removed with an end open, and another name for the same.
	r = open(path, O_RDONLY | O_NONBLOCK);
	w = open(path, O_WRONLY);
	printf("the name removed with both ends open: %s", said(unlink(path)));
	write(w, "still", 5);
	memset(got, 0, sizeof got);
	printf("; written and read: %s \"%s\"; opened again by the name: %s\n", said(read(r, got, sizeof got)), got, said(open(path, O_RDONLY | O_NONBLOCK)));
	return 0;
}
