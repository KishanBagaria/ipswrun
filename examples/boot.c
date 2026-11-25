// What a machine says of the time it has been up: when it booted, by what name that boot goes,
// and that its chip keeps no world apart from the kernel (as none before the A18 and the M4
// does). It prints the same natively on such a Mac as under ipswrun, for a release that has
// the call the last line makes.
#include <mach/mach.h>
#include <stdio.h>
#include <string.h>
#include <sys/sysctl.h>
#include <sys/time.h>
#include <time.h>

extern kern_return_t _exclaves_ctl_trap(mach_port_name_t name, uint32_t operation_and_flags, uint64_t identifier, void *buffer, uint64_t size, uint64_t size2,
                                        uint64_t offset, uint64_t status) __attribute__((weak_import));

int main(void) {
	struct timeval boot = {0}, now;
	size_t size = sizeof boot;
	int mib[2] = {CTL_KERN, KERN_BOOTTIME};
	int r = sysctl(mib, 2, &boot, &size, NULL, 0);
	gettimeofday(&now, NULL);
	double since = (now.tv_sec - boot.tv_sec) + (now.tv_usec - boot.tv_usec) / 1e6;
	double up = clock_gettime_nsec_np(CLOCK_MONOTONIC_RAW) / 1e9;
	// A Mac's kernel says it booted some seconds after its clock began to count.
	printf("kern.boottime: %d, %zu bytes; in the past: %d; no longer ago than the clock has counted: %d, and within a minute of that: %d\n", r, size, since > 0,
	       since <= up, up - since < 60);
	struct timeval named = {0};
	size = sizeof named;
	r = sysctlbyname("kern.boottime", &named, &size, NULL, 0);
	printf("by name: %d, the same: %d\n", r, named.tv_sec == boot.tv_sec && named.tv_usec == boot.tv_usec);

	char session[64] = "", again[64] = "";
	size = sizeof session;
	r = sysctlbyname("kern.bootsessionuuid", session, &size, NULL, 0);
	int shaped = strlen(session) == 36;
	for (int i = 0; shaped && i < 36; i++)
		shaped = i == 8 || i == 13 || i == 18 || i == 23 ? session[i] == '-' : strchr("0123456789ABCDEF", session[i]) != NULL;
	printf("kern.bootsessionuuid: %d, %zu bytes, a UUID in capitals: %d, of the random kind: %d", r, size, shaped, session[14] == '4');
	size = sizeof again;
	sysctlbyname("kern.bootsessionuuid", again, &size, NULL, 0);
	printf("; the same when asked again: %d\n", !strcmp(session, again));

	char buffer[0x98] = "";
	if (!_exclaves_ctl_trap) { // a release from before there were any
		printf("exclaves: not a call of this release\n");
		return 0;
	}
	printf("exclaves: looking up services %#x; anything else %#x and %#x\n", _exclaves_ctl_trap(MACH_PORT_NULL, 7u << 24, 0, buffer, sizeof buffer, 0, 0, 0),
	       _exclaves_ctl_trap(mach_task_self(), 1u << 24, 0, buffer, sizeof buffer, 0, 0, 0), _exclaves_ctl_trap(MACH_PORT_NULL, 0, 0, NULL, 0, 0, 0, 0));
	return 0;
}
