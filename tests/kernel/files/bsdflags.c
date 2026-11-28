// The flags a file carries beside its mode (chflags, and the flags setattrlist sets, by which a
// daemon hides the directory it keeps its files in): a user sets and clears those that are a
// user's on what is its own, is refused those that are the system's, and is told them back by
// stat and getattrlist. Those that keep a file from being changed are not set here. It prints
// the same natively on a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/stat.h>
#include <unistd.h>

static const char *said(long r) {
	static char text[8][64];
	static int next;
	char *t = text[next++ % 8];
	if (r < 0)
		snprintf(t, 64, "-1 (%s)", strerror(errno));
	else
		snprintf(t, 64, "%ld", r);
	return t;
}

// flags is what stat says a path's are, a link not followed, and listed is what getattrlist says.
static unsigned flags(const char *path) {
	struct stat st;
	return lstat(path, &st) ? ~0u : st.st_flags;
}

static unsigned listed(const char *path) {
	struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_FLAGS};
	struct {
		uint32_t length, flags;
	} out = {0, ~0u};
	return getattrlist(path, &list, &out, sizeof out, FSOPT_NOFOLLOW) ? ~0u : out.flags;
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char file[512], dir[512], link[512], gone[512];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(file, sizeof file, "%s/bsdflags.%d", tmp, getpid());
	snprintf(dir, sizeof dir, "%s/bsdflags.%d.d", tmp, getpid());
	snprintf(link, sizeof link, "%s/bsdflags.%d.l", tmp, getpid());
	snprintf(gone, sizeof gone, "%s/bsdflags.%d.none", tmp, getpid());
	int fd = open(file, O_CREAT | O_RDWR, 0644);
	mkdir(dir, 0755);
	symlink(file, link);

	printf("a new file's: %#x, a new directory's: %#x, a link's: %#x\n", flags(file), flags(dir), flags(link));
	printf("hidden: %s", said(chflags(file, UF_HIDDEN)));
	printf(", and it is then %#x, listed %#x; not to be dumped too: %s", flags(file), listed(file), said(chflags(file, UF_HIDDEN | UF_NODUMP)));
	printf(", then %#x; none: %s", flags(file), said(chflags(file, 0)));
	printf(", then %#x\n", flags(file));
	printf("by its descriptor, opaque: %s", said(fchflags(fd, UF_OPAQUE)));
	printf(", then %#x; a directory hidden: %s", flags(file), said(chflags(dir, UF_HIDDEN)));
	printf(", then %#x; a link itself: %s", flags(dir), said(lchflags(link, UF_HIDDEN)));
	printf(", then the link %#x and its file %#x\n", flags(link), flags(file));
	printf("through the link: %s", said(chflags(link, UF_NODUMP)));
	printf(", then the link %#x and its file %#x\n", flags(link), flags(file));
	printf("the system's, archived: %s", said(chflags(file, SF_ARCHIVED)));
	printf("; a user's that there is not (0x100): %s", said(chflags(file, 0x100)));
	printf(", then %#x; of a file that is not there: %s", flags(file), said(chflags(gone, UF_HIDDEN)));
	printf("; of a descriptor that is not open: %s\n", said(fchflags(99, UF_HIDDEN)));

	// As setattrlist sets them, alone and with what the Finder keeps of a file.
	struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_FLAGS};
	uint32_t hidden = UF_HIDDEN, nothing = 0, archived = SF_ARCHIVED;
	chflags(file, 0);
	printf("set as an attribute, hidden: %s", said(setattrlist(file, &list, &hidden, sizeof hidden, 0)));
	printf(", then %#x; the system's: %s", flags(file), said(setattrlist(file, &list, &archived, sizeof archived, 0)));
	printf(", then %#x; none: %s", flags(file), said(setattrlist(file, &list, &nothing, sizeof nothing, 0)));
	printf(", then %#x\n", flags(file));
	struct {
		char finder[32];
		uint32_t flags;
	} both = {{0}, UF_HIDDEN};
	both.finder[8] = 0x40; // invisible, as the Finder has it
	list.commonattr = ATTR_CMN_FNDRINFO | ATTR_CMN_FLAGS;
	printf("with the Finder's own, of a directory: %s", said(setattrlist(dir, &list, &both, sizeof both, 0)));
	printf(", then %#x\n", flags(dir));

	// Compressed, of a file that has nothing to say how it is: taken and not kept, which is
	// how tar unpacks every file.
	chflags(file, 0);
	printf("compressed, of a file that is not: %s", said(fchflags(fd, UF_COMPRESSED)));
	printf(", then %#x; with hidden: %s", flags(file), said(chflags(file, UF_COMPRESSED | UF_HIDDEN)));
	printf(", then %#x; of a directory: %s", flags(file), said(chflags(dir, UF_COMPRESSED)));
	printf(", then %#x\n", flags(dir));

	chflags(file, 0);
	chflags(dir, 0);
	lchflags(link, 0);
	close(fd);
	printf("tidied: %s %s %s\n", said(unlink(link)), said(unlink(file)), said(rmdir(dir)));
	return 0;
}
