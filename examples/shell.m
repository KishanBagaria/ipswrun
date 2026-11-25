// Starting programs of the system the ways programs usually do: system() and popen(), which
// run the shell, and NSTask. Under ipswrun the shell and the tools are the release's own. It
// prints the same natively on a Mac as under ipswrun. There is no shell on iOS to run.
#import <Foundation/Foundation.h>
#include <stdio.h>
#include <stdlib.h>
int main(void) { @autoreleasepool {
  setvbuf(stdout, NULL, _IOLBF, 0);
  int rc = system("echo from the shell; exit 5");
  printf("system: status %d\n", WEXITSTATUS(rc));
  FILE *f = popen("/bin/echo through popen", "r"); char line[128] = ""; if (f) { fgets(line, sizeof line, f); printf("popen: %s -> %d\n", strtok(line, "\n"), pclose(f)); }
  NSTask *task = [NSTask new]; task.executableURL = [NSURL fileURLWithPath:@"/bin/echo"]; task.arguments = @[@"from", @"NSTask"];
  NSPipe *out = [NSPipe pipe]; task.standardOutput = out; NSError *error = nil;
  if (![task launchAndReturnError:&error]) { printf("NSTask: %s\n", error.description.UTF8String); return 1; }
  NSData *data = [out.fileHandleForReading readDataToEndOfFile]; [task waitUntilExit];
  printf("NSTask: \"%s\", status %d\n", [[[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding] stringByTrimmingCharactersInSet:NSCharacterSet.newlineCharacterSet].UTF8String, task.terminationStatus);
} return 0; }
