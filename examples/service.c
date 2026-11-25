// Service guest: what a daemon does. It checks a Mach service in with launchd under a name, and
// a client, here in the same process, finds it by that name and has a request answered.
//
// Only a process that launchd started to provide the service may check its name in, so run on
// its own, on a Mac as under ipswrun, this prints that there is no such service. Under ipswrun
// --service com.example.ipswrun.echo says that this guest is that process.
#include <ctype.h>
#include <dispatch/dispatch.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

#include "darwin.h"
#include "xpc_request.h"

#define NAME "com.example.ipswrun.echo"
#define WAIT 2000 // milliseconds a wait may take before it is given up

static dispatch_time_t patience(void) { return dispatch_time(DISPATCH_TIME_NOW, WAIT * NSEC_PER_MSEC); }

static const char *xpc_name(xpc_object_t object) {
    if (object == NULL) return "nothing in time";
    if (object == XPC_ERROR_CONNECTION_INVALID) return "XPC_ERROR_CONNECTION_INVALID";
    if (object == XPC_ERROR_CONNECTION_INTERRUPTED) return "XPC_ERROR_CONNECTION_INTERRUPTED";
    if (xpc_get_type(object) == XPC_TYPE_DICTIONARY) return "a dictionary";
    return "something else";
}

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    dispatch_queue_t queue = dispatch_queue_create("service", DISPATCH_QUEUE_SERIAL);
    dispatch_semaphore_t refused = dispatch_semaphore_create(0);
    __block int connections = 0, requests = 0, same_process = 0;
    __block xpc_object_t listener_error = NULL;

    // The service: every connection gets its text back in capitals.
    xpc_connection_t listener = xpc_connection_create_mach_service(NAME, queue, XPC_CONNECTION_MACH_SERVICE_LISTENER);
    xpc_connection_set_event_handler(listener, ^(xpc_object_t event) {
        if (xpc_get_type(event) != XPC_TYPE_CONNECTION) {
            listener_error = event;
            dispatch_semaphore_signal(refused);
            return;
        }
        xpc_connection_t peer = (xpc_connection_t)event;
        connections++;
        same_process = xpc_connection_get_pid(peer) == getpid();
        xpc_connection_set_event_handler(peer, ^(xpc_object_t message) {
            if (xpc_get_type(message) != XPC_TYPE_DICTIONARY) return;
            requests++;
            char capitals[64] = {0};
            const char *text = xpc_dictionary_get_string(message, "text");
            for (size_t i = 0; text != NULL && text[i] != 0 && i < sizeof capitals - 1; i++) {
                capitals[i] = (char)toupper((unsigned char)text[i]);
            }
            xpc_object_t reply = xpc_dictionary_create_reply(message);
            xpc_dictionary_set_string(reply, "text", capitals);
            xpc_dictionary_set_int64(reply, "requests", requests);
            xpc_connection_send_message(peer, reply);
        });
        xpc_connection_resume(peer);
    });
    xpc_connection_resume(listener);

    // The client, which knows only the name.
    xpc_connection_t client = xpc_connection_create_mach_service(NAME, queue, 0);
    xpc_connection_set_event_handler(client, ^(xpc_object_t event) { (void)event; });
    xpc_connection_resume(client);
    for (int i = 1; i <= 2; i++) {
        xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
        xpc_dictionary_set_string(query, "text", i == 1 ? "hello, service" : "and again");
        xpc_object_t reply = request_with_timeout(client, query, WAIT);
        if (reply != NULL && xpc_get_type(reply) == XPC_TYPE_DICTIONARY) {
            printf("request %d answered \"%s\", the service's request %lld\n", i, xpc_dictionary_get_string(reply, "text"),
                   (long long)xpc_dictionary_get_int64(reply, "requests"));
        } else {
            printf("request %d not answered: %s\n", i, xpc_name(reply));
        }
    }
    if (connections > 0) {
        printf("the service had %d connection, from this process %d, and %d requests\n", connections, same_process, requests);
    } else {
        int told = dispatch_semaphore_wait(refused, patience()) == 0;
        printf("no such service: the listener was told %d, %s\n", told, told ? xpc_name(listener_error) : "nothing");
    }
    return 0;
}
