// ADI guest: it asks the release's own adid, the daemon that makes the one-time passwords an
// Apple ID signs in with, for one, and prints the answer. With no state to make a password
// from, the answer is that this machine has not been provisioned.
//
// Since macOS 15 adid is an XPC service, and serves only a client whose code signature carries
// the entitlement adi-client with a value it knows, so this guest is signed with one
// (adi.entitlements). Before that it is a Mach service that asks nothing of its client, and
// takes the same request in a message of its own.
//
// Under ipswrun adid is started beside it:
//
//   ipswrun --daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid <release> examples/bin/adi
#include <dispatch/dispatch.h>
#include <libkern/OSByteOrder.h>
#include <mach/mach.h>
#include <servers/bootstrap.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <xpc/xpc.h>

#define WAIT 60000 // milliseconds a wait may take before it is given up: a daemon takes its time to start

// The request for a one-time password, as CoreADI sends it: a version, what is asked, and for
// which account: -2, which is none in particular. All big-endian.
static const uint32_t request[5] = {0x01000000, 0x6ab4e0cf, 0x01000000, 0xffffffff, 0xfeffffff};

static int answer(const char *how, const void *data, size_t length) {
    if (data == NULL || length < 4) {
        printf("adid, %s: an answer with no status\n", how);
        return 1;
    }
    uint32_t status;
    memcpy(&status, data, 4);
    int32_t code = (int32_t)OSSwapBigToHostInt32(status);
    const char *meaning = code == 0 ? "a password" : code == -45061 ? "not provisioned" : code == -45070 ? "this client is not entitled" : "another error";
    printf("adid, %s: status %d (%s), %zu bytes\n", how, code, meaning, length);
    return 0;
}

// The way of an adid that is a Mach service: the request out of line in message 1200, and the
// answer out of line in the reply.
static int by_message(void) {
    typedef struct {
        mach_msg_header_t header;
        mach_msg_size_t descriptors;
        mach_msg_ool_descriptor_t data;
        NDR_record_t ndr;
        mach_msg_size_t count;
    } adi_message_t;
    union {
        adi_message_t message;
        char room[sizeof(adi_message_t) + sizeof(mach_msg_trailer_t)];
    } buffer = {0};
    mach_port_t adid = MACH_PORT_NULL, reply = mig_get_reply_port();
    if (bootstrap_look_up(bootstrap_port, "com.apple.adid", &adid) != KERN_SUCCESS) {
        printf("adid: no such service\n");
        return 1;
    }
    adi_message_t *m = &buffer.message;
    m->header.msgh_bits = MACH_MSGH_BITS_COMPLEX | MACH_MSGH_BITS(MACH_MSG_TYPE_COPY_SEND, MACH_MSG_TYPE_MAKE_SEND_ONCE);
    m->header.msgh_size = sizeof *m;
    m->header.msgh_remote_port = adid;
    m->header.msgh_local_port = reply;
    m->header.msgh_id = 1200;
    m->descriptors = 1;
    m->data.address = (void *)request;
    m->data.size = sizeof request;
    m->data.copy = MACH_MSG_VIRTUAL_COPY;
    m->data.type = MACH_MSG_OOL_DESCRIPTOR;
    m->ndr = NDR_record;
    m->count = sizeof request;
    kern_return_t result = mach_msg(&m->header, MACH_SEND_MSG | MACH_RCV_MSG | MACH_RCV_TIMEOUT, sizeof *m, sizeof buffer, reply, WAIT, MACH_PORT_NULL);
    if (result != KERN_SUCCESS || m->header.msgh_id != 1300 || !(m->header.msgh_bits & MACH_MSGH_BITS_COMPLEX)) {
        printf("adid, by message: %s\n", result != KERN_SUCCESS ? mach_error_string(result) : "not the answer to this");
        return 1;
    }
    return answer("by message", m->data.address, m->data.size);
}

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    xpc_connection_t adid = xpc_connection_create_mach_service("com.apple.adid", NULL, 0);
    xpc_connection_set_event_handler(adid, ^(xpc_object_t event) { (void)event; });
    xpc_connection_resume(adid);

    xpc_object_t query = xpc_dictionary_create(NULL, NULL, 0);
    xpc_dictionary_set_data(query, "data", request, sizeof request);

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
    if (reply == XPC_ERROR_CONNECTION_INVALID) {
        printf("adid: no such service\n");
        return 1;
    }
    if (xpc_get_type(reply) != XPC_TYPE_DICTIONARY) return by_message(); // it hung up: not an XPC service
    size_t length = 0;
    const void *data = xpc_dictionary_get_data(reply, "data", &length);
    return answer("by XPC", data, length);
}
