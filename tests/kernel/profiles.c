// A sandbox by the name of its profile (sandbox_init with SANDBOX_NAMED), as a daemon asks for
// its own. Some profiles the kernel has itself, opendirectoryd's among them; the others are
// files of the release's, which the library compiles; and a name that is neither is refused.
// Each is asked for by a copy of this process, since a sandbox once on stays on. It prints the
// same natively on a Mac as under ipswrun.
#include <sandbox.h>
#include <stdio.h>
#include <sys/wait.h>
#include <unistd.h>

static void named(const char *what, const char *name) {
	fflush(stdout);
	pid_t pid = fork();
	if (pid == 0) {
		char *error = NULL;
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
		int r = sandbox_init(name, SANDBOX_NAMED, &error);
#pragma clang diagnostic pop
		printf("%s (%s): %d%s%s\n", what, name, r, error ? ", " : "", error ? error : "");
		fflush(stdout);
		_exit(0);
	}
	waitpid(pid, NULL, 0);
}

int main(void) {
	named("a profile the kernel has", "com.apple.opendirectoryd");
	named("one that is a file of the release's", "com.apple.notifyd");
	named("one there is not", "no.such.profile");
	return 0;
}
