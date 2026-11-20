// IPC guest: Mach messages between threads (reply ports, timeouts, port sets, large and complex
// messages, mk_timer), and what is built on them: a libdispatch Mach source, a CFRunLoop with a
// timer and a CFMachPort, and XPC connections inside one process and to a service nobody runs.
// Each line is printed as its check finishes, and every wait gives up after a while, so a check
// that cannot work says so and the next one still runs. An argument picks the sections to run by
// their letters, a to j: "ipc cf" runs the port set and mk_timer sections.
#include <CoreFoundation/CoreFoundation.h>
#include <dispatch/dispatch.h>
#include <mach/mach.h>
#include <mach/mach_time.h>
#include <pthread.h>
#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include "xpc_request.h"

// Not in any header: a port to be answered on, and the timers CFRunLoop itself used to be built on.
mach_port_name_t mach_reply_port(void);
mach_port_t mk_timer_create(void);
kern_return_t mk_timer_arm(mach_port_t timer, uint64_t deadline);
kern_return_t mk_timer_destroy(mach_port_t timer);

#define WAIT 2000 // milliseconds any one wait may take before the check is given up
#define OOL_SIZE (64 * 1024)
#define XPC_DATA_SIZE (100 * 1024)

typedef struct {
    mach_msg_header_t header;
    char text[40];
} message;

// A received message is followed by a trailer the kernel adds.
typedef struct {
    message message;
    mach_msg_trailer_t trailer;
} received;

// A complex message: the descriptors are not aligned to their pointers.
#pragma pack(push, 4)
typedef struct {
    mach_msg_header_t header;
    mach_msg_body_t body;
    mach_msg_ool_descriptor_t memory;
    mach_msg_port_descriptor_t port;
} parcel;

typedef struct {
    parcel parcel;
    mach_msg_trailer_t trailer;
} received_parcel;
#pragma pack(pop)

// A receive right and a send right to it, under one name.
static mach_port_t new_port(void) {
    mach_port_t port = MACH_PORT_NULL;
    if (mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &port) != KERN_SUCCESS) return MACH_PORT_NULL;
    mach_port_insert_right(mach_task_self(), port, port, MACH_MSG_TYPE_MAKE_SEND);
    return port;
}

static void drop_port(mach_port_t port) {
    mach_port_mod_refs(mach_task_self(), port, MACH_PORT_RIGHT_RECEIVE, -1);
    mach_port_deallocate(mach_task_self(), port); // the send right, a dead name by now
}

static kern_return_t send_text(mach_port_t to, mach_port_t reply, mach_msg_id_t id, const char *text) {
    message out = {{0}};
    out.header.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, reply ? MACH_MSG_TYPE_MAKE_SEND_ONCE : 0);
    out.header.msgh_size = sizeof out;
    out.header.msgh_remote_port = to;
    out.header.msgh_local_port = reply;
    out.header.msgh_id = id;
    strlcpy(out.text, text, sizeof out.text);
    return mach_msg(&out.header, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof out, 0, MACH_PORT_NULL, WAIT, MACH_PORT_NULL);
}

static kern_return_t receive(mach_port_t from, received *in, mach_msg_timeout_t milliseconds) {
    memset(in, 0, sizeof *in);
    return mach_msg(&in->message.header, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof *in, from, milliseconds, MACH_PORT_NULL);
}

static uint64_t checksum(const unsigned char *bytes, size_t length) {
    uint64_t sum = 0;
    for (size_t i = 0; i < length; i++) sum = sum * 31 + bytes[i];
    return sum;
}

static void fill(unsigned char *bytes, size_t length) {
    for (size_t i = 0; i < length; i++) bytes[i] = (unsigned char)(i * 7 + (i >> 8));
}

static uint64_t milliseconds_since(uint64_t start) {
    mach_timebase_info_data_t timebase;
    mach_timebase_info(&timebase);
    return (mach_absolute_time() - start) * timebase.numer / timebase.denom / NSEC_PER_MSEC;
}

