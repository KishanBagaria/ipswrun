// Which volume a file is on, as a Mac says it. The system and what changes are two volumes of
// one group: stat and getattrlist give the files of both one device, the data volume's, and
// statfs tells them apart, by where each is mounted, by an id of its own for the system's, and
// by how it is mounted. Either id finds a file again (fsgetpath). It prints the same natively on
// a Mac as under ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/attr.h>
#include <sys/fsgetpath.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <unistd.h>

static dev_t first;

static void volume(const char *what, const char *path, int named) {
	struct stat s = {0};
	struct statfs f = {0};
	if (stat(path, &s) || statfs(path, &f)) {
		printf("%s: %s\n", what, strerror(errno));
		return;
	}
	if (!first)
		first = s.st_dev;
	struct attrlist list = {.bitmapcount = ATTR_BIT_MAP_COUNT, .commonattr = ATTR_CMN_DEVID | ATTR_CMN_FSID};
	struct {
		uint32_t length;
		dev_t dev;
		fsid_t fsid;
	} __attribute__((packed)) attrs = {0};
	getattrlist(path, &list, &attrs, sizeof attrs, 0);
	char found[1024] = "", again[1024] = "";
	fsid_t device = {{s.st_dev, 0}};
	ssize_t byVolume = fsgetpath(found, sizeof found, &f.f_fsid, s.st_ino), byDevice = fsgetpath(again, sizeof again, &device, s.st_ino);
	printf("%s: mounted on %s, as an %s volume, flags %#x (read-only %d, local %d)\n", what, f.f_mntonname, f.f_fstypename, f.f_flags, (f.f_flags & MNT_RDONLY) != 0, (f.f_flags & MNT_LOCAL) != 0);
	printf("   its device to stat is the first file's %d, and to getattrlist that too %d, as is the volume getattrlist names %d; the volume statfs names is that device %d\n", s.st_dev == first,
	       attrs.dev == s.st_dev, attrs.fsid.val[0] == s.st_dev, f.f_fsid.val[0] == s.st_dev);
	struct attrlist ofVolume = {.bitmapcount = ATTR_BIT_MAP_COUNT, .volattr = ATTR_VOL_INFO | ATTR_VOL_MOUNTPOINT | ATTR_VOL_NAME | ATTR_VOL_MOUNTFLAGS};
	struct {
		uint32_t length;
		attrreference_t mount, name;
		uint32_t flags;
		char text[2048];
	} __attribute__((packed)) v = {0};
	if (getattrlist(path, &ofVolume, &v, sizeof v, 0))
		printf("   its volume to getattrlist: %s\n", strerror(errno));
	else
		printf("   its volume to getattrlist: \"%s\", mounted on what statfs says %d, with its flags %d\n", (char *)&v.name + v.name.attr_dataoffset,
		       !strcmp((char *)&v.mount + v.mount.attr_dataoffset, f.f_mntonname), v.flags == f.f_flags);
	if (named)
		printf("   found again by the volume statfs names: %s; by its device: %s\n", byVolume > 0 ? found : strerror(errno), byDevice > 0 ? again : strerror(errno));
	else
		printf("   found again by the volume statfs names: %d; by its device: %d\n", byVolume > 0 && !strcmp(found, again), byDevice > 0);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char own[512];
	snprintf(own, sizeof own, "%s/volumes.%d", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp", getpid());
	close(open(own, O_CREAT | O_WRONLY, 0600));
	volume("a program of the system's", "/usr/lib/dyld", 1);
	volume("the root", "/", 1);
	volume("a file of the user's own", own, 0);
	unlink(own);

	// What is mounted: a Mac has more than these two, and these are among them, the root first.
	int count = getfsstat(NULL, 0, MNT_NOWAIT);
	struct statfs *mounts = calloc(count + 1, sizeof *mounts);
	int listed = getfsstat(mounts, (count + 1) * (int)sizeof *mounts, MNT_NOWAIT), root = -1, data = -1;
	for (int i = 0; i < listed; i++) {
		if (!strcmp(mounts[i].f_mntonname, "/"))
			root = i;
		if (!strcmp(mounts[i].f_mntonname, "/System/Volumes/Data"))
			data = i;
	}
	printf("mounted: as many listed as counted %d; the root first %d, read-only %d; the data volume among them %d, as statfs says of a file on it %d\n", count > 0 && listed == count, root == 0,
	       root >= 0 && (mounts[root].f_flags & MNT_RDONLY) != 0, data > 0, data > 0 && mounts[data].f_fsid.val[0] == first);
	printf("with room for one: %d; for less than one: %d; told to wait for each to answer: as many %d\n", getfsstat(mounts, sizeof *mounts, MNT_NOWAIT), getfsstat(mounts, 100, MNT_NOWAIT),
	       getfsstat(NULL, 0, MNT_WAIT) == count);
	return 0;
}
