// The directories of the system that every release has are listed with what is in them: each
// is looked through for a name it must have, and for itself and the one above. It prints the
// same natively on a Mac as under ipswrun.
#include <dirent.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>

static void has(const char *dir, const char *name) {
	DIR *d = opendir(dir);
	if (!d) {
		printf("%s is not opened: %s\n", dir, strerror(errno));
		return;
	}
	int found = 0, self = 0, above = 0, others = 0, kind = 0;
	for (struct dirent *e; (e = readdir(d));) {
		if (!strcmp(e->d_name, name)) {
			found++;
			kind = e->d_type;
		} else if (!strcmp(e->d_name, "."))
			self++;
		else if (!strcmp(e->d_name, ".."))
			above++;
		else
			others++;
	}
	closedir(d);
	printf("%s has %s: %d, of kind %s; itself: %d, the one above: %d; more besides: %d\n", dir, name, found, kind == DT_DIR ? "directory" : kind == DT_REG ? "file" : kind == DT_LNK ? "link" : "other", self, above, others > 0);
}

int main(void) {
	has("/", "System");
	has("/", "usr");
	has("/", "private");
	has("/", "dev");
	has("/System", "Library");
	has("/System/Library", "CoreServices");
	has("/System/Library/CoreServices", "SystemVersion.plist");
	has("/usr", "lib");
	has("/usr/lib", "dyld");
	has("/private", "var");
	has("/private", "etc");
	has("/private/var", "root");
	has("/private/var", "tmp");
	return 0;
}
