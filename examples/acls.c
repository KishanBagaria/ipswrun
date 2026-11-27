// A file's access control list: the entries that say who may and may not do what beyond its
// mode, which fseventsd puts on the directory it keeps its records in so that nothing deletes
// them. A list is set, read back entry by entry, replaced and emptied, by path and by
// descriptor, on a file and on a directory. Who the entries name is not asked of anyone here:
// they are named by identifiers made up for it. It prints the same natively on a Mac as under
// ipswrun.
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/acl.h>
#include <sys/stat.h>
#include <unistd.h>
#include <uuid/uuid.h>

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

// entry adds one to a list: whether it allows or denies, whom (by the last byte of a made-up
// identifier), what, and how it is handed down.
static void entry(acl_t *list, acl_tag_t kind, unsigned char whom, acl_perm_t what, acl_flag_t how) {
	acl_entry_t e;
	acl_permset_t perms;
	acl_flagset_t flags;
	uuid_t id = {0xab, 0xcd, 0xef, 0xab, 0xcd, 0xef, 0xab, 0xcd, 0xef, 0xab, 0xcd, 0xef, 0, 0, 0, whom};
	acl_create_entry(list, &e);
	acl_set_tag_type(e, kind);
	acl_set_qualifier(e, id);
	acl_get_permset(e, &perms);
	acl_clear_perms(perms);
	acl_add_perm(perms, what);
	acl_get_flagset_np(e, &flags);
	acl_clear_flags_np(flags);
	if (how)
		acl_add_flag_np(flags, how);
}

// shown prints what a list holds, or that there is none.
static void shown(const char *of, acl_t list) {
	printf("%s:", of);
	if (list == NULL) {
		printf(" none (%s)\n", strerror(errno));
		return;
	}
	acl_entry_t e;
	int n = 0;
	for (int at = ACL_FIRST_ENTRY; acl_get_entry(list, at, &e) == 0; at = ACL_NEXT_ENTRY, n++) {
		acl_tag_t kind = 0;
		acl_permset_mask_t what = 0;
		acl_flagset_t flags;
		acl_get_tag_type(e, &kind);
		acl_get_permset_mask_np(e, &what);
		acl_get_flagset_np(e, &flags);
		unsigned char *id = acl_get_qualifier(e);
		printf(" [%s %d what %#llx%s]", kind == ACL_EXTENDED_ALLOW ? "allow" : kind == ACL_EXTENDED_DENY ? "deny" : "other", id ? id[15] : -1, (unsigned long long)what,
		       acl_get_flag_np(flags, ACL_ENTRY_FILE_INHERIT) ? " handed down to files" : "");
		acl_free(id);
	}
	printf(" %d entries\n", n);
	acl_free(list);
}

int main(void) {
	setvbuf(stdout, NULL, _IOLBF, 0);
	char file[512], dir[512];
	const char *tmp = getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp";
	snprintf(file, sizeof file, "%s/acls.%d", tmp, getpid());
	snprintf(dir, sizeof dir, "%s/acls.%d.d", tmp, getpid());
	int fd = open(file, O_CREAT | O_RDWR, 0644);
	mkdir(dir, 0755);

	shown("a new file's", acl_get_file(file, ACL_TYPE_EXTENDED));
	shown("a new directory's", acl_get_file(dir, ACL_TYPE_EXTENDED));

	acl_t one = acl_init(1);
	entry(&one, ACL_EXTENDED_DENY, 7, ACL_DELETE, 0);
	printf("one entry set on the file: %s\n", said(acl_set_file(file, ACL_TYPE_EXTENDED, one)));
	shown("the file's", acl_get_file(file, ACL_TYPE_EXTENDED));
	shown("by its descriptor", acl_get_fd(fd));
	struct stat st;
	stat(file, &st);
	printf("its mode still %o; made 600: %s", st.st_mode & 07777, said(chmod(file, 0600)));
	shown("; the list then", acl_get_file(file, ACL_TYPE_EXTENDED));

	acl_t two = acl_init(2);
	entry(&two, ACL_EXTENDED_ALLOW, 9, ACL_READ_DATA, 0);
	entry(&two, ACL_EXTENDED_DENY, 3, ACL_WRITE_DATA, 0);
	printf("two entries set by the descriptor, an allow before a deny: %s\n", said(acl_set_fd(fd, two)));
	shown("the file's", acl_get_file(file, ACL_TYPE_EXTENDED));

	acl_t handed = acl_init(1);
	entry(&handed, ACL_EXTENDED_DENY, 5, ACL_DELETE_CHILD, ACL_ENTRY_FILE_INHERIT);
	printf("one that is handed down, on the directory: %s\n", said(acl_set_file(dir, ACL_TYPE_EXTENDED, handed)));
	shown("the directory's", acl_get_file(dir, ACL_TYPE_EXTENDED));
	char inside[600];
	snprintf(inside, sizeof inside, "%s/made", dir);
	close(open(inside, O_CREAT | O_WRONLY, 0644));
	shown("a file made in it has", acl_get_file(inside, ACL_TYPE_EXTENDED));
	printf("that file removed, which the list denies nobody here: %s\n", said(unlink(inside)));

	acl_t empty = acl_init(0);
	printf("a list of none set on the file: %s\n", said(acl_set_file(file, ACL_TYPE_EXTENDED, empty)));
	shown("the file's", acl_get_file(file, ACL_TYPE_EXTENDED));
	printf("a list of none set on the directory: %s\n", said(acl_set_file(dir, ACL_TYPE_EXTENDED, empty)));
	shown("the directory's", acl_get_file(dir, ACL_TYPE_EXTENDED));
	printf("of a file that is not there: set %s", said(acl_set_file(inside, ACL_TYPE_EXTENDED, one)));
	shown(", read", acl_get_file(inside, ACL_TYPE_EXTENDED));

	acl_free(one);
	acl_free(two);
	acl_free(handed);
	acl_free(empty);
	close(fd);
	printf("tidied: %s %s\n", said(unlink(file)), said(rmdir(dir)));
	return 0;
}
