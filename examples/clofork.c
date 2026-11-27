// A descriptor that a copy of the process does not get (O_CLOFORK, FD_CLOFORK), as SQLite opens
// its files: what fork, a duplicate, a program started and the program it becomes each do with
// it. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

// has says whether a descriptor is there.
static const char *has(int fd) {
	struct stat st;
	return fstat(fd, &st) == 0 ? "there" : errno == EBADF ? "not there" : strerror(errno);
}

int main(int argc, char **argv) {
	if (argc == 4) { // started, or become, to say what it was handed
		printf("%s: the one marked is %s, the plain one %s\n", argv[1], has(atoi(argv[2])), has(atoi(argv[3])));
		return 0;
	}
	int marked = open("/dev/null", O_RDONLY | O_CLOFORK), plain = open("/dev/null", O_RDONLY);
	printf("opened with O_CLOFORK: its flags %d; a plain one's %d\n", fcntl(marked, F_GETFD), fcntl(plain, F_GETFD));
	int later = open("/dev/null", O_RDONLY);
	printf("marked afterwards (FD_CLOFORK): %d, its flags %d; with close-on-exec too: %d, %d; unmarked: %d, %d\n", fcntl(later, F_SETFD, FD_CLOFORK), fcntl(later, F_GETFD), fcntl(later, F_SETFD, FD_CLOFORK | FD_CLOEXEC),
	       fcntl(later, F_GETFD), fcntl(later, F_SETFD, 0), fcntl(later, F_GETFD));
	int copy = dup(marked), again = fcntl(marked, F_DUPFD, 20), both = open("/dev/null", O_RDONLY | O_CLOFORK | O_CLOEXEC);
	printf("a duplicate of the marked one has flags %d, one made by fcntl %d; opened with both marks: %d\n", fcntl(copy, F_GETFD), fcntl(again, F_GETFD), fcntl(both, F_GETFD));
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		printf("in a copy of the process the marked one is %s, the plain one %s, the duplicate %s; the marked one's number is free: %d\n", has(marked), has(plain), has(copy), open("/dev/null", O_RDONLY) == marked);
		fflush(stdout);
		_exit(0);
	}
	waitpid(child, NULL, 0);
	printf("and in the process itself it is still %s\n", has(marked));
	fflush(stdout);
	char a[16], b[16];
	snprintf(a, sizeof a, "%d", marked);
	snprintf(b, sizeof b, "%d", plain);
	char *started[] = {argv[0], "a program it starts", a, b, NULL};
	pid_t pid;
	if (posix_spawn(&pid, argv[0], NULL, NULL, started, environ) == 0)
		waitpid(pid, NULL, 0);
	char *become[] = {argv[0], "the program it becomes", a, b, NULL};
	execv(argv[0], become);
	printf("execv: %s\n", strerror(errno));
	return 1;
}
