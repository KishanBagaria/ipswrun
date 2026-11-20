// ADI guest: it asks the release's own adid, the daemon that makes the one-time passwords an
// Apple ID signs in with, for one, and prints the answer. adid serves only a client whose code
// signature carries the entitlement adi-client with a value it knows, so this guest is signed
// with one (adi.entitlements). With no state to make a password from, the answer is that
// this machine has not been provisioned. The question is put as CoreADI puts it since macOS 15,
// over XPC. An earlier adid is asked another way, and hangs up on this one.
//
// Under ipswrun adid is started beside it:
//
//   ipswrun --daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid <release> examples/bin/adi
#include <dispatch/dispatch.h>
#include <libkern/OSByteOrder.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <xpc/xpc.h>

#define WAIT 60000 // milliseconds a wait may take before it is given up: a daemon takes its time to start

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    xpc_connection_t adid = xpc_connection_create_mach_service("com.apple.adid", NULL, 0);
    xpc_connection_set_event_handler(adid, ^(xpc_object_t event) { (void)event; });
    xpc_connection_resume(adid);

    // The request for a one-time password, as CoreADI sends it: a version, what is asked, and for
    // which account: -2, which is none in particular. All big-endian.
    uint32_t words[5] = {OSSwapHostToBigInt32(1), OSSwapHostToBigInt32(0xcfe0b46a), OSSwapHostToBigInt32(1), 0xffffffff,
                         OSSwapHostToBigInt32(0xfffffffe)};
    xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_data(query, "data", words, sizeof words);

    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    __block xpc_object_t reply = NULL;
    dispatch_async(dispatch_get_global_queue(QOS_CLASS_DEFAULT, 0), ^{
        reply = xpc_connection_send_message_with_reply_sync(adid, query);
        dispatch_semaphore_signal(done);
    });
    if (dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, WAIT * NSEC_PER_MSEC)) != 0) {
        printf("adid: no answer in time\n");
        return 1;
    }
    if (xpc_get_type(reply) != XPC_TYPE_DICTIONARY) {
        printf("adid: %s\n", reply == XPC_ERROR_CONNECTION_INVALID ? "no such service" : "the connection was interrupted");
        return 1;
    }
    size_t length = 0;
    const void *data = xpc_dictionary_get_data(reply, "data", &length);
    if (data == NULL || length < 4) {
        printf("adid: an answer with no status\n");
        return 1;
    }
    uint32_t status;
    memcpy(&status, data, 4);
    int32_t code = (int32_t)OSSwapBigToHostInt32(status);
    const char *meaning = code == 0 ? "a password" : code == -45061 ? "not provisioned" : code == -45070 ? "this client is not entitled" : "another error";
    printf("adid: status %d (%s), %zu bytes\n", code, meaning, length);
    return 0;
}
