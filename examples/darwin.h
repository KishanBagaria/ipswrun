// What the guests use of the system that iOS has but its SDK leaves out or marks unavailable to
// apps: launchd's bootstrap calls and XPC's connections to Mach services. Built for macOS, this is
// only the SDK's own headers.
#pragma once
#include <TargetConditionals.h>
#include <dispatch/dispatch.h>
#include <mach/mach.h>
#include <unistd.h>
#include <xpc/xpc.h>

#if TARGET_OS_OSX
#include <servers/bootstrap.h>
#else
kern_return_t bootstrap_look_up(mach_port_t bp, const char *service_name, mach_port_t *sp);
kern_return_t bootstrap_check_in(mach_port_t bp, const char *service_name, mach_port_t *sp);
const char *bootstrap_strerror(kern_return_t r);

// The same symbols as macOS's, declared again without the availability that keeps them from apps.
xpc_connection_t guest_xpc_connection_create_mach_service(const char *name, dispatch_queue_t targetq, uint64_t flags)
    __asm__("_xpc_connection_create_mach_service");
pid_t guest_xpc_connection_get_pid(xpc_connection_t connection) __asm__("_xpc_connection_get_pid");
#define xpc_connection_create_mach_service guest_xpc_connection_create_mach_service
#define xpc_connection_get_pid guest_xpc_connection_get_pid
#endif
