// fork: a process makes a copy of itself, which goes on from the same place with memory of its
// own, the same open files, and the same handlers, and is told apart by what fork answers. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach-o/dyld.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

static int global = 1;

static const char *ended(pid_t pid) {
	static char text[64];
	int status = -1;
	if (waitpid(pid, &status, 0) != pid)
		snprintf(text, sizeof text, "waitpid: %s", strerror(errno));
	else if (WIFEXITED(status))
		snprintf(text, sizeof text, "exited %d", WEXITSTATUS(status));
	else
		snprintf(text, sizeof text, "ended by signal %d", WTERMSIG(status));
	return text;
}

static void handler(int sig) { printf("  child: its parent's handler, for signal %d\n", sig); }

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1) {
		printf("  child: now another program, told \"%s\"\n", argv[1]);
		return 6;
	}
	pid_t parent = getpid();
	int *heap = malloc(sizeof *heap);
	*heap = 10;

	// A copy, with memory of its own.
	pid_t pid = fork();
	if (pid == 0) {
		global++;
		(*heap)++;
		printf("  child: fork answered 0; a process of its own: %d; its parent is the one that forked: %d; its memory: %d and %d\n", getpid() != parent, getppid() == parent,
		       global, *heap);
		_exit(3);
	}
	printf("fork answered the child's pid: %d; it %s; the parent's memory is as it was: %d and %d\n", pid > 0 && pid != parent, ended(pid), global, *heap);

	// The same open files: a pipe's ends, and a file's place.
	int p[2];
	char buf[64];
	pipe(p);
	if ((pid = fork()) == 0) {
		close(p[0]);
		write(p[1], "from the copy", 13);
		_exit(0);
	}
	close(p[1]);
	ssize_t n = read(p[0], buf, sizeof buf);
	printf("read from it: %zd \"%.*s\"", n, n > 0 ? (int)n : 0, buf);
	printf(", then %zd; it %s\n", read(p[0], buf, sizeof buf), ended(pid));
	close(p[0]);
	char path[1024];
	snprintf(path, sizeof path, "%s/fork.tmp", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp");
	int file = open(path, O_RDWR | O_CREAT | O_TRUNC, 0600);
	if ((pid = fork()) == 0) {
		write(file, "child,", 6);
		_exit(0);
	}
	ended(pid);
	write(file, "parent", 6);
	n = pread(file, buf, sizeof buf, 0);
	printf("a file both wrote to, one after the other: \"%.*s\"\n", (int)n, buf);
	close(file);
	unlink(path);

	// The same handlers, and what is ignored.
	signal(SIGUSR1, handler);
	if ((pid = fork()) == 0) {
		raise(SIGUSR1);
		_exit(4);
	}
	printf("it %s\n", ended(pid));

	// And then another program, which is how a shell runs one.
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	if ((pid = fork()) == 0) {
		char *again[] = {self, "by its copy", NULL};
		execv(self, again);
		_exit(127);
	}
	printf("it %s\n", ended(pid));

	// One that ends without leaving, and one its parent ends.
	if ((pid = fork()) == 0)
		abort();
	printf("one that aborts: it %s\n", ended(pid));
	if ((pid = fork()) == 0) {
		pause();
		_exit(0);
	}
	usleep(30000);
	kill(pid, SIGTERM);
	printf("one that waits, and is ended: it %s\n", ended(pid));
	printf("exit runs what the parent set up, in a copy too: ");
	fflush(stdout);
	if ((pid = fork()) == 0) {
		printf("its output is written as it leaves");
		exit(5);
	}
	const char *how = ended(pid);
	printf("; it %s\n", how);
	return 0;
}
