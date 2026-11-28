// Locks on files: an open file's on parts of a file, a process's own, and a lock on a whole file
// in the one list with them. Each part is a program of this folder (see tests/suite.h). It
// prints the same natively on a Mac as under ipswrun.
#include "suite.h"

int rangelocks_main(void), proclocks_main(void), locklist_main(void);

int main(void) {
	part("rangelocks", rangelocks_main);
	part("proclocks", proclocks_main);
	part("locklist", locklist_main);
	return 0;
}
