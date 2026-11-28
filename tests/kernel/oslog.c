// Logging to the system's log, which the release's own logd keeps (under ipswrun started when
// it is first asked for, with a data volume to keep its files on). It prints the same natively
// on a Mac as under ipswrun.
#include <os/log.h>
#include <os/signpost.h>
#include <stdio.h>
#include <unistd.h>

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	os_log_t log = os_log_create("run.ipsw.example", "oslog");
	printf("a log of its own; logged at all: %d, errors: %d, faults: %d\n", os_log_type_enabled(log, OS_LOG_TYPE_DEFAULT), os_log_type_enabled(log, OS_LOG_TYPE_ERROR),
	       os_log_type_enabled(log, OS_LOG_TYPE_FAULT));
	os_log(log, "a message, with a number %d and a string %{public}s", 42, "in the open");
	os_log_info(log, "for information");
	os_log_error(log, "an error: %{public}s", "something went wrong");
	os_log_fault(log, "a fault: %{private}s", "not for everyone");
	os_log(OS_LOG_DEFAULT, "to the log everyone has");
	os_signpost_id_t id = os_signpost_id_generate(log);
	os_signpost_interval_begin(log, id, "work");
	os_signpost_interval_end(log, id, "work");
	usleep(200000); // logd takes what was logged in its own time
	printf("logged\n");
	return 0;
}