static dispatch_time_t patience(void) { return dispatch_time(DISPATCH_TIME_NOW, WAIT * NSEC_PER_MSEC); }

// a. A server thread answers on the reply port that came with the message.

struct server {
    mach_port_t port;
    kern_return_t result;
    int own_port, reply_port;
};

static void *serve_ping(void *arg) {
    struct server *server = arg;
    received in;
    server->result = receive(server->port, &in, WAIT);
    if (server->result != KERN_SUCCESS) return NULL;
    server->own_port = in.message.header.msgh_local_port == server->port;
    server->reply_port = in.message.header.msgh_remote_port != MACH_PORT_NULL;

    message out = in.message;
    out.header.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MOVE_SEND_ONCE, 0);
    out.header.msgh_size = sizeof out;
    out.header.msgh_local_port = MACH_PORT_NULL;
    out.header.msgh_id = in.message.header.msgh_id + 100;
    out.text[sizeof out.text - 1] = 0;
    for (char *c = out.text; *c; c++)
        if (*c >= 'a' && *c <= 'z') *c -= 'a' - 'A';
    server->result =
        mach_msg(&out.header, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof out, 0, MACH_PORT_NULL, WAIT, MACH_PORT_NULL);
    return NULL;
}

static void ping(void) {
    struct server server = {new_port(), KERN_SUCCESS, 0, 0};
    pthread_t thread;
    pthread_create(&thread, NULL, serve_ping, &server);

    // One call sends the request and waits for the answer, the way every MIG routine does.
    mach_port_t reply = mach_reply_port();
    received call;
    memset(&call, 0, sizeof call);
    call.message.header.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
    call.message.header.msgh_size = sizeof call.message;
    call.message.header.msgh_remote_port = server.port;
    call.message.header.msgh_local_port = reply;
    call.message.header.msgh_id = 100;
    strlcpy(call.message.text, "ping from the main thread", sizeof call.message.text);
    kern_return_t result = mach_msg(&call.message.header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_SEND_TIMEOUT | MACH_RCV_TIMEOUT,
                                    sizeof call.message, sizeof call, reply, WAIT, MACH_PORT_NULL);
    pthread_join(thread, NULL);
    call.message.text[sizeof call.message.text - 1] = 0;
    if (result != KERN_SUCCESS || server.result != KERN_SUCCESS)
        printf("ping     failed: client 0x%x server 0x%x\n", result, server.result);
    else
        printf("ping     reply %d \"%s\" on the reply port %d, server saw its port %d and a reply port %d\n",
               call.message.header.msgh_id, call.message.text, call.message.header.msgh_local_port == reply, server.own_port,
               server.reply_port);
    mach_port_mod_refs(mach_task_self(), reply, MACH_PORT_RIGHT_RECEIVE, -1);
    drop_port(server.port);
}

// b. Nothing is sent: the receive gives up, and not before its time.
static void receive_timeout(void) {
    mach_port_t port = new_port();
    received in;
    uint64_t start = mach_absolute_time();
    kern_return_t result = receive(port, &in, 10);
    printf("timeout  empty port: MACH_RCV_TIMED_OUT %d after 10 ms %d\n", result == MACH_RCV_TIMED_OUT,
           milliseconds_since(start) >= 10);
    drop_port(port);
}

// c. A receive on a port set says which member the message came to.
static void port_set(void) {
    mach_port_t set = MACH_PORT_NULL, first = new_port(), second = new_port();
    mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_PORT_SET, &set);
    kern_return_t joined = mach_port_insert_member(mach_task_self(), first, set);
    if (joined == KERN_SUCCESS) joined = mach_port_insert_member(mach_task_self(), second, set);

    received in;
    send_text(second, MACH_PORT_NULL, 302, "to the second");
    kern_return_t result = receive(set, &in, WAIT);
    int to_second = result == KERN_SUCCESS && in.message.header.msgh_local_port == second && in.message.header.msgh_id == 302;
    send_text(first, MACH_PORT_NULL, 301, "to the first");
    result = receive(set, &in, WAIT);
    int to_first = result == KERN_SUCCESS && in.message.header.msgh_local_port == first && in.message.header.msgh_id == 301;
    int empty = receive(set, &in, 0) == MACH_RCV_TIMED_OUT;
    printf("portset  2 members %d: message to the second came to it %d, to the first %d, then empty %d\n", joined == KERN_SUCCESS,
           to_second, to_first, empty);
    drop_port(first);
    drop_port(second);
    mach_port_mod_refs(mach_task_self(), set, MACH_PORT_RIGHT_PORT_SET, -1);
}

