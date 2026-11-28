// A process starting others with posix_spawn: what the child is given (arguments, environment,
// descriptors, the directory it is in), what it is told about itself, and what its parent learns
// when it ends. The child is this same program, given a role to play. It prints the same natively
// on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <mach-o/dyld.h>
#include <spawn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;
static char self[1024];

// child is what this program does when it was started by itself.
static int child(int argc, char **argv) {
	const char *role = argv[1];
	if (!strcmp(role, "exit"))
		return atoi(argv[2]);
	if (!strcmp(role, "say")) {
		printf("child: %s\n", argv[2]);
		return 0;
	}
	if (!strcmp(role, "about")) {
		const char *given = getenv("SPAWN_GIVEN");
		printf("child: %d arguments, the last \"%s\"; SPAWN_GIVEN is %s; its parent is the one that started it: %d\n", argc, argv[argc - 1], given ? given : "not set",
		       getppid() == atoi(argv[2]));
		return 0;
	}
	if (!strcmp(role, "shout")) { // what it reads, in capitals, to the end
		char buf[256];
		ssize_t n;
		while ((n = read(0, buf, sizeof buf)) > 0) {
			for (ssize_t i = 0; i < n; i++)
				if (buf[i] >= 'a' && buf[i] <= 'z')
					buf[i] -= 32;
			write(1, buf, n);
		}
		return 0;
	}
	if (!strcmp(role, "open")) { // whether a descriptor is open
		int fd = atoi(argv[2]);
		printf("child: descriptor %d is %s\n", fd, fcntl(fd, F_GETFD) < 0 ? "closed" : "open");
		return 0;
	}
	if (!strcmp(role, "where")) {
		char cwd[1024] = "";
		getcwd(cwd, sizeof cwd);
		const char *last = strrchr(cwd, '/');
		printf("child: in %s\n", last ? last + 1 : cwd);
		return 0;
	}
	if (!strcmp(role, "wait")) {
		usleep(atoi(argv[2]));
		return 3;
	}
	return 99;
}

// start starts this program in a role, and returns what posix_spawn answered.
static int start(pid_t *pid, const posix_spawn_file_actions_t *actions, const posix_spawnattr_t *attr, const char *role, const char *arg) {
	char *argv[] = {self, (char *)role, (char *)arg, NULL};
	return posix_spawn(pid, self, actions, attr, argv, environ);
}

