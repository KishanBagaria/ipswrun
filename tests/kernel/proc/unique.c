// What tells one process from every other the machine has had (proc_pidinfo's unique record, as
// logd and the crash reporter ask of a process): the UUID of its program, a number of its own
// that no later process gets, its parent's, and a version that moves on when it becomes another
// program. It prints the same natively on a Mac as under ipswrun.
#include <mach-o/dyld.h>
#include <mach-o/loader.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

extern char **environ;
extern int proc_pidinfo(int pid, int flavor, uint64_t arg, void *buffer, int size);

struct unique { // struct proc_uniqidentifierinfo, which no SDK has
	uint8_t uuid[16];
	uint64_t id, parent;
	int32_t version, parentVersion;
	uint64_t reserved[2];
};

static int of(pid_t pid, struct unique *u) {
	memset(u, 0x55, sizeof *u);
	return proc_pidinfo(pid, 17, 0, u, sizeof *u); // PROC_PIDUNIQIDENTIFIERINFO
}

static int zeros(const void *at, size_t n) {
	const unsigned char *b = at;
	for (size_t i = 0; i < n; i++)
		if (b[i])
			return 0;
	return 1;
}

int main(int argc, char **argv) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	struct unique mine, other;
	int got = of(getpid(), &mine);
	if (argc > 1) { // what a copy became: the numbers it had as a copy are its arguments
		printf("the program a copy becomes: its own number still %d, its parent's still %d, its version later %d, its parent's version still %d\n", mine.id == strtoull(argv[1], NULL, 10),
		       mine.parent == strtoull(argv[2], NULL, 10), mine.version > atoi(argv[3]), mine.parentVersion == atoi(argv[4]));
		return 0;
	}
	const struct mach_header_64 *header = (const void *)_dyld_get_image_header(0);
	const struct load_command *c = (const void *)(header + 1);
	int same = 0;
	for (uint32_t i = 0; i < header->ncmds; i++, c = (const void *)((const char *)c + c->cmdsize))
		if (c->cmd == LC_UUID)
			same = !memcmp(((const struct uuid_command *)c)->uuid, mine.uuid, 16);
	printf("of itself: %d bytes; the UUID is its program's %d; a number of its own %d, and its parent's %d; a version %d; the rest zeros %d\n", got, same, mine.id > 1, mine.parent > 0, mine.version > 0,
	       zeros(mine.reserved, sizeof mine.reserved));

	got = of(1, &other);
	printf("of launchd: %d bytes; a UUID %d; its number %llu, its parent's %llu; a version %d, its parent's %d\n", got, !zeros(other.uuid, 16), other.id, other.parent, other.version > 0, other.parentVersion);
	got = of(0, &other);
	printf("of the kernel: %d bytes, all zeros %d; of no process: %d\n", got, zeros(&other, sizeof other), of(99999, &other));

	// A copy has a number of its own, and its parent's for its parent's.
	int back[2], on[2];
	pipe(back);
	pipe(on);
	pid_t pid = fork();
	if (pid == 0) {
		struct unique copy;
		of(getpid(), &copy);
		write(back[1], &copy, sizeof copy);
		char text[4][24], self[1024], go;
		read(on[0], &go, 1); // once its parent has said what it was as a copy
		snprintf(text[0], 24, "%llu", copy.id);
		snprintf(text[1], 24, "%llu", copy.parent);
		snprintf(text[2], 24, "%d", copy.version);
		snprintf(text[3], 24, "%d", copy.parentVersion);
		uint32_t size = sizeof self;
		_NSGetExecutablePath(self, &size);
		char *becomes[] = {self, text[0], text[1], text[2], text[3], NULL};
		execve(self, becomes, environ);
		_exit(1);
	}
	struct unique first, second;
	read(back[0], &first, sizeof first);
	printf("a copy: the same program %d; a later number of its own %d, its parent's is this one's %d; a later version %d, its parent's is this one's %d\n", !memcmp(first.uuid, mine.uuid, 16),
	       first.id > mine.id, first.parent == mine.id, first.version > mine.version, first.parentVersion == mine.version);
	write(on[1], "", 1);
	waitpid(pid, NULL, 0);
	pid = fork();
	if (pid == 0) {
		struct unique copy;
		of(getpid(), &copy);
		write(back[1], &copy, sizeof copy);
		_exit(0);
	}
	read(back[0], &second, sizeof second);
	printf("another copy: a number later than the first's %d, and a later version %d\n", second.id > first.id, second.version > first.version);
	usleep(100000);
	printf("of it once it has ended, not waited for: %d", of(pid, &other));
	waitpid(pid, NULL, 0);
	of(getpid(), &other);
	printf("; and this one's own are as they were %d\n", !memcmp(&other, &mine, sizeof mine));
	return 0;
}
