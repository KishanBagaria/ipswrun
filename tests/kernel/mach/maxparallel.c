// How many threads of a class of service may usefully run at once, as the thread library asks
// the kernel for libdispatch (bsdthread_ctl's BSDTHREAD_CTL_QOS_MAX_PARALLELISM): the machine's
// processors that the class may use, which is every one for the classes from utility up, those
// of its least level for the two below, and those of its first level for real time. The
// numbers are the machine's, so each is held against what the machine says of its processors.
// It prints the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
#include <sys/sysctl.h>
#include <unistd.h>

#pragma clang diagnostic ignored "-Wdeprecated-declarations"

static int said(const char *name) {
	int value = -1;
	size_t size = sizeof value;
	return sysctlbyname(name, &value, &size, NULL, 0) ? -1 : value;
}

static int most(int class, int how) {
	errno = 0;
	int r = syscall(SYS_bsdthread_ctl, 0x800, class, how, 0);
	return r < 0 ? -errno : r;
}

int main(void) {
	int levels = said("hw.nperflevels");
	char least[64];
	snprintf(least, sizeof least, "hw.perflevel%d.physicalcpu", levels - 1);
	int physical = said("hw.physicalcpu"), logical = said("hw.logicalcpu"), first = said("hw.perflevel0.physicalcpu"), first_logical = said("hw.perflevel0.logicalcpu"), low = said(least);
	snprintf(least, sizeof least, "hw.perflevel%d.logicalcpu", levels - 1);
	int low_logical = said(least);
	printf("the machine says of its processors: all %d, by levels %d, the first level's %d, the least level's %d\n", physical > 0 && logical > 0, levels > 0, first > 0 && first_logical > 0, low > 0 && low_logical > 0);
	printf("classes from utility up may use every processor:");
	for (int class = 3; class <= 6; class++)
		printf(" %d %d", most(class, 0) == physical, most(class, 1) == logical);
	printf("; the two below, those of the least level: %d %d %d %d\n", most(1, 0) == low, most(1, 1) == low_logical, most(2, 0) == low, most(2, 1) == low_logical);
	printf("real time, the first level's: %d %d\n", most(0, 2) == first, most(0, 3) == first_logical);
	printf("what does not go together: no class counted as the others are %s, %s; a class counted as real time is", strerror(-most(0, 0)), strerror(-most(0, 1)));
	for (int class = 1; class <= 6; class++)
		printf(" %s%s", strerror(-most(class, 2)), class < 6 ? "," : "");
	printf("; a class there is not: %s, %s, %s\n", strerror(-most(7, 0)), strerror(-most(7, 1)), strerror(-most(7, 2)));
	errno = 0;
	int r = syscall(SYS_bsdthread_ctl, 0x12345, 0, 0, 0);
	printf("a thing to ask of threads that there is not: %s\n", r < 0 ? strerror(errno) : "done");
	uid_t user = 77;
	gid_t group = 77;
	errno = 0;
	r = syscall(SYS_gettid, &user, &group);
	printf("whom the thread acts as, apart from its process: %s\n", r < 0 ? strerror(errno) : "someone");
	return 0;
}