// ended waits for a child and says how it ended.
static const char *ended(pid_t pid) {
	static char text[64];
	int status = -1;
	pid_t got = waitpid(pid, &status, 0);
	if (got != pid)
		snprintf(text, sizeof text, "waitpid: %s", strerror(errno));
	else if (WIFEXITED(status))
		snprintf(text, sizeof text, "exited %d", WEXITSTATUS(status));
	else
		snprintf(text, sizeof text, "status %#x", status);
	return text;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc > 1)
		return child(argc, argv);
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	pid_t pid = 0;
	char number[32];

	int rc = start(&pid, NULL, NULL, "exit", "7");
	printf("a child that exits 7: posix_spawn %d, a pid of its own: %d, %s\n", rc, pid > 0 && pid != getpid(), ended(pid));
	rc = start(&pid, NULL, NULL, "say", "hello, on the descriptors it was started with");
	printf("it %s\n", ended(pid));
	setenv("SPAWN_GIVEN", "the parent's", 1);
	snprintf(number, sizeof number, "%d", getpid());
	char *about[] = {self, "about", number, "one more", NULL};
	posix_spawn(&pid, self, NULL, NULL, about, environ);
	printf("it %s\n", ended(pid));
	char *bare[] = {"SPAWN_OTHER=1", NULL};
	posix_spawn(&pid, self, NULL, NULL, about, bare);
	printf("with another environment, it %s\n", ended(pid));

	// A child that writes into a pipe its parent reads, and one that reads from a pipe too.
	int out[2], in[2];
	char buf[256];
	posix_spawn_file_actions_t actions;
	pipe(out);
	posix_spawn_file_actions_init(&actions);
	posix_spawn_file_actions_adddup2(&actions, out[1], 1);
	posix_spawn_file_actions_addclose(&actions, out[0]);
	posix_spawn_file_actions_addclose(&actions, out[1]);
	rc = start(&pid, &actions, NULL, "say", "into a pipe");
	posix_spawn_file_actions_destroy(&actions);
	close(out[1]);
	ssize_t n = read(out[0], buf, sizeof buf);
	printf("read from the child: %zd \"%.*s\"", n, n > 0 ? (int)n - 1 : 0, buf);
	printf(", then %zd; it %s\n", read(out[0], buf, sizeof buf), ended(pid));
	close(out[0]);

	pipe(out);
	pipe(in);
	posix_spawn_file_actions_init(&actions);
	posix_spawn_file_actions_adddup2(&actions, in[0], 0);
	posix_spawn_file_actions_adddup2(&actions, out[1], 1);
	for (int i = 0; i < 2; i++) {
		posix_spawn_file_actions_addclose(&actions, in[i]);
		posix_spawn_file_actions_addclose(&actions, out[i]);
	}
	rc = start(&pid, &actions, NULL, "shout", "");
	posix_spawn_file_actions_destroy(&actions);
	close(in[0]);
	close(out[1]);
	write(in[1], "through the child", 17);
	close(in[1]);
	size_t total = 0;
	while ((n = read(out[0], buf + total, sizeof buf - total)) > 0)
		total += n;
	printf("through a child and back: \"%.*s\"; it %s\n", (int)total, buf, ended(pid));
	close(out[0]);

	// What it is told to open is its own, and what it is not to have is closed.
	char path[1024];
	snprintf(path, sizeof path, "%s/spawn.out", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp");
	posix_spawn_file_actions_init(&actions);
	posix_spawn_file_actions_addopen(&actions, 1, path, O_WRONLY | O_CREAT | O_TRUNC, 0600);
	rc = start(&pid, &actions, NULL, "say", "into a file");
	posix_spawn_file_actions_destroy(&actions);
	printf("a child writing to a file: it %s", ended(pid));
	int fd = open(path, O_RDONLY);
	n = read(fd, buf, sizeof buf);
	printf("; the file has \"%.*s\"\n", n > 0 ? (int)n - 1 : 0, buf);
	close(fd);
	unlink(path);

	pipe(out);
	snprintf(number, sizeof number, "%d", out[0]);
	rc = start(&pid, NULL, NULL, "open", number);
	printf("it %s\n", ended(pid));
	posix_spawnattr_t attr;
	posix_spawnattr_init(&attr);
	posix_spawnattr_setflags(&attr, POSIX_SPAWN_CLOEXEC_DEFAULT);
	rc = start(&pid, NULL, &attr, "open", number);
	posix_spawnattr_destroy(&attr);
	printf("told to close what it is not given: it %s\n", ended(pid));
	fcntl(out[0], F_SETFD, FD_CLOEXEC);
	rc = start(&pid, NULL, NULL, "open", number);
	printf("one marked to be closed: it %s\n", ended(pid));
	close(out[0]);
	close(out[1]);

	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(path, sizeof path, "%s/spawn.dir", tmp);
	mkdir(path, 0700);
	chdir(path);
	rc = start(&pid, NULL, NULL, "where", "");
	printf("it %s\n", ended(pid));
	chdir("/");
	rmdir(path);

	// Waiting: for one that has not ended, for one that is not there, and for any.
	rc = start(&pid, NULL, NULL, "wait", "100000");
	int status = -1;
	pid_t got = waitpid(pid, &status, WNOHANG);
	printf("a child still running, not waited for: %d", got);
	got = waitpid(-1, &status, 0);
	printf("; waited for, any child: it is that one: %d, exited %d\n", got == pid, WEXITSTATUS(status));
	got = waitpid(pid, &status, 0);
	printf("waited for again: %d (%s)\n", got, strerror(errno));
	got = waitpid(-1, &status, WNOHANG);
	printf("with no child at all: %d (%s)\n", got, strerror(errno));

	// What cannot be started.
	char *none[] = {"none", NULL};
	printf("a program that is not there: %s\n", strerror(posix_spawn(&pid, "/com.example.nonesuch/program", NULL, NULL, none, environ)));
	printf("a directory: %s\n", strerror(posix_spawn(&pid, "/usr", NULL, NULL, none, environ)));
	snprintf(path, sizeof path, "%s/spawn.text", tmp);
	fd = open(path, O_WRONLY | O_CREAT | O_TRUNC, 0644);
	write(fd, "not a program\n", 14);
	close(fd);
	printf("a file that may not be run: %s\n", strerror(posix_spawn(&pid, path, NULL, NULL, none, environ)));
	chmod(path, 0755);
	printf("one that may, and is no program: %s\n", strerror(posix_spawn(&pid, path, NULL, NULL, none, environ)));
	unlink(path);
	return 0;
}
