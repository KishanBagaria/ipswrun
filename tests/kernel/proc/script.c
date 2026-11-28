// A script: a file that begins with #! and names the program that is to read it. The kernel
// starts that program, with what the line says after it, the script's path, and the arguments
// the script was given. The program named here is this one, which says what it was started
// with. It prints the same natively on a Mac as under ipswrun.
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
static char self[1024], dir[512];

// run writes a script with a first line, starts it with two arguments, and says how it went.
static void run(const char *what, const char *first, int mode, int exec) {
	char path[600];
	snprintf(path, sizeof path, "%s/the-script", dir);
	int fd = open(path, O_CREAT | O_TRUNC | O_WRONLY, 0600);
	write(fd, first, strlen(first));
	write(fd, "\nthe rest of the script\n", 24);
	fchmod(fd, mode);
	close(fd);
	char *argv[] = {"its own name", "one", "two words", NULL};
	printf("%s:\n", what);
	pid_t pid = 0;
	int status = 0, r = 0;
	if (exec) {
		if ((pid = fork()) == 0) {
			execve(path, argv, environ);
			printf("  execve: %s\n", strerror(errno));
			_exit(9);
		}
	} else if ((r = posix_spawn(&pid, path, NULL, NULL, argv, environ)) != 0) {
		printf("  posix_spawn: %s\n", strerror(r));
		unlink(path);
		return;
	}
	waitpid(pid, &status, 0);
	printf("  it exited %d\n", WEXITSTATUS(status));
	unlink(path);
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (getenv("SCRIPT_NAMED_ME")) {
		// As the program a script names: what it was started with, less where the script is.
		for (int i = 0; i < argc; i++) {
			const char *arg = argv[i], *slash = strrchr(arg, '/');
			printf("  %d: \"%s\"\n", i, slash && !strcmp(slash, "/the-script") ? "the script's path" : i == 0 && slash ? "this program's path" : arg);
		}
		return 3;
	}
	uint32_t size = sizeof self;
	_NSGetExecutablePath(self, &size);
	snprintf(dir, sizeof dir, "%s/script.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	mkdir(dir, 0755);
	setenv("SCRIPT_NAMED_ME", "1", 1); // so that this program, started for a script, does not start more
	char line[1400];
	snprintf(line, sizeof line, "#!%s --told", self);
	run("a script that names this program", line, 0755, 0);
	run("the same, by execve", line, 0755, 1);
	snprintf(line, sizeof line, "#! \t%s   --told  and more\twords  ", self);
	run("with spaces about, and more words after", line, 0755, 0);
	snprintf(line, sizeof line, "#!%s", self);
	run("with nothing after the program", line, 0755, 0);
	run("that may not be run", line, 0644, 0);
	run("that names a program there is not", "#!/no/such/program --told", 0755, 0);
	run("that names a directory", "#!/usr/bin", 0755, 0);
	run("that names nothing", "#!", 0755, 0);
	run("that is no script, and no program", "just words", 0755, 0);
	run("the same, by execve", "just words", 0755, 1);
	run("a shell's", "#!/bin/sh\nexit 4", 0755, 0);
	printf("tidied: %d\n", rmdir(dir));
	return 0;
}
