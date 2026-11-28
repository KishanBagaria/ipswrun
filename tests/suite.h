// A suite is one guest made of several programs of its folder, so that a release is started
// once for all of them. Each part is a program in a file of its own, built with its main under
// its own name, and is run in a copy of the process, so that it starts with the descriptors,
// the signals and the memory a program of its own would have had and leaves nothing to the
// next. What it prints comes under a line with its name.
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

static void part(const char *name, int (*run)(void)) {
	printf("== %s\n", name);
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		int status = run();
		fflush(stdout);
		_exit(status);
	}
	int status = 0;
	if (child < 0 || waitpid(child, &status, 0) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 0)
		printf("== %s did not end well: %d\n", name, status);
	fflush(stdout);
}
