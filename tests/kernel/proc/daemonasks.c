// Four things daemons ask of the kernel as they start, by a program that is no daemon and not
// root: a limit on how often it may be woken (proc_rlimit_control), which the kernel no longer
// keeps; a process's path by its pid and the version or number that tells it from every other
// (proc_info_extended_id, as proc_pidpath_audittoken asks); the groups of a user
// (initgroups); and how much memory it may use before it is ended (memorystatus_control).
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <mach/mach.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

extern int proc_rlimit_control(pid_t pid, int flavor, void *arg);
extern int __proc_info_extended_id(int call, int pid, int flavor, uint32_t flags, uint64_t id, uint64_t arg, void *buffer, int size);
extern int __proc_info(int call, int pid, int flavor, uint64_t arg, void *buffer, int size);
extern int memorystatus_control(uint32_t command, int32_t pid, uint32_t flags, void *buffer, size_t size);
extern int initgroups(const char *name, int basegid); // the library's, which asks the kernel what follows
extern int __initgroups(unsigned count, int *groups, int user);

static const char *said(int r) {
	static char text[8][48];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 48, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 48, "%d", r);
	return t;
}

struct unique { // struct proc_uniqidentifierinfo, which no SDK has
	uint8_t uuid[16];
	uint64_t id, parent;
	int32_t version, parentVersion;
	uint64_t reserved[2];
};

// path asks for this process's path by its pid and something more that must be its own.
static const char *path(uint32_t flags, uint64_t id) {
	static char text[4][32];
	static int next;
	char *t = text[next++ % 4], found[1024] = "";
	if (__proc_info_extended_id(2, getpid(), 11, flags, id, 0, found, sizeof found) < 0) // PROC_PIDPATHINFO
		return strerror(errno);
	snprintf(t, 32, "%s", found[0] == '/' ? "its path" : "something else");
	return t;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	struct {
		uint32_t flags;
		int32_t rate;
	} wakeups = {1, 150};
	printf("a limit on its wakeups: %s; by its pid: %s; of launchd: %s; of no process: %s; a kind of limit there is not: %s\n", said(proc_rlimit_control(-1, 1, &wakeups)), said(proc_rlimit_control(getpid(), 1, &wakeups)),
	       said(proc_rlimit_control(1, 1, &wakeups)), said(proc_rlimit_control(99999, 1, &wakeups)), said(proc_rlimit_control(-1, 99, &wakeups)));

	audit_token_t token;
	mach_msg_type_number_t count = TASK_AUDIT_TOKEN_COUNT;
	task_info(mach_task_self(), TASK_AUDIT_TOKEN, (task_info_t)&token, &count);
	struct unique mine;
	__proc_info(2, getpid(), 17, 0, &mine, sizeof mine); // PROC_PIDUNIQIDENTIFIERINFO
	printf("the version its token has is its own %d\n", token.val[7] == (uint32_t)mine.version);
	printf("its path, by its pid and its version: %s; and a version that is not its own: %s; by its number: %s; and one that is not: %s; by both at once: %s; by neither: %s\n", path(1, token.val[7]),
	       path(1, (uint64_t)token.val[7] + 1), path(2, mine.id), path(2, mine.id + 1), path(3, mine.id), path(0, 12345));

	int groups[17] = {20};
	printf("the groups of a user set, by one who is not root: %s; seventeen of them: %s\n", said(__initgroups(1, groups, getuid())), said(__initgroups(17, groups, getuid())));

	char room[64];
	printf("asked how much memory it may use: %s; to be kept from being ended while idle: %s; to be told when memory is short before others: %s\n", said(memorystatus_control(8, getpid(), 0, room, 16)),
	       said(memorystatus_control(14, getpid(), 1, NULL, 0)), said(memorystatus_control(9, getpid(), 0, NULL, 0)));
	return 0;
}
