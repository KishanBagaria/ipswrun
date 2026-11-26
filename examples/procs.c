// The processes there are, as ps asks the kernel for them (the sysctl kern.proc): one by its
// pid, those of a group, those of a user, and all of them, each as a struct kinfo_proc. It
// prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/wait.h>
#include <unistd.h>

static const char *said(long r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%ld", r);
	return t;
}

// asked asks for the processes of a kind, and says how many came, and whether pid is among them.
static struct kinfo_proc *asked(int kind, int of, int *count) {
	int mib[4] = {CTL_KERN, KERN_PROC, kind, of};
	size_t size = 0;
	*count = -1;
	if (sysctl(mib, kind == KERN_PROC_ALL ? 3 : 4, NULL, &size, NULL, 0))
		return NULL;
	struct kinfo_proc *list = calloc(1, size + sizeof *list);
	if (sysctl(mib, kind == KERN_PROC_ALL ? 3 : 4, list, &size, NULL, 0))
		return NULL;
	*count = (int)(size / sizeof *list);
	return list;
}

static int among(struct kinfo_proc *list, int count, pid_t pid) {
	for (int i = 0; i < count; i++)
		if (list[i].kp_proc.p_pid == pid)
			return 1;
	return 0;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	int mib[4] = {CTL_KERN, KERN_PROC, KERN_PROC_PID, getpid()}, count;
	struct kinfo_proc me;
	memset(&me, 0xAA, sizeof me);
	size_t size = 0;
	printf("a struct kinfo_proc is %zu bytes; asked how much room one process by its pid takes: %s", sizeof me, said(sysctl(mib, 4, NULL, &size, NULL, 0)));
	printf(", room for %zu and a remainder of %zu\n", size / sizeof me, size % sizeof me);
	size = sizeof me;
	printf("this process by its pid: %s, %zu bytes", said(sysctl(mib, 4, &me, &size, NULL, 0)), size);
	printf("; its pid %d, its parent %d, its group %d, its user %d and real user %d, its group of users %d\n", me.kp_proc.p_pid == getpid(), me.kp_eproc.e_ppid == getppid(),
	       me.kp_eproc.e_pgid == getpgrp(), me.kp_eproc.e_ucred.cr_uid == getuid(), me.kp_eproc.e_pcred.p_ruid == getuid(), me.kp_eproc.e_pcred.p_rgid == getgid());
	printf("it is called \"%s\", is running (%d), is a 64-bit process %d, at priority %d, started at some time %d, and has %d groups, the first its own %d\n", me.kp_proc.p_comm, me.kp_proc.p_stat,
	       (me.kp_proc.p_flag & P_LP64) != 0, me.kp_proc.p_nice, me.kp_proc.p_starttime.tv_sec > 1000000000, me.kp_eproc.e_ucred.cr_ngroups > 0, me.kp_eproc.e_ucred.cr_groups[0] == getgid());
	size = sizeof me - 8;
	printf("into too little room: %s, %zu bytes; into none at all: ", said(sysctl(mib, 4, &me, &size, NULL, 0)), size);
	size = 0;
	printf("%s, %zu bytes\n", said(sysctl(mib, 4, &me, &size, NULL, 0)), size);
	mib[3] = 99999;
	size = sizeof me;
	printf("a pid there is not: %s, %zu bytes; a kind there is not: ", said(sysctl(mib, 4, &me, &size, NULL, 0)), size);
	mib[2] = 77;
	size = sizeof me;
	printf("%s; with no pid given: ", said(sysctl(mib, 4, &me, &size, NULL, 0)));
	mib[2] = KERN_PROC_PID;
	size = sizeof me;
	printf("%s\n", said(sysctl(mib, 3, &me, &size, NULL, 0)));

	// A child, which is in this process's group and is its user's.
	pid_t child = fork();
	if (child == 0)
		for (;;)
			pause();
	struct kinfo_proc *list = asked(KERN_PROC_PGRP, getpgrp(), &count);
	printf("the processes of this one's group: some %d, this one among them %d, and its child %d", count > 1, among(list, count, getpid()), among(list, count, child));
	list = asked(KERN_PROC_UID, getuid(), &count);
	printf("; of its user: this one %d, its child %d", among(list, count, getpid()), among(list, count, child));
	list = asked(KERN_PROC_RUID, getuid(), &count);
	printf("; of its real user: both %d", among(list, count, getpid()) && among(list, count, child));
	list = asked(KERN_PROC_ALL, 0, &count);
	printf("; of all: both %d\n", among(list, count, getpid()) && among(list, count, child));
	list = asked(KERN_PROC_PID, child, &count);
	printf("the child by its pid: %d, its parent this process %d, called \"%s\"\n", count, count == 1 && list[0].kp_eproc.e_ppid == getpid(), count == 1 ? list[0].kp_proc.p_comm : "");
	kill(child, SIGKILL);
	usleep(100000);
	list = asked(KERN_PROC_PID, child, &count);
	printf("ended and not waited for: %d, its state %d", count, count == 1 ? list[0].kp_proc.p_stat : 0);
	waitpid(child, NULL, 0);
	list = asked(KERN_PROC_PID, child, &count);
	printf("; waited for: %d\n", count);
	return 0;
}