// d. With MACH_RCV_LARGE a message too big for the buffer stays queued, and the header says how
// big it is (without its trailer).
static void receive_large(void) {
    static struct {
        mach_msg_header_t header;
        unsigned char bytes[1024];
        mach_msg_trailer_t trailer;
    } big, out;
    struct {
        mach_msg_header_t header;
        char room[40];
    } small;
    mach_port_t port = new_port();

    fill(out.bytes, sizeof out.bytes);
    out.header.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0);
    out.header.msgh_size = sizeof out - sizeof out.trailer;
    out.header.msgh_remote_port = port;
    out.header.msgh_id = 400;
    kern_return_t sent =
        mach_msg(&out.header, MACH_SEND_MSG | MACH_SEND_TIMEOUT, out.header.msgh_size, 0, MACH_PORT_NULL, WAIT, MACH_PORT_NULL);

    memset(&small, 0, sizeof small);
    kern_return_t first =
        mach_msg(&small.header, MACH_RCV_MSG | MACH_RCV_TIMEOUT | MACH_RCV_LARGE, 0, sizeof small, port, WAIT, MACH_PORT_NULL);
    kern_return_t second =
        mach_msg(&big.header, MACH_RCV_MSG | MACH_RCV_TIMEOUT | MACH_RCV_LARGE, 0, sizeof big, port, WAIT, MACH_PORT_NULL);
    printf("large    sent %d, %d-byte buffer: MACH_RCV_TOO_LARGE %d needs %u, retry got %u bytes %d intact %d\n",
           sent == KERN_SUCCESS, (int)sizeof small, first == MACH_RCV_TOO_LARGE, small.header.msgh_size,
           second == KERN_SUCCESS ? big.header.msgh_size : 0, second == KERN_SUCCESS && big.header.msgh_id == 400,
           memcmp(big.bytes, out.bytes, sizeof out.bytes) == 0);
    drop_port(port);
}

// e. A message that carries memory and a port right instead of bytes.

struct receiver {
    mach_port_t port;
    void *sent_from;
    uint64_t sum;
    kern_return_t result;
    int complex, descriptors, size, elsewhere, intact, send_right, used, freed;
};

static void *receive_parcel(void *arg) {
    struct receiver *receiver = arg;
    received_parcel in;
    memset(&in, 0, sizeof in);
    receiver->result =
        mach_msg(&in.parcel.header, MACH_RCV_MSG | MACH_RCV_TIMEOUT, 0, sizeof in, receiver->port, WAIT, MACH_PORT_NULL);
    if (receiver->result != KERN_SUCCESS) return NULL;
    receiver->complex = (in.parcel.header.msgh_bits & MACH_MSGH_BITS_COMPLEX) != 0;
    receiver->descriptors = in.parcel.body.msgh_descriptor_count;
    if (!receiver->complex || receiver->descriptors != 2) return NULL;

    void *memory = in.parcel.memory.address;
    receiver->size = in.parcel.memory.size;
    receiver->elsewhere = memory != NULL && memory != receiver->sent_from;
    receiver->intact = memory != NULL && receiver->size == OOL_SIZE && checksum(memory, OOL_SIZE) == receiver->sum;
    receiver->send_right = in.parcel.port.disposition == MACH_MSG_TYPE_PORT_SEND;
    receiver->used = send_text(in.parcel.port.name, MACH_PORT_NULL, 500, "through the carried right") == KERN_SUCCESS;
    receiver->freed = vm_deallocate(mach_task_self(), (vm_address_t)memory, receiver->size) == KERN_SUCCESS;
    mach_port_deallocate(mach_task_self(), in.parcel.port.name);
    return NULL;
}

