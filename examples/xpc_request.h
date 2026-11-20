#pragma once

#include <dispatch/dispatch.h>
#include <xpc/xpc.h>

// xpc_connection_send_message_with_reply_sync waits for as long as it takes, so it is called on
// another thread, which is left behind (with everything it holds) if no reply comes in time.
static xpc_object_t request_with_timeout(xpc_connection_t connection, xpc_object_t query, int64_t timeout_ms) {
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block xpc_object_t reply = NULL;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
        reply = xpc_connection_send_message_with_reply_sync(connection, query);
        dispatch_semaphore_signal(done);
    });
    return dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, timeout_ms * NSEC_PER_MSEC)) == 0 ? reply : NULL;
}
