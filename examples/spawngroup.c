// A program started into a process group that it is told by number (posix_spawn with
// POSIX_SPAWN_SETPGROUP): one that is there, one that is not, and another session's. And a file
// found again by its number (fsgetpath) by a program that never looked at it, having been told
// the number by the one that started it. It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach-o/dyld.h>
#include <signal.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/fsgetpath.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;

// started starts this program again to wait, into a group by its number: 0 is one of its own.
static int started(const char *self, pid_t group, pid_t *pid) {
	posix_spawnattr_t attr;
	posix_spawnattr_init(&attr);
	posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETPGROUP);
	posix_spawnattr_setpgroup(&attr, group);
	char *argv[] = {(char *)self, "wait", NULL};
	*pid = 0;
	int r = posix_spawn(pid, self, NULL, &attr, argv, environ);
	posix_spawnattr_destroy(&attr);
	return r;
}

static void done(pid_t pid) {
	if (pid > 0) {
		kill(pid, SIGKILL);
		waitpid(pid, NULL, 0);
	}
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char self[1024];
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	if (argc > 1 && !strcmp(argv[1], "wait"))
		for (;;)
			pause();
	if (argc > 3 && !strcmp(argv[1], "find")) { // a file by the numbers it was told, which it never looked at
		char found[1024] = "";
		fsid_t volume = {{atoi(argv[2]), 0}};
		ssize_t n = fsgetpath(found, sizeof found, &volume, strtoull(argv[3], NULL, 10));
		const char *name = strrchr(found, '/');
		printf("a program told a file's numbers finds it: %s\n", n > 0 ? (name && !strncmp(name, "/spawngroup.", 12) ? "the file" : "another") : strerror(errno));
		return 0;
	}
	if (argc == 1) { // what follows is done by the first of a group, whatever started this
		posix_spawnattr_t attr;
		posix_spawnattr_init(&attr);
		posix_spawnattr_setflags(&attr, POSIX_SPAWN_SETPGROUP);
		posix_spawnattr_setpgroup(&attr, 0);
		char *again[] = {self, "first", NULL};
		pid_t pid;
		int status = 0;
		if (posix_spawn(&pid, self, NULL, &attr, again, environ) == 0)
			waitpid(pid, &status, 0);
		return WEXITSTATUS(status);
	}

	pid_t me = getpid(), own, into, sibling, none, other, below;
	int r = started(self, 0, &own);
	printf("into a group of its own: %d, its group its own %d\n", r, getpgid(own) == own);
	r = started(self, own, &into);
	printf("into that one's group: %d, its group that %d\n", r, getpgid(into) == own);
	r = started(self, me, &sibling);
	printf("into its parent's by number: %d, its group its parent's %d\n", r, getpgid(sibling) == me);
	r = started(self, 99999, &none);
	printf("into a group there is not: %d (%s), a process started %d\n", r, strerror(r), none > 0);
	r = started(self, 1, &other);
	printf("into launchd's, which is another session's: %d (%s), a process started %d\n", r, strerror(r), other > 0);
	r = started(self, -5, &below);
	printf("told a group below zero: %d (%s)", r, strerror(r));
	if (r == 0)
		printf(", its group its parent's %d", getpgid(below) == me);
	printf("\n");
	done(own), done(into), done(sibling), done(none), done(other), done(below);

	char path[512], dev[24], ino[24];
	snprintf(path, sizeof path, "%s/spawngroup.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", me);
	close(open(path, O_CREAT | O_WRONLY, 0600));
	struct stat s;
	stat(path, &s);
	snprintf(dev, sizeof dev, "%d", s.st_dev);
	snprintf(ino, sizeof ino, "%llu", s.st_ino);
	char *find[] = {self, "find", dev, ino, NULL};
	pid_t pid;
	if (posix_spawn(&pid, self, NULL, NULL, find, environ) == 0)
		waitpid(pid, NULL, 0);
	unlink(path);
	return 0;
}
