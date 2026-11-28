// /dev: the devices every program finds there, and a process's descriptors as files. Each part
// is a program of this folder (see tests/suite.h). It prints the same natively on a Mac as
// under ipswrun.
#include "suite.h"

int devnames_main(void), devzero_main(void), devfd_main(void), devfdlist_main(void), devfdpaths_main(void);

int main(void) {
	part("devnames", devnames_main);
	part("devzero", devzero_main);
	part("devfd", devfd_main);
	part("devfdlist", devfdlist_main);
	part("devfdpaths", devfdpaths_main);
	return 0;
}