static void complex_message(void) {
    mach_port_t port = new_port(), carried = MACH_PORT_NULL;
    mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &carried);
    vm_address_t memory = 0;
    if (vm_allocate(mach_task_self(), &memory, OOL_SIZE, VM_FLAGS_ANYWHERE) != KERN_SUCCESS) {
        printf("complex  failed: no memory\n");
        return;
    }
    fill((unsigned char *)memory, OOL_SIZE);
    struct receiver receiver = {port, (void *)memory, checksum((unsigned char *)memory, OOL_SIZE)};
    pthread_t thread;
    pthread_create(&thread, NULL, receive_parcel, &receiver);

    parcel out;
    memset(&out, 0, sizeof out);
    out.header.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, 0) | MACH_MSGH_BITS_COMPLEX;
    out.header.msgh_size = sizeof out;
    out.header.msgh_remote_port = port;
    out.header.msgh_id = 501;
    out.body.msgh_descriptor_count = 2;
    out.memory.address = (void *)memory;
    out.memory.size = OOL_SIZE;
    out.memory.deallocate = FALSE;
    out.memory.copy = MACH_MSG_VIRTUAL_COPY;
    out.memory.type = MACH_MSG_OOL_DESCRIPTOR;
    out.port.name = carried;
    out.port.disposition = MACH_MSG_TYPE_MAKE_SEND;
    out.port.type = MACH_MSG_PORT_DESCRIPTOR;
    kern_return_t sent =
        mach_msg(&out.header, MACH_SEND_MSG | MACH_SEND_TIMEOUT, sizeof out, 0, MACH_PORT_NULL, WAIT, MACH_PORT_NULL);
    // The receiver gets the memory as it was when it was sent, whatever happens to it here.
    memset((void *)memory, 0xff, OOL_SIZE);

    received in;
    kern_return_t result = receive(carried, &in, WAIT);
    pthread_join(thread, NULL);
    if (sent != KERN_SUCCESS || receiver.result != KERN_SUCCESS)
        printf("complex  failed: send 0x%x receive 0x%x\n", sent, receiver.result);
    else
        printf("complex  complex %d descriptors %d: %d bytes out of line at another address %d intact %d freed %d, "
               "send right %d carried a message %d\n",
               receiver.complex, receiver.descriptors, receiver.size, receiver.elsewhere, receiver.intact, receiver.freed,
               receiver.send_right, receiver.used && result == KERN_SUCCESS && in.message.header.msgh_id == 500);
    vm_deallocate(mach_task_self(), memory, OOL_SIZE);
    mach_port_mod_refs(mach_task_self(), carried, MACH_PORT_RIGHT_RECEIVE, -1);
    drop_port(port);
}

// f. A kernel timer is a port that gets a message when its deadline, in mach_absolute_time
// units, has passed.
static void kernel_timer(void) {
    mach_timebase_info_data_t timebase;
    mach_timebase_info(&timebase);
    mach_port_t timer = mk_timer_create();
    uint64_t start = mach_absolute_time();
    kern_return_t armed = mk_timer_arm(timer, start + 5 * NSEC_PER_MSEC * timebase.denom / timebase.numer);
    received in;
    kern_return_t result = receive(timer, &in, WAIT);
    printf("mk_timer created %d armed %d fired %d after 5 ms %d destroyed %d\n", timer != MACH_PORT_NULL, armed == KERN_SUCCESS,
           result == KERN_SUCCESS, milliseconds_since(start) >= 5, mk_timer_destroy(timer) == KERN_SUCCESS);
}

