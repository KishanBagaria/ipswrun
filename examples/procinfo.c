// What the kernel says of one process (proc_pidinfo), which ps and Activity Monitor ask after
// they have the list: of this process, of a copy of it, of one that has ended, and of launchd,
// which is root's and says less of itself to a user. It prints the same natively on a Mac as
// under ipswrun.
#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/proc_info.h>
#include <sys/resource.h>
#include <sys/wait.h>
#include <unistd.h>

extern int proc_pidinfo(int pid, int flavor, uint64_t arg, void *buffer, int size);
extern int proc_pidpath(int pid, void *buffer, uint32_t size);

static const char *whose(uint32_t pid) { return pid == (uint32_t)getpid() ? "this process" : pid == (uint32_t)getppid() ? "this one's parent" : pid == 1 ? "launchd" : pid == 0 ? "none" : "another"; }

static void said(const char *what, pid_t pid) {
	struct proc_bsdinfo b;
	memset(&b, 0, sizeof b);
	errno = 0;
	int r = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &b, sizeof b);
	if (r <= 0)
		printf("%s: %d (%s)\n", what, r, strerror(errno));
	else
		printf("%s: %d bytes; a 64-bit process %d, has become a program %d, state %d, its pid %d, its parent %s, user %d and group %d its caller's, called \"%s\" and \"%s\", in %s group, on a terminal %d, at "
		       "priority %d, started at some time %d\n",
		       what, r, (b.pbi_flags & PROC_FLAG_LP64) != 0, (b.pbi_flags & PROC_FLAG_EXEC) != 0, b.pbi_status, b.pbi_pid == (uint32_t)pid, whose(b.pbi_ppid),
		       b.pbi_uid == getuid() && b.pbi_ruid == getuid() && b.pbi_svuid == getuid(), b.pbi_gid == getgid() && b.pbi_rgid == getgid() && b.pbi_svgid == getgid(), b.pbi_comm, b.pbi_name,
		       b.pbi_pgid == (uint32_t)getpgrp() ? "this process's" : b.pbi_pgid == (uint32_t)pid ? "its own" : "another", b.e_tdev != 0xFFFFFFFF, b.pbi_nice, b.pbi_start_tvsec > 1000000000);
	struct proc_bsdshortinfo s;
	memset(&s, 0, sizeof s);
	errno = 0;
	r = proc_pidinfo(pid, PROC_PIDT_SHORTBSDINFO, 0, &s, sizeof s);
	if (r <= 0)
		printf("   in short: %d (%s)", r, strerror(errno));
	else
		printf("   in short: %d bytes; a 64-bit process %d, has become a program %d, state %d, its parent %s, called \"%s\", user %d, in %s group", r, (s.pbsi_flags & PROC_FLAG_LP64) != 0,
		       (s.pbsi_flags & PROC_FLAG_EXEC) != 0, s.pbsi_status, whose(s.pbsi_ppid), s.pbsi_comm, s.pbsi_uid, s.pbsi_pgid == (uint32_t)getpgrp() ? "this process's" : s.pbsi_pgid == (uint32_t)pid ? "its own" : "another");
	char path[4096] = "", arch[16];
	errno = 0;
	r = proc_pidpath(pid, path, sizeof path);
	// Where the program is is the host's to say for a guest; launchd's is the release's.
	printf("; its program: %s", r <= 0 ? strerror(errno) : pid == 1 ? path : path[0] == '/' ? "a whole path" : path);
	errno = 0;
	r = proc_pidinfo(pid, 19, 0, arch, 12); // PROC_PIDARCHINFO, with room for more than it is
	printf("; its architecture: %d%s%s\n", r, r > 0 ? "" : " ", r > 0 ? "" : strerror(errno));
}

// What file a part of a process's memory is of (flavor 31, PROC_PIDREGIONPATH: where the region
// begins, how long it is, and the file's path), which an older dyld asks of where it is itself.
static void region(const char *what, uint64_t address, int room) {
	struct {
		uint64_t begins, length;
		char path[1024];
	} r;
	memset(&r, 0, sizeof r);
	errno = 0;
	int n = proc_pidinfo(getpid(), 31, address, &r, room);
	if (n <= 0)
		printf("%s: %d (%s)\n", what, n, strerror(errno));
	else
		printf("%s: %d bytes; the region begins %s, is whole pages %d, and is of a file with a whole path %d\n", what, n, r.begins == address ? "there" : r.begins < address ? "below" : "above",
		       r.length > 0 && r.length % 16384 == 0, r.path[0] == '/');
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	printf("a struct proc_bsdinfo is %zu bytes, and the short one %zu\n", sizeof(struct proc_bsdinfo), sizeof(struct proc_bsdshortinfo));
	said("this process", getpid());
	setpriority(PRIO_PROCESS, 0, 5);
	said("made nicer by 5", getpid());
	pid_t copy = fork();
	if (copy == 0)
		for (;;)
			pause();
	said("a copy of it", copy);
	kill(copy, SIGKILL);
	usleep(100000);
	said("ended, and not waited for", copy);
	waitpid(copy, NULL, 0);
	said("waited for", copy);
	said("launchd", 1);
	said("no process", 99999);
	unsigned char both[192], full[136], unique[56];
	memset(both, 0, sizeof both);
	int got = proc_pidinfo(getpid(), 18, 1, both, 192); // PROC_PIDBSDINFOWITHUNIQID, as logd asks of who logs
	proc_pidinfo(getpid(), PROC_PIDTBSDINFO, 0, full, 136);
	proc_pidinfo(getpid(), 17, 0, unique, 56);
	printf("the full record and the unique one at once: %d bytes, the one after the other %d", got, !memcmp(both, full, 136) && !memcmp(both + 136, unique, 56));
	errno = 0;
	printf("; with too little room: %d (%s)", proc_pidinfo(getpid(), 18, 1, both, 191), strerror(errno));
	errno = 0;
	printf("; of launchd: %d (%s)\n", proc_pidinfo(1, 18, 1, both, 192), strerror(errno));
	int local = 0;
	region("the memory this program's code is in", (uint64_t)(uintptr_t)&main, 1040);
	region("with more room than it takes", (uint64_t)(uintptr_t)&main, 2000);
	region("with too little", (uint64_t)(uintptr_t)&main, 1039);
	region("the stack, which is of no file", (uint64_t)(uintptr_t)&local, 1040);
	region("a library of the shared cache", (uint64_t)(uintptr_t)&printf, 1040);
	return 0;
}
