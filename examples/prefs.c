// Preferences guest: it keeps a count in its preferences, as NSUserDefaults would, and says what
// it found there. Preferences are kept by a daemon, cfprefsd, which is another process: without
// one nothing is remembered.
//
// Under ipswrun the release's own cfprefsd is started beside it, as the user's agent and as the
// system's daemon, and the two variables are taken away by which ipswrun otherwise has
// CoreFoundation keep preferences in the process itself:
//
//   ipswrun --agent 'com.apple.cfprefsd.agent=system:/usr/sbin/cfprefsd agent' \
//           --daemon 'com.apple.cfprefsd.daemon=system:/usr/sbin/cfprefsd daemon' \
//           --env __CFPREFERENCES_AVOID_DAEMON --env CFFIXED_USER_HOME <release> examples/bin/prefs
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
