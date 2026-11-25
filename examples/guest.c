// The guest the readme's "verified on" claim refers to: it loads a private framework from the
// build's shared cache and creates a SQLite database in the emulated home, using that build's
// libsqlite3. Its output names the build it ran in, so runs against different images differ only
// where the builds do.
//
//   clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains -o guest guest.c -lsqlite3
//   ipswrun --home out 'https://…/UniversalMac_15.6.1_24G90_Restore.ipsw' ./guest
//
// An argument, if given, is the framework to load instead of the default.
#include <TargetConditionals.h>
#include <dlfcn.h>
#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/sysctl.h>

static void sysctl_string(const char *name, char *out, size_t size) {
	out[0] = 0;
	if (sysctlbyname(name, out, &size, NULL, 0) != 0) {
		perror(name);
		exit(1);
	}
}

static void check(sqlite3 *db, int rc, const char *what) {
	if (rc != SQLITE_OK && rc != SQLITE_ROW && rc != SQLITE_DONE) {
		fprintf(stderr, "%s: %s\n", what, sqlite3_errmsg(db));
		exit(1);
	}
}

int main(int argc, char **argv) {
	char version[64], build[64], path[1024];
	sysctl_string("kern.osproductversion", version, sizeof version);
	sysctl_string("kern.osversion", build, sizeof build);
	printf("%s %s (%s)\n", TARGET_OS_OSX ? "macOS" : "iOS", version, build);

	const char *framework = argc > 1 ? argv[1] : "/System/Library/PrivateFrameworks/SoftLinking.framework/SoftLinking";
	if (!dlopen(framework, RTLD_NOW)) {
		fprintf(stderr, "dlopen: %s\n", dlerror());
		return 1;
	}
	printf("loaded %s\n", framework);

	const char *home = getenv("HOME");
	if (!home) {
		fprintf(stderr, "HOME is not set\n");
		return 1;
	}
	snprintf(path, sizeof path, "%s/guest.sqlite", home);
	sqlite3 *db;
	check(NULL, sqlite3_open(path, &db), "open");
	check(db, sqlite3_exec(db,
		"CREATE TABLE IF NOT EXISTS runs (version TEXT, build TEXT, sqlite TEXT);", NULL, NULL, NULL), "create");
	sqlite3_stmt *insert;
	check(db, sqlite3_prepare_v2(db, "INSERT INTO runs VALUES (?, ?, sqlite_version());", -1, &insert, NULL), "prepare");
	sqlite3_bind_text(insert, 1, version, -1, SQLITE_STATIC);
	sqlite3_bind_text(insert, 2, build, -1, SQLITE_STATIC);
	check(db, sqlite3_step(insert), "insert");
	sqlite3_finalize(insert);

	sqlite3_stmt *count;
	check(db, sqlite3_prepare_v2(db, "SELECT count(*) FROM runs;", -1, &count, NULL), "prepare");
	check(db, sqlite3_step(count), "count");
	printf("sqlite %s: %d row(s) in %s\n", sqlite3_libversion(), sqlite3_column_int(count, 0), path);
	sqlite3_finalize(count);
	check(db, sqlite3_close(db), "close");
	return 0;
}
