// Preferences guest: it keeps a count in its preferences, as NSUserDefaults would, and says what
// it found there. Preferences are kept by a daemon, cfprefsd, which is another process: without
// one nothing is remembered.
//
// Under ipswrun the release's own cfprefsd is started beside it (see the readme), and with a
// --home that is kept, the second run finds what the first left.
#include <CoreFoundation/CoreFoundation.h>
#include <stdio.h>

int main(void) {
    CFStringRef app = CFSTR("com.example.ipswrun.prefs"), key = CFSTR("runs");
    CFIndex runs = 0;
    CFPropertyListRef found = CFPreferencesCopyAppValue(key, app);
    if (found != NULL && CFGetTypeID(found) == CFNumberGetTypeID()) CFNumberGetValue(found, kCFNumberCFIndexType, &runs);
    printf("found runs = %ld\n", (long)runs);
    runs++;
    CFNumberRef next = CFNumberCreate(NULL, kCFNumberCFIndexType, &runs);
    CFPreferencesSetAppValue(key, next, app);
    CFPreferencesSetAppValue(CFSTR("greeting"), CFSTR("hello from the guest"), app);
    printf("synchronized: %d\n", CFPreferencesAppSynchronize(app));
    CFPropertyListRef again = CFPreferencesCopyAppValue(key, app);
    CFIndex read = 0;
    if (again != NULL && CFGetTypeID(again) == CFNumberGetTypeID()) CFNumberGetValue(again, kCFNumberCFIndexType, &read);
    printf("read back runs = %ld\n", (long)read);
    return 0;
}
