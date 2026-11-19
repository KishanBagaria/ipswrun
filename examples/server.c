// Server guest: a daemon, as launchd would start one. It provides two Mach services to other
// processes and never exits. One is an XPC service that gives every text back in capitals. The
// other is a plain Mach port, checked in the way daemons older than XPC do, that gives memory
// sent out of line back reversed. Both tell the client which process they took it to be.
//
// Under ipswrun it is started beside its client (examples/client.c):
//
//   ipswrun --daemon com.example.ipswrun.echo,com.example.ipswrun.reverse=examples/bin/server <release> examples/bin/client
#include <ctype.h>
#include <dispatch/dispatch.h>
#include <mach/mach.h>
#include <servers/bootstrap.h>
#include <stdio.h>
#include <string.h>
#include <xpc/xpc.h>

#include "service.h"

static void reverse(mach_port_t port) {
    union {
        reverse_message_t message;
        char room[sizeof(reverse_message_t) + sizeof(mach_msg_audit_trailer_t)];
    } in = {0};
    mach_msg_option_t audit = MACH_RCV_TRAILER_TYPE(MACH_MSG_TRAILER_FORMAT_0) | MACH_RCV_TRAILER_ELEMENTS(MACH_RCV_TRAILER_AUDIT);
    if (mach_msg(&in.message.header, MACH_RCV_MSG | audit, 0, sizeof in, port, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL) != KERN_SUCCESS) return;
    reverse_message_t *request = &in.message;
    mach_msg_audit_trailer_t *trailer = (mach_msg_audit_trailer_t *)((char *)request + request->header.msgh_size);
    char *bytes = request->memory.address;
    for (mach_msg_size_t i = 0, j = request->memory.size; i + 1 < j; i++) {
        char c = bytes[i];
        bytes[i] = bytes[--j];
        bytes[j] = c;
    }
    reverse_message_t reply = {0};
    reply.header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_MOVE_SEND_ONCE, 0);
    reply.header.msgh_size = sizeof reply;
    reply.header.msgh_remote_port = request->header.msgh_remote_port;
    reply.header.msgh_id = request->header.msgh_id + 100;
    reply.count = 1;
    reply.memory = request->memory;
    reply.memory.deallocate = 1;
    reply.pid = (int)trailer->msgh_audit.val[5];
    mach_msg(&reply.header, MACH_SEND_MSG, sizeof reply, 0, MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
}

int main(void) {
    dispatch_queue_t queue = dispatch_queue_create("server", DISPATCH_QUEUE_SERIAL);
    static int requests;

    xpc_connection_t listener = xpc_connection_create_mach_service(ECHO_SERVICE, queue, XPC_CONNECTION_MACH_SERVICE_LISTENER);
    xpc_connection_set_event_handler(listener, ^(xpc_object_t event) {
        if (xpc_get_type(event) != XPC_TYPE_CONNECTION) return;
        xpc_connection_t peer = (xpc_connection_t)event;
        xpc_connection_set_event_handler(peer, ^(xpc_object_t message) {
            if (xpc_get_type(message) != XPC_TYPE_DICTIONARY) return;
            char capitals[64] = {0};
            const char *text = xpc_dictionary_get_string(message, "text");
            for (size_t i = 0; text != NULL && text[i] != 0 && i < sizeof capitals - 1; i++) {
                capitals[i] = (char)toupper((unsigned char)text[i]);
            }
            xpc_object_t reply = xpc_dictionary_create_reply(message);
            xpc_dictionary_set_string(reply, "text", capitals);
            xpc_dictionary_set_int64(reply, "requests", ++requests);
            xpc_dictionary_set_int64(reply, "client", xpc_connection_get_pid(peer));
            xpc_dictionary_set_int64(reply, "server", getpid());
            xpc_connection_send_message(peer, reply);
        });
        xpc_connection_resume(peer);
    });
    xpc_connection_resume(listener);

    mach_port_t port = MACH_PORT_NULL;
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wdeprecated-declarations"
    kern_return_t checked = bootstrap_check_in(bootstrap_port, REVERSE_SERVICE, &port);
#pragma clang diagnostic pop
    if (checked != KERN_SUCCESS) {
        fprintf(stderr, "server: bootstrap_check_in of %s: %s\n", REVERSE_SERVICE, bootstrap_strerror(checked));
        return 1;
    }
    dispatch_source_t source = dispatch_source_create(DISPATCH_SOURCE_TYPE_MACH_RECV, port, 0, queue);
    dispatch_source_set_event_handler(source, ^{ reverse(port); });
    dispatch_resume(source);
    dispatch_main();
}
