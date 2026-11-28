// The top of a new process's stack, as the kernel leaves it: the program's path, the arguments,
// the environment and the kernel's own strings, one after another up to the top. It is what the
// sysctl kern.procargs2 gives of a process, as it is now and not as it was started, so that one
// that writes over an argument is seen to have. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <mach-o/dyld.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <sys/sysctl.h>
#include <unistd.h>

extern char **environ;

static char block[1 << 18];

// asked is kern.procargs2 of this process into so much room: how much was given, or -1.
static long asked(size_t room) {
	int mib[3] = {CTL_KERN, KERN_PROCARGS2, getpid()};
	size_t size = room;
	memset(block, 0x55, sizeof block);
	return sysctl(mib, 3, block, &size, NULL, 0) ? -1 : (long)size;
}

int main(int argc, char **argv, char **envp, char **apple) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	if (argc == 1) { // again, with arguments that are the same whatever started it
		char self[1024];
		uint32_t size = sizeof self;
		_NSGetExecutablePath(self, &size);
		char *with[] = {self, "first", "two words", "", NULL};
		execve(self, with, environ);
		return 1;
	}
	char *top = pthread_get_stackaddr_np(pthread_self());
	long size = asked(sizeof block);
	int count;
	memcpy(&count, block, sizeof count);
	char *strings = top - (size - 4);
	printf("what the kernel says the process was started with: %d arguments, and then the top of its stack as it is %d\n", count, size > 4 && !memcmp(block + 4, strings, size - 4));

	// What is there, from the bottom.
	char **lastEnv = envp, **last = apple;
	while (lastEnv[0] && lastEnv[1])
		lastEnv++;
	while (last[1])
		last++;
	char *end = *last + strlen(*last) + 1;
	printf("it starts with the program's path, which the first of the kernel's strings names %d (\"%.16s\")", strings == apple[0] + 16, apple[0]);
	printf("; the arguments start a multiple of 8 after that string %d, with fewer than 8 zeros between %d\n", (argv[0] - apple[0]) % 8 == 0, argv[0] - (apple[0] + strlen(apple[0]) + 1) < 8);
	int inOrder = 1;
	for (int i = 0; i + 1 < argc; i++)
		inOrder &= argv[i + 1] == argv[i] + strlen(argv[i]) + 1;
	char *end0 = *lastEnv + strlen(*lastEnv) + 1;
	printf("the arguments one after another %d; the environment after them %d; the rest of the kernel's strings after that, a multiple of 8 above the first %d", inOrder, envp[0] == argv[argc - 1] + strlen(argv[argc - 1]) + 1,
	       apple[1] >= end0 && apple[1] - end0 < 8 && (apple[1] - apple[0]) % 8 == 0);
	printf("; they end fewer than 8 bytes below the top %d, which is a multiple of 8 above where they start %d\n", top - end >= 0 && top - end < 8, (top - apple[0]) % 8 == 0);
	printf("the pointers to them are below them all: the arguments' %d, the environment's above those %d, the kernel's above those %d\n", (char *)argv < apple[0], envp == argv + argc + 1,
	       (char **)apple > envp && (char *)apple < apple[0]);

	// It is the stack as it is now.
	argv[1][0] = 'F';
	asked(sizeof block);
	printf("an argument written over (\"%s\"): what the kernel then says of it: \"%s\"\n", argv[1], block + 4 + (argv[1] - strings));

	// Into less room than it all it is the count and nothing of the stack: zeros.
	for (long room = 5; room < size; room = room == 5 ? 20 : room == 20 ? size - 1 : size) {
		long given = asked(room), zeros = 0;
		memcpy(&count, block, sizeof count);
		for (long i = 4; i < room; i++)
			zeros += block[i] == 0;
		printf("into %s: %s, the count %d, and zeros for all the rest %d, with nothing after %d\n", room == 5 ? "5 bytes" : room == 20 ? "20" : "one less than it all", given == room ? "as much" : "not as much",
		       count, zeros == room - 4, block[room] == 0x55);
	}
	printf("into none: %ld", asked(0));
	printf("; into 4: %ld\n", asked(4));
	return 0;
}
