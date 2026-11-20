// Scratch client: asks AOSKit for the anisette headers, which CoreADI gets from adid.
#include <CoreFoundation/CoreFoundation.h>
#include <dlfcn.h>
#include <objc/message.h>
#include <objc/runtime.h>
#include <stdio.h>

int main(void) {
    setvbuf(stdout, NULL, _IOLBF, 0);
    void *aos = dlopen("/System/Library/PrivateFrameworks/AOSKit.framework/AOSKit", RTLD_NOW);
    printf("AOSKit: %s\n", aos ? "loaded" : dlerror());
    Class utilities = objc_getClass("AOSUtilities");
    if (utilities == Nil) { printf("no AOSUtilities\n"); return 1; }
    id (*send)(id, SEL, id) = (id (*)(id, SEL, id))objc_msgSend;
    id headers = send((id)utilities, sel_registerName("retrieveOTPHeadersForDSID:"), (id)CFSTR("-2"));
    if (headers == NULL) { printf("no headers\n"); return 2; }
    CFStringRef text = CFCopyDescription((CFTypeRef)headers);
    char buffer[2048];
    CFStringGetCString(text, buffer, sizeof buffer, kCFStringEncodingUTF8);
    printf("headers: %s\n", buffer);
    return 0;
}
