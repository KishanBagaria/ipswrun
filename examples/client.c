// Client guest: it uses the two Mach services of the server guest (examples/server.c), which is
// another process. Run without that server, on a Mac as under ipswrun, it says that the services
// do not exist.
#include <dispatch/dispatch.h>
#include <mach/mach.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include "darwin.h"
#include "service.h"
#include "xpc_request.h"

#define WAIT 5000 // milliseconds a wait may take before it is given up
#define SIZE 70000

static void echo(void) {
    xpc_connection_t client = xpc_connection_create_mach_service(ECHO_SERVICE, NULL, 0);
    xpc_connection_set_event_handler(client, ^(xpc_object_t event) { (void)event; });
    xpc_connection_resume(client);
    for (int i = 1; i <= 2; i++) {
        xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
        xpc_dictionary_set_string(query, "text", i == 1 ? "hello, server" : "and again");
        xpc_object_t reply = request_with_timeout(client, query, WAIT);
        if (reply == NULL) {
            printf("echo %d: no answer in time\n", i);
        } else if (xpc_get_type(reply) != XPC_TYPE_DICTIONARY) {
            printf("echo %d: %s\n", i, reply == XPC_ERROR_CONNECTION_INVALID ? "no such service" : "the connection was interrupted");
        } else {
            printf("echo %d: \"%s\", the server's request %lld; it is another process: %d, and it knows this one: %d\n", i,
                   xpc_dictionary_get_string(reply, "text"), (long long)xpc_dictionary_get_int64(reply, "requests"),
                   xpc_dictionary_get_int64(reply, "server") != getpid(), xpc_dictionary_get_int64(reply, "client") == getpid());
        }
    }
}

static void reverse(void) {
    mach_port_t service = MACH_PORT_NULL, answer = MACH_PORT_NULL;
    if (bootstrap_look_up(bootstrap_port, REVERSE_SERVICE, &service) != KERN_SUCCESS) {
        printf("reverse: no such service\n");
        return;
    }
    static char bytes[SIZE];
    for (int i = 0; i < SIZE; i++) bytes[i] = (char)(i * 7);
    mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &answer);
    union {
        reverse_message_t message;
        char room[sizeof(reverse_message_t) + sizeof(mach_msg_trailer_t)];
    } buffer = {0};
    reverse_message_t *m = &buffer.message;
    m->header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
    m->header.msgh_size = sizeof *m;
    m->header.msgh_remote_port = service;
    m->header.msgh_local_port = answer;
    m->header.msgh_id = 1;
    m->count = 1;
    m->memory.address = bytes;
    m->memory.size = SIZE;
    m->memory.copy = MACH_MSG_VIRTUAL_COPY;
    m->memory.type = MACH_MSG_OOL_DESCRIPTOR;
    kern_return_t result = mach_msg(&m->header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_RCV_TIMEOUT, sizeof *m, sizeof buffer, answer, WAIT, MACH_PORT_NULL);
    if (result != KERN_SUCCESS) {
        printf("reverse: %s\n", mach_error_string(result));
        return;
    }
    const char *back = m->memory.address;
    int reversed = m->memory.size == SIZE && back != bytes;
    for (int i = 0; reversed && i < SIZE; i++) reversed = back[i] == bytes[SIZE - 1 - i];
    printf("reverse: answer %d with %u bytes, reversed: %d, in memory of its own: %d; the server knows this process: %d\n", m->header.msgh_id,
           m->memory.size, reversed, back != bytes, m->pid == getpid());
}

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    echo();
    reverse();
    return 0;
}
