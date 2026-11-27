// A program that has launchd take it out of the console's session, as nohup does before it
// starts what it was given, so that what it starts outlives the window it was started from:
// the first time launchd does, and a second time it says it already has. It prints the same
// natively on a Mac as under ipswrun.
#include <stdint.h>
#include <stdio.h>

// What launchd's library has for it (vproc_priv.h, which no SDK ships): no error is NULL.
extern void *_vprocmgr_detach_from_console(uint64_t flags);

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	printf("detached from the console: %s\n", _vprocmgr_detach_from_console(0) ? "an error" : "no error");
	printf("and a second time: %s\n", _vprocmgr_detach_from_console(0) ? "an error" : "no error");
	return 0;
}