// g. libdispatch watches the port and runs the handler, which receives the message itself. The
// port may only go once the source has been cancelled, in its cancel handler.
static void dispatch_mach_source(void) {
    mach_port_t port = new_port();
    dispatch_queue_t queue = dispatch_queue_create("ipswrun.ipc.source", DISPATCH_QUEUE_SERIAL);
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    dispatch_source_t source = dispatch_source_create(DISPATCH_SOURCE_TYPE_MACH_RECV, port, 0, queue);
    __block int handled = 0, id = 0, cancelled = 0;
    dispatch_source_set_event_handler(source, ^{
        received in;
        if (receive(port, &in, 0) != KERN_SUCCESS) return;
        handled++;
        id = in.message.header.msgh_id;
        dispatch_semaphore_signal(done);
    });
    dispatch_source_set_cancel_handler(source, ^{
        drop_port(port);
        cancelled = 1;
        dispatch_semaphore_signal(done);
    });
    dispatch_resume(source);

    kern_return_t sent = send_text(port, MACH_PORT_NULL, 700, "to a dispatch source");
    long timed_out = dispatch_semaphore_wait(done, patience());
    printf("source   sent %d, handler received %d message id %d timed out %ld\n", sent == KERN_SUCCESS, handled, id, timed_out);
    dispatch_source_cancel(source);
    timed_out = dispatch_semaphore_wait(done, patience());
    printf("source   cancelled %d timed out %ld\n", cancelled, timed_out);
}

// h. A run loop waits on the ports of its sources and timers in one receive.

static const char *run_result(CFRunLoopRunResult result) {
    switch (result) {
    case kCFRunLoopRunFinished: return "finished";
    case kCFRunLoopRunStopped: return "stopped";
    case kCFRunLoopRunTimedOut: return "timed out";
    case kCFRunLoopRunHandledSource: return "handled a source";
    }
    return "unknown";
}

static void tick(CFRunLoopTimerRef timer, void *info) {
    if (++*(int *)info < 3) return;
    CFRunLoopTimerInvalidate(timer);
    CFRunLoopStop(CFRunLoopGetCurrent());
}

struct delivery {
    int calls, id;
    char text[40];
};

static void deliver(CFMachPortRef port, void *msg, CFIndex size, void *info) {
    struct delivery *delivery = info;
    message *in = msg;
    delivery->calls++;
    delivery->id = in->header.msgh_id;
    if (in->header.msgh_size >= sizeof *in) strlcpy(delivery->text, in->text, sizeof delivery->text);
}

static void *send_later(void *arg) {
    usleep(10000);
    return (void *)(long)send_text((mach_port_t)(long)arg, MACH_PORT_NULL, 800, "to a CFMachPort");
}

static void run_loop(void) {
    int ticks = 0;
    CFRunLoopTimerContext timer_context = {0, &ticks, NULL, NULL, NULL};
    CFRunLoopTimerRef timer = CFRunLoopTimerCreate(NULL, CFAbsoluteTimeGetCurrent() + 0.01, 0.01, 0, 0, tick, &timer_context);
    CFRunLoopAddTimer(CFRunLoopGetCurrent(), timer, kCFRunLoopDefaultMode);
    CFRunLoopRunResult result = CFRunLoopRunInMode(kCFRunLoopDefaultMode, WAIT / 1000.0, false);
    printf("runloop  timer every 10 ms fired %d times, run %s\n", ticks, run_result(result));
    CFRunLoopTimerInvalidate(timer);
    CFRelease(timer);

    struct delivery delivery = {0, 0, ""};
    CFMachPortContext port_context = {0, &delivery, NULL, NULL, NULL};
    CFMachPortRef port = CFMachPortCreate(NULL, deliver, &port_context, NULL);
    CFRunLoopSourceRef source = CFMachPortCreateRunLoopSource(NULL, port, 0);
    CFRunLoopAddSource(CFRunLoopGetCurrent(), source, kCFRunLoopDefaultMode);
    pthread_t thread;
    void *sent;
    pthread_create(&thread, NULL, send_later, (void *)(long)CFMachPortGetPort(port));
    result = CFRunLoopRunInMode(kCFRunLoopDefaultMode, WAIT / 1000.0, true);
    pthread_join(thread, &sent);
    printf("runloop  CFMachPort sent %d, callback ran %d time with message %d \"%s\", run %s\n",
           (kern_return_t)(long)sent == KERN_SUCCESS, delivery.calls, delivery.id, delivery.text, run_result(result));
    CFRunLoopRemoveSource(CFRunLoopGetCurrent(), source, kCFRunLoopDefaultMode);
    CFMachPortInvalidate(port);
    CFRelease(source);
    CFRelease(port);

    // The main thread's run loop always has the main dispatch queue to wait for, so it is never
    // out of sources: with nothing to do it runs for as long as it was given.
    uint64_t start = mach_absolute_time();
    result = CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.05, false);
    printf("runloop  nothing to do for 50 ms %d, run %s\n", milliseconds_since(start) >= 50, run_result(result));
}

