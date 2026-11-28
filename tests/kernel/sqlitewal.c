// Two connections to one SQLite database kept with a write-ahead log, in one process and in two.
#include <sqlite3.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>
static int count_cb(void *out, int n, char **v, char **c) { (void)n; (void)c; *(int *)out = atoi(v[0]); return 0; }
static int text_cb(void *out, int n, char **v, char **c) { (void)n; (void)c; snprintf(out, 64, "%s", v[0]); return 0; }
static int run(sqlite3 *db, const char *sql) { char *e = NULL; int r = sqlite3_exec(db, sql, NULL, NULL, &e); if (r) printf("[%s: %s] ", sql, e ? e : "?"); return r; }
int main(void) {
	char path[300]; const char *dir = getenv("TMPDIR"); snprintf(path, sizeof path, "%s/wal-%d.sqlite", dir && *dir ? dir : "/tmp", 0);
	char cmd[400]; snprintf(cmd, sizeof cmd, "%s-wal", path); unlink(path); unlink(cmd); snprintf(cmd, sizeof cmd, "%s-shm", path); unlink(cmd);
	sqlite3 *a, *b; char mode[64] = ""; int n = -1;
	printf("opened: %d %d; ", sqlite3_open(path, &a), sqlite3_open(path, &b));
	sqlite3_exec(a, "PRAGMA journal_mode=WAL", text_cb, mode, NULL);
	printf("journal mode %s\n", mode);
	run(a, "CREATE TABLE t(x)"); run(a, "INSERT INTO t VALUES(1)"); run(a, "INSERT INTO t VALUES(2)");
	sqlite3_exec(b, "SELECT count(*) FROM t", count_cb, &n, NULL);
	printf("written through the first, the second counts %d", n);
	run(b, "INSERT INTO t VALUES(3)");
	sqlite3_exec(a, "SELECT count(*) FROM t", count_cb, &n, NULL);
	printf("; written through the second, the first counts %d\n", n);
	fflush(stdout);
	pid_t child = fork();
	if (child == 0) {
		sqlite3 *c; sqlite3_open(path, &c);
		int r = run(c, "INSERT INTO t VALUES(4)");
		sqlite3_exec(c, "SELECT count(*) FROM t", count_cb, &n, NULL);
		printf("another process inserts: %d, and counts %d\n", r, n);
		sqlite3_close(c); fflush(stdout); _exit(0);
	}
	waitpid(child, NULL, 0);
	sqlite3_exec(a, "SELECT count(*) FROM t", count_cb, &n, NULL);
	printf("after it the first counts %d", n);
	int logged = -1, kept = -1;
	printf("; checkpoint: %d", sqlite3_wal_checkpoint_v2(a, NULL, SQLITE_CHECKPOINT_TRUNCATE, &logged, &kept));
	sqlite3_exec(b, "SELECT sum(x) FROM t", count_cb, &n, NULL);
	printf(", the second sums %d\n", n);
	printf("closed: %d %d\n", sqlite3_close(a), sqlite3_close(b));
	unlink(path);
	return 0;
}
