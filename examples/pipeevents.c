// Pipes as libdispatch watches them: a source that is told when a pipe can be read or written,
// and dispatch_read and dispatch_write, which NSPipe and NSFileHandle are built on. The kernel's
// part is the kqueue filters EVFILT_READ and EVFILT_WRITE. It prints the same natively on a Mac
// as under ipswrun.
#include <dispatch/dispatch.h>
#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

static void wait_for(dispatch_semaphore_t done, const char *what) {
	if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, 5 * NSEC_PER_SEC)))
		printf("%s: never happened\n", what);
}

static int p[2], q[2];

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	signal(SIGPIPE, SIG_IGN);
	pipe(p);
	dispatch_queue_t queue = dispatch_queue_create("com.example.ipswrun.pipes", DISPATCH_QUEUE_SERIAL);
	dispatch_semaphore_t done = dispatch_semaphore_create(0);

	// A source for reading is told how much there is to read, each time there is something, and
	// with nothing to read that its writer has gone.
	__block int events = 0;
	dispatch_semaphore_t cancelled = dispatch_semaphore_create(0);
	dispatch_source_t readable = dispatch_source_create(DISPATCH_SOURCE_TYPE_READ, p[0], 0, queue);
	dispatch_source_set_event_handler(readable, ^{
		char buf[64];
		unsigned long waiting = dispatch_source_get_data(readable);
		ssize_t n = read(p[0], buf, sizeof buf);
		printf("event %d: %lu to read, read %zd \"%.*s\"\n", ++events, waiting, n, n > 0 ? (int)n : 0, buf);
		if (n == 0)
			dispatch_source_cancel(readable);
		else
			dispatch_semaphore_signal(done);
	});
	dispatch_source_set_cancel_handler(readable, ^{
		printf("the reading source is cancelled\n");
		dispatch_semaphore_signal(cancelled);
	});
	dispatch_resume(readable);
	printf("write 5: %zd\n", write(p[1], "hello", 5));
	wait_for(done, "the first event");
	printf("write 3: %zd\n", write(p[1], "abc", 3));
	wait_for(done, "the second event");
	close(p[1]);
	wait_for(cancelled, "the event of the end");
	close(p[0]);

	// A source for writing is told how much room there is, until there is none.
	pipe(p);
	fcntl(p[1], F_SETFL, O_NONBLOCK);
	__block int rounds = 0;
	__block long total = 0;
	static char block[30000];
	dispatch_source_t writable = dispatch_source_create(DISPATCH_SOURCE_TYPE_WRITE, p[1], 0, queue);
	dispatch_source_set_event_handler(writable, ^{
		unsigned long room = dispatch_source_get_data(writable);
		ssize_t n = write(p[1], block, sizeof block);
		if (n > 0)
			total += n;
		printf("round %d: room for %lu, wrote %zd\n", ++rounds, room, n);
		if (n < (ssize_t)sizeof block) {
			dispatch_source_cancel(writable);
			dispatch_semaphore_signal(done);
		}
	});
	dispatch_resume(writable);
	wait_for(done, "the pipe filling");
	printf("in the pipe: %ld\n", total);
	close(p[0]);
	close(p[1]);

	// dispatch_write and dispatch_read, each to the end: what was written is read back.
	pipe(p);
	dispatch_data_t message = dispatch_data_create("through dispatch", 16, NULL, DISPATCH_DATA_DESTRUCTOR_DEFAULT);
	dispatch_write(p[1], message, queue, ^(dispatch_data_t left, int error) {
		printf("dispatch_write: error %d, %zu left\n", error, left ? dispatch_data_get_size(left) : 0);
		close(p[1]);
		dispatch_semaphore_signal(done);
	});
	wait_for(done, "dispatch_write");
	dispatch_read(p[0], 1024, queue, ^(dispatch_data_t data, int error) {
		const void *bytes = NULL;
		size_t size = 0;
		dispatch_data_t flat = dispatch_data_create_map(data, &bytes, &size);
		printf("dispatch_read: error %d, %zu bytes \"%.*s\"\n", error, size, (int)size, (const char *)bytes);
		(void)flat;
		dispatch_semaphore_signal(done);
	});
	wait_for(done, "dispatch_read");

	// A channel reads as much as there is, as it comes, to the end.
	pipe(q);
	__block size_t got = 0;
	__block int chunks = 0;
	dispatch_io_t channel = dispatch_io_create(DISPATCH_IO_STREAM, q[0], queue, ^(int error) {
		printf("the channel is closed: error %d\n", error);
		close(q[0]);
		dispatch_semaphore_signal(done);
	});
	dispatch_io_set_low_water(channel, 1);
	dispatch_io_read(channel, 0, SIZE_MAX, queue, ^(bool finished, dispatch_data_t data, int error) {
		if (data && dispatch_data_get_size(data)) {
			got += dispatch_data_get_size(data);
			printf("the channel read %zu, in all %zu\n", dispatch_data_get_size(data), got);
			chunks++;
			dispatch_semaphore_signal(done);
		}
		if (finished) {
			printf("the channel's read is finished: error %d, after %d reads\n", error, chunks);
			dispatch_semaphore_signal(done);
		}
	});
	printf("write 4: %zd\n", write(q[1], "one,", 4));
	wait_for(done, "the channel's first read");
	printf("write 4: %zd\n", write(q[1], "two.", 4));
	wait_for(done, "the channel's second read");
	close(q[1]);
	wait_for(done, "the channel's end");
	dispatch_io_close(channel, 0);
	wait_for(done, "the channel closing");
	return 0;
}