// i. and j. XPC.

static const char *xpc_name(xpc_object_t object) {
    if (object == NULL) return "nothing in time";
    if (object == XPC_ERROR_CONNECTION_INVALID) return "XPC_ERROR_CONNECTION_INVALID";
    if (object == XPC_ERROR_CONNECTION_INTERRUPTED) return "XPC_ERROR_CONNECTION_INTERRUPTED";
    if (object == XPC_ERROR_TERMINATION_IMMINENT) return "XPC_ERROR_TERMINATION_IMMINENT";
    if (xpc_get_type(object) == XPC_TYPE_DICTIONARY) return "a dictionary";
    if (xpc_get_type(object) == XPC_TYPE_ERROR) return "another error";
    return "something else";
}

// i. A listener without a name is reached through an endpoint, which could travel in a message
// to another process; here the client is in the same one.
static void xpc_in_process(void) {
    dispatch_queue_t serving = dispatch_queue_create("ipswrun.ipc.listener", DISPATCH_QUEUE_SERIAL);
    dispatch_queue_t events = dispatch_queue_create("ipswrun.ipc.client", DISPATCH_QUEUE_SERIAL);
    dispatch_semaphore_t peer_closed = dispatch_semaphore_create(0), client_closed = dispatch_semaphore_create(0),
                         answered = dispatch_semaphore_create(0);
    __block int peers = 0, requests = 0;

    xpc_connection_t listener = xpc_connection_create(NULL, serving);
    xpc_connection_set_event_handler(listener, ^(xpc_object_t peer) {
        if (xpc_get_type(peer) != XPC_TYPE_CONNECTION) return;
        peers++;
        xpc_connection_set_event_handler(peer, ^(xpc_object_t event) {
            if (event == XPC_ERROR_CONNECTION_INVALID) dispatch_semaphore_signal(peer_closed);
            if (xpc_get_type(event) != XPC_TYPE_DICTIONARY) return;
            xpc_object_t reply = xpc_dictionary_create_reply(event);
            if (reply == NULL) return;
            requests++;
            size_t length = 0;
            const void *data = xpc_dictionary_get_data(event, "data", &length);
            const char *text = xpc_dictionary_get_string(event, "text");
            xpc_dictionary_set_string(reply, "text", text ? text : "(no text)");
            xpc_dictionary_set_int64(reply, "number", xpc_dictionary_get_int64(event, "number") + 1);
            xpc_dictionary_set_uint64(reply, "length", length);
            xpc_dictionary_set_uint64(reply, "sum", data ? checksum(data, length) : 0);
            xpc_connection_send_message(xpc_dictionary_get_remote_connection(event), reply);
            xpc_release(reply);
        });
        xpc_connection_resume(peer);
    });
    xpc_connection_resume(listener);

    xpc_endpoint_t endpoint = xpc_endpoint_create(listener);
    xpc_connection_t client = xpc_connection_create_from_endpoint(endpoint);
    xpc_release(endpoint);
    __block xpc_object_t client_event = NULL;
    xpc_connection_set_event_handler(client, ^(xpc_object_t event) {
        if (client_event == NULL) client_event = event; // errors are never freed
        dispatch_semaphore_signal(client_closed);
    });
    xpc_connection_resume(client);

    // Data of this size travels as out-of-line memory, not inside the message.
    static unsigned char data[XPC_DATA_SIZE];
    fill(data, sizeof data);
    xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_string(query, "text", "hello over xpc");
    xpc_dictionary_set_int64(query, "number", 41);
    xpc_dictionary_set_data(query, "data", data, sizeof data);
    xpc_object_t reply = request_with_timeout(client, query, WAIT);
    if (reply == NULL || xpc_get_type(reply) != XPC_TYPE_DICTIONARY) {
        printf("xpc      sync reply: %s\n", xpc_name(reply));
    } else {
        const char *text = xpc_dictionary_get_string(reply, "text");
        printf("xpc      sync reply: text \"%s\" number %lld data %llu bytes intact %d\n", text ? text : "(none)",
               (long long)xpc_dictionary_get_int64(reply, "number"),
               (unsigned long long)xpc_dictionary_get_uint64(reply, "length"),
               xpc_dictionary_get_uint64(reply, "sum") == checksum(data, sizeof data));
        xpc_release(reply);
        xpc_release(query);
    }

    query = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_string(query, "text", "and again");
    xpc_dictionary_set_int64(query, "number", -8);
    __block int64_t number = 0;
    __block const char *kind = "nothing in time";
    xpc_connection_send_message_with_reply(client, query, events, ^(xpc_object_t answer) {
        kind = xpc_name(answer);
        if (xpc_get_type(answer) == XPC_TYPE_DICTIONARY) number = xpc_dictionary_get_int64(answer, "number");
        dispatch_semaphore_signal(answered);
    });
    xpc_release(query);
    long timed_out = dispatch_semaphore_wait(answered, patience());
    printf("xpc      async reply: %s number %lld timed out %ld\n", kind, (long long)number, timed_out);
    printf("xpc      listener got %d connection and %d requests, client no events %d\n", peers, requests, client_event == NULL);

    // Cancelling ends both sides: each handler is told once, and never called again.
    xpc_connection_cancel(client);
    timed_out = dispatch_semaphore_wait(client_closed, patience());
    printf("xpc      cancelled client got %s timed out %ld\n", xpc_name(client_event), timed_out);
    timed_out = dispatch_semaphore_wait(peer_closed, patience());
    printf("xpc      its peer in the listener got XPC_ERROR_CONNECTION_INVALID timed out %ld\n", timed_out);
    xpc_connection_cancel(listener);
}

