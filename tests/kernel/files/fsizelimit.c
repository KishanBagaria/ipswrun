// The limit on how large a file a process may make (RLIMIT_FSIZE, the shell's ulimit -f): a
// write that would pass it writes up to it, one that starts at or past it is refused and the
// process sent SIGXFSZ, which ends one that does not catch it; and what else is held to it.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/uio.h>
#include <sys/wait.h>
#include <unistd.h>

static volatile sig_atomic_t told;
static void tell(int sig) { told++; }

// did says what a call that writes came to, and whether the signal was sent for it.
static const char *did(ssize_t r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "%s%s", strerror(errno), told ? ", and the signal" : "");
	else
		snprintf(t, 64, "%zd%s", r, told ? ", and the signal" : "");
	told = 0;
	return t;
}

static long long size(int fd) {
	struct stat st;
	return fstat(fd, &st) ? -1 : (long long)st.st_size;
}

int main(void) {
	setvbuf(stdout, NULL, _IONBF, 0);
	char path[1024], other[1024], buf[4096];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/fsizelimit.%d", tmp, getpid());
	snprintf(other, sizeof other, "%s/fsizelimit.%d.b", tmp, getpid());
	memset(buf, 'x', sizeof buf);
	signal(SIGXFSZ, tell);

	struct rlimit was, limit;
	getrlimit(RLIMIT_FSIZE, &was);
	printf("the limit to start with: %s, at most %s\n", was.rlim_cur == RLIM_INFINITY ? "none" : "one", was.rlim_max == RLIM_INFINITY ? "none" : "one");
	limit = was;
	limit.rlim_cur = 1000;
	printf("set to 1000: %s\n", setrlimit(RLIMIT_FSIZE, &limit) ? strerror(errno) : "set");

	int fd = open(path, O_CREAT | O_RDWR | O_TRUNC, 0600);
	printf("600 bytes written: %s", did(write(fd, buf, 600)));
	printf("; 600 more: %s", did(write(fd, buf, 600)));
	printf("; and the file is of %lld; one more: %s", size(fd), did(write(fd, buf, 1)));
	printf("; none: %s\n", did(write(fd, buf, 0)));
	printf("written at 2000: %s", did(pwrite(fd, buf, 10, 2000)));
	printf("; at 999, two bytes: %s", did(pwrite(fd, buf, 2, 999)));
	printf("; at 10, inside what is there: %s", did(pwrite(fd, buf, 100, 10)));
	struct iovec parts[2] = {{buf, 300}, {buf, 300}};
	lseek(fd, 900, SEEK_SET);
	printf("; two pieces of 300 at 900: %s", did(writev(fd, parts, 2)));
	printf("; and the file is of %lld\n", size(fd));
	ftruncate(fd, 700);
	lseek(fd, 700, SEEK_SET);
	printf("two pieces of 300 at 700, the first ending at the limit: %s", did(writev(fd, parts, 2)));
	printf("; then two more: %s", did(writev(fd, parts, 2)));
	printf("; a descriptor that is none made 2000 long: %s", did(ftruncate(99, 2000)));
	printf("; and 10: %s", did(ftruncate(99, 10)));
	printf("; a name that is not there made 2000 long: %s", did(truncate("/nowhere/at/all", 2000)));
	printf("; a file of the system: %s", did(truncate("/System/Library/CoreServices/SystemVersion.plist", 2000)));
	printf("; and 10: %s\n", did(truncate("/System/Library/CoreServices/SystemVersion.plist", 10)));

	printf("made 2000 long: %s", did(ftruncate(fd, 2000)));
	printf("; 1000: %s", did(ftruncate(fd, 1000)));
	printf("; 1001: %s", did(ftruncate(fd, 1001)));
	printf("; by its name 3000: %s", did(truncate(path, 3000)));
	printf("; 500: %s", did(truncate(path, 500)));
	printf("; and the file is of %lld\n", size(fd));

	int added = open(path, O_WRONLY | O_APPEND);
	printf("added to at its end, 800 bytes: %s", did(write(added, buf, 800)));
	printf("; again: %s", did(write(added, buf, 800)));
	close(added);
	int pipes[2];
	pipe(pipes);
	printf("; 2000 into a pipe: %s", did(write(pipes[1], buf, 2000)));
	int null = open("/dev/null", O_WRONLY);
	printf("; into /dev/null: %s", did(write(null, buf, 2000)));
	lseek(null, 5000, SEEK_SET);
	printf("; and at 5000 of it: %s\n", did(write(null, buf, 10)));
	close(null);

	// A file that is larger already is read, and is written inside the limit and not past it.
	limit.rlim_cur = was.rlim_cur;
	setrlimit(RLIMIT_FSIZE, &limit);
	int big = open(other, O_CREAT | O_RDWR | O_TRUNC, 0600);
	write(big, buf, 3000);
	limit.rlim_cur = 1000;
	setrlimit(RLIMIT_FSIZE, &limit);
	printf("a file of 3000 from before: read at 2000: %s", did(pread(big, buf, 100, 2000)));
	printf("; written at 500: %s", did(pwrite(big, buf, 100, 500)));
	printf("; at 2000: %s", did(pwrite(big, buf, 100, 2000)));
	printf("; made 2500 long: %s", did(ftruncate(big, 2500)));
	printf("; 900: %s", did(ftruncate(big, 900)));
	printf("; and it is of %lld\n", size(big));

	// With a limit of nothing, and what a child that does not catch the signal comes to.
	limit.rlim_cur = 0;
	setrlimit(RLIMIT_FSIZE, &limit);
	ftruncate(fd, 0);
	told = 0;
	lseek(fd, 0, SEEK_SET);
	printf("with a limit of nothing, one byte: %s", did(write(fd, buf, 1)));
	printf("; made empty: %s", did(ftruncate(fd, 0)));
	limit.rlim_cur = 1000;
	setrlimit(RLIMIT_FSIZE, &limit);
	pid_t child = fork();
	if (child == 0) {
		signal(SIGXFSZ, SIG_DFL);
		pwrite(fd, buf, 10, 5000);
		_exit(3);
	}
	int status = 0;
	waitpid(child, &status, 0);
	printf("; a child that writes past it and does not catch the signal: ");
	if (WIFSIGNALED(status))
		printf("ended by signal %d", WTERMSIG(status));
	else
		printf("left with %d", WEXITSTATUS(status));
	child = fork();
	if (child == 0) {
		signal(SIGXFSZ, SIG_IGN);
		ssize_t r = pwrite(fd, buf, 10, 5000);
		_exit(r < 0 && errno == EFBIG ? 4 : 5);
	}
	waitpid(child, &status, 0);
	printf("; one that ignores it: %s\n", WIFEXITED(status) && WEXITSTATUS(status) == 4 ? "is refused, and goes on" : "something else");

	limit.rlim_cur = was.rlim_cur;
	printf("the limit put back: %s\n", setrlimit(RLIMIT_FSIZE, &limit) ? strerror(errno) : "set");
	close(fd);
	close(big);
	unlink(path);
	unlink(other);
	return 0;
}