// j. Nobody is asked whether a service exists until the first message, and the answer is an
// error in place of the reply and another to the event handler.
static void xpc_no_service(void) {
    dispatch_queue_t events = dispatch_queue_create("ipswrun.ipc.missing", DISPATCH_QUEUE_SERIAL);
    dispatch_semaphore_t told = dispatch_semaphore_create(0);
    __block xpc_object_t first_event = NULL;
    xpc_connection_t connection = xpc_connection_create_mach_service("com.example.ipswrun.no-such-service", events, 0);
    xpc_connection_set_event_handler(connection, ^(xpc_object_t event) {
        if (first_event == NULL) first_event = event;
        dispatch_semaphore_signal(told);
    });
    xpc_connection_resume(connection);

    xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_string(query, "text", "anyone there?");
    xpc_object_t reply = request_with_timeout(connection, query, WAIT);
    long timed_out = dispatch_semaphore_wait(told, patience());
    printf("xpc      no such service: reply %s, handler got %s timed out %ld\n", xpc_name(reply), xpc_name(first_event),
           timed_out);
    if (reply == NULL) return;
    xpc_release(reply);
    xpc_release(query);
    xpc_connection_cancel(connection);
}

static const struct {
    char letter;
    void (*run)(void);
} sections[] = {
    {'a', ping},
    {'b', receive_timeout},
    {'c', port_set},
    {'d', receive_large},
    {'e', complex_message},
    {'f', kernel_timer},
    {'g', dispatch_mach_source},
    {'h', run_loop},
    {'i', xpc_in_process},
    {'j', xpc_no_service},
};

int main(int argc, char **argv) {
    setvbuf(stdout, NULL, _IONBF, 0);
    for (size_t i = 0; i < sizeof sections / sizeof sections[0]; i++)
        if (argc < 2 || strchr(argv[1], sections[i].letter)) sections[i].run();
    return 0;
}
