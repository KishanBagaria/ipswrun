# Limits

What ipswrun cannot run or do yet, beyond the short list in the [readme](readme.md#what-does-not-work), ranked by how much real-world code each limit blocks. "Fixable" means a fix keeps to the project's rules: no guessed success for unimplemented calls, nothing keyed to one build, no kext emulation, and no replacing Apple's libraries.

The biggest gap is that nothing starts Apple's daemons unless asked, so most frameworks quietly lose the services they lean on.

This list was made by reading the code, and then checked by a [survey](#survey-of-a-releases-own-binaries) of a release's own binaries and a run of [everyday commands](#everyday-tools). The [last section](#suspicions-not-yet-tested) lists what most needs a run to confirm.

## Ranked limits

| # | Limit | What it blocks | Where in the code | Fixable? |
| --- | --- | --- | --- | --- |
| 1 | The release's daemons are started only where asked for, and of the ones nearly everything asks notifyd and logd run, and opendirectoryd does not: `--on-demand` starts the daemon or agent of a service when it is looked up ([below](#daemons-on-demand)), and without it a Mach service exists only if it is named with `--service`, `--agent` or `--daemon` | Frameworks silently lose opendirectoryd, distnoted, trustd, securityd, tccd and fontd; daemons that need other daemons | `emulator/launchd.go` (`onDemand`), `emulator/build.go` (`launcher`) | One daemon at a time: each needs what it asks of the kernel and of launchd |
| 2 | A thread takes a signal only as it comes back from the kernel, or when it faults: handlers are run ([below](#signals)), but no thread is ever taken off the CPU for one. The timers that count a process's own time on the CPU (`ITIMER_VIRTUAL`, `ITIMER_PROF`) cannot be started | A handler for a thread that spins without a system call (a `SIGALRM` timeout around a loop that computes), profilers, Mach exception ports | `emulator/signals.go` | Taking a thread off the CPU is limit 7 |
| 3 | Descriptor I/O beyond files, pipes and pairs of sockets: no socket but those of a `socketpair` of stream sockets ([below](#pairs-of-sockets); `socket` answers EACCES). Of a terminal or any other descriptor `select` finds nothing ready, a `poll` that would wait for one stops as unimplemented, and a kqueue's `EVFILT_READ` and `EVFILT_WRITE` never happen, nor do `EVFILT_VNODE` and signals there: a `kevent` that would wait for one of those stops as unimplemented ([below](#kqueue-poll-and-select)) | Unix-socket clients (syslog, mDNSResponder), any network code, descriptors sent over a socket, `dispatch_io` on a file, programs that watch files or read a terminal as it is typed at | `emulator/pipes.go`, `emulator/kqueue.go`, `emulator/kevent.go` | Sockets with names in the file system, yes. A real network is a policy call, since it breaks reproducibility |
| 4 | Accelerate's BLAS and LAPACK: every release's libBLAS runs `cblas_sgemm` and the like on Apple's matrix coprocessor (AMX), whatever chip the guest is told it has, and Unicorn has no AMX; the run stops at the first AMX instruction and names it | Linear algebra through Accelerate, and what is built on it (CoreML's and BNNS's CPU paths, likely vDSP's matrix calls) | `emulator/process.go` (`amx`, `interrupt`) | Yes, [below](#amx-emulation): emulate the coprocessor in the invalid-instruction hook |
| 5 | No hardware beyond the device tree: the I/O Registry holds the restore image's device tree and nothing a driver or the boot loader adds ([below](#the-io-registry)), and a search finds no service but the device itself | Serial number and platform UUID, NVRAM, power, HID, most of MobileGestalt (it answers the marketing name, and no product type) | `emulator/registry.go`, `emulator/iokit.go` | Partly: each further fact needs a source in the restore image |
| 6 | Shared file memory: `MAP_SHARED` of a real file is a private copy, and there is no `msync`. Protections are kept per region (`mmap`, `mprotect`, `mach_vm_protect`, the `mach_vm_map` trap, the shared cache's own) and a forbidden access faults, but memory from the MIG `vm_map` and `mach_vm_remap` is mapped with every protection | SQLite WAL `-shm` files shared by two connections or processes; anything that writes a mapped file | `emulator/bsd.go` (`sysMmap`), `emulator/mach.go` | Shared file mappings, yes, through the `shm_open` path |
| 7 | Threads take turns on one CPU, with no preemption, and `hw.ncpu` is 1 | Spin-waits without a system call hang; nothing runs in parallel, so compute-heavy guests are slow | `emulator/threads.go` | Hard: Unicorn does not resume reliably after a stop from outside a hook |
| 8 | Device and OS coverage: iPhones and iPads from the A12 only; no tvOS, watchOS, visionOS or bridgeOS; no macOS before 11 | iOS 14 to 16 on A9 to A11 devices; the other Apple platforms | `cmd/ipswrun` device tables | A9 to A11 (plain arm64 caches) and tvOS look reachable; watchOS is arm64_32; macOS before 11 has no arm64 code |
| 9 | Hosts: an Apple silicon Mac, or Linux on arm64 or x86-64 with glibc; guests have to be built on a Mac | Intel Macs, Windows, musl Linux, CI without a Mac | `emulator/artifact.go` (pinned libunicorn builds) | Intel Macs and musl need a libunicorn build to pin; building guests on Linux needs an SDK |
| 10 | What the system says of itself is left out where it isn't known for sure ([below](#sysctls-left-out)) | A guest that needs one of those sysctls gets ENOENT, as from a kernel without it | `emulator/bsd.go` (`sysctlValue`, `cpuFamilies`) | Yes, one value at a time, from a real device or the restore image |

## What to fix first

These two would unlock the most code for the least work, and each keeps to the rules above.

1. **The daemons `/bin/sh` wakes under `--on-demand '*'`.** opendirectoryd, configd, fseventsd, dirhelper and diskarbitrationd run, and two things stand before a shell's answer ([below](#daemons-on-demand)): opendirectoryd and dirhelper each wait on the other, and powerd stops the run asking the power root for what is a machine's state. Until a shell gets its answer from opendirectoryd, `--on-demand '*'` is not the default.
2. **Taking a thread off the CPU** (limit 7). Signals, their handlers, faults and timers are there, but a thread that spins takes no signal, and nothing else runs meanwhile.

## Calls that stop, and answers that are not a Mac's

A call, a request or a flavour that has no answer stops the run and names itself. A Mac's kernel says ENOTTY to an `fcntl` request it has none of, and ends a process (SIGKILL, at once) that asks a Mach request of a number it has none of; here both stop the run as unimplemented, there being no telling a number the kernel lacks from one that is only not written here.

With no handler at all, of the calls libraries are known to make: `thread_selfusage`, `audit_session_self`, `debug_syscall_reject_config`, `debug_syscall_reject`, `record_system_event`, `log_data`, `oslog_coproc_reg` and `oslog_coproc`. None of the example guests calls them on the releases tested. `objc_bp_assist_cfg_np`, which libobjc of iOS 16.0 calls as it starts, fails with ENOTSUP, as on a core without the hardware that assists Objective-C breakpoints.

Still to check against a real kernel: the constant errors (`necp_open` and `getaudit_addr`, `coalition_info` and `persona` answer ENOSYS, `socket` EACCES, `work_interval_ctl` and `crossarch_trap` ENOTSUP), which fail but perhaps not as a Mac does.

Calls that are there, and where they stop or differ:

| Call | Limit |
| --- | --- |
| `chown` and its kin | A Mac's kernel lets an owner give a file to another of its own groups; which groups a user is in is not kept, so that is EPERM. A guest's executable and devices are EPERM. Owners and modes are said and not enforced ([below](#files-and-volumes)) |
| `setxattr` and its kin | System files have no extended attributes and take none |
| `setattrlist`, `fsetattrlist` | Stop as unimplemented for any attribute but a file's dates, owner, group, mode and flags |
| `setrlimit`, `getrlimit` | RLIMIT_NPROC's real value depends on the device's memory and is not known, so it stays unlimited |
| `fsync`, `sync` | 0 for what is read from the system volume. `fsync` of shared memory or a kqueue stops as unimplemented |
| `getpriority`, `setpriority` | Darwin's own kinds (`PRIO_DARWIN_THREAD` and the like) stop as unimplemented |
| `iopolicysys` | Setting a policy that would change what files show (access times, skipped modification times, ignored permissions) stops as unimplemented |
| `getlogin` | Stops as unimplemented for a daemon and on iOS, where the name of launchd's own session is not known |
| `getdtablesize` | Unimplemented above 10240, where `kern.maxfilesperproc`, which is not known, could be the lower |
| `setuid`, `seteuid`, `setgid`, `setegid` | 0 to become what the process already is, EPERM for anything else; root becoming another user or group stops as unimplemented, as a process here has one of each |
| `getgroups` | A process is in its own group and no other. A Mac's user is put in more at a login, by opendirectoryd, which does not run here |
| `initgroups` | More than 16 groups is EINVAL and a user is refused (EPERM); what root is told stops. newgrp and ssh-keysign, which ask, are root's on a Mac by their files' modes and a user's here |
| `memorystatus_control` | A program that is not root and has no entitlement for it is refused (EPERM), as measured. The daemons that ask have the entitlement, and what they and root are told was not measured, so they stop |
| `proc_rlimit_control` | A limit on wakeups is taken and nothing kept; the kinds that do something stop |
| `kqueue_workloop_ctl` | Threads have no priorities here, so how a workloop is to be scheduled is taken and not used, and a thread that asks what its priority is would not be told this ([examples/workloops.c](examples/workloops.c)) |
| `semaphore_create`, `semaphore_destroy` | A semaphore is a name and a count and no port: asked what it is, or sent a message, it stops. Destroying one that is waited on stops as unimplemented ([examples/semaphore.c](examples/semaphore.c)) |
| `_exclaves_ctl_trap` (88) | "Not supported" (46) for every chip from before the A18 and the M4, as a Mac with an M3 was measured to answer. For a chip that has exclaves, or one not known, it stops as unimplemented ([examples/boot.c](examples/boot.c)) |
| An int argument | A descriptor is read as an int. An int that is no descriptor (the command of `fcntl`, the operation of `flock`) is still read with whatever is above it in the register, where it is not narrowed by hand. A Mac's kernel reads the lower half only |

### `fcntl`

The requests the example guests and the everyday commands make were measured on macOS 27.2.1 and answered ([examples/rangelocks.c](examples/rangelocks.c), [examples/fileflags.c](examples/fileflags.c)). Of those:

- **Locks on parts of a file** are the open file's (`F_OFD_SETLK`, `F_OFD_GETLK`). Not there, and stopping: waiting for such a lock that another holds (`F_OFD_SETLKW`), a process's own locks (`F_SETLK`, `F_SETLKW`), and such locks on what is no file of the home's or a mount's. adid on a data volume waits for a lock of its own on a file of the directory it keeps there, so the adi guest is run with `--no-data`.
- **`F_GETPROTECTIONCLASS`** is 3 for a file and a directory: that an iPhone's files have this class unless given another is what Apple documents and was not measured. `guarded_open_dprotected_np` is an open, as `guarded_open_np` and `open_dprotected_np` are: the guard is not kept and the class is not enforced.
- **`F_CHECK_LV`** is 0 for a program with a code signature from where it starts in its file, and EPERM for a file that is none and for the start of one that holds several. A team's own libraries, and what else the kernel's check of a library looks at besides its having a signature, were not measured.
- **`F_NOCACHE` and `F_RDAHEAD`** are only said back by `F_GETFL`, there being no such memory; `F_SPECULATIVE_READ` is 0 of a file with nothing done. `F_PREALLOCATE` says all the room asked for was had, with none kept on the host, so that what `stat` says a file takes on the disk does not grow as a Mac's does.
- **The flags of an open file** are kept and said back; appending set afterwards is done, and the rest is only kept, a file being always ready. Not there: not waiting on what the run itself was given to read and write, which stops as unimplemented; a file written through memory that maps it, which a Mac may count as written and was not measured apart from cutting it to a length; and the bit a lock taken at the open leaves (0x4000, as measured), such an open's lock not being taken here.
- **Any other request** stops.

## `__mac_syscall`

The calls guests make are answered as a Mac's kernel answers a process that is not sandboxed, as [examples/macsyscall.c](examples/macsyscall.c), [examples/copied.c](examples/copied.c) and [examples/profiles.c](examples/profiles.c) measure them (natively on macOS 27.2.1, with the arguments the guests pass): AMFI 90; Sandbox 0, 1, 2, 4, 12, 22 and 67; Quarantine 80, 82 and 87. Of a sandbox check (Sandbox 2), the result 2 for a path that does not exist was measured on this Mac only.

**A deliberate exception: a profile that is applied confines nothing.** cfprefsd enters its sandbox as it starts, and a real kernel would hold it to its profile from then on. Here applying the profile succeeds and changes nothing, whether it was compiled from a file (Sandbox 0) or is one the kernel has by name (Sandbox 1, of which only the name is read), and a process that has put one on is still told it is in no sandbox, so a guest can do whatever its profile forbids, and sandbox behaviour can't be studied with ipswrun. This was chosen over stopping, so that cfprefsd, and the prefs guest beside it, run.

Everything else stops as unimplemented: putting on by name a profile the kernel has none for, issuing or consuming a sandbox extension (Sandbox 5 and 6 on this Mac, whose token is the kernel's), and any other policy, call, filter type, flags, descriptor or process not measured.

**The profiles a kernel has by name** (Sandbox 22) are read from each release's own kernelcache, from a table in its sandbox extension that is found by its own shape. On macOS 27.2.1 that gives the 14 names the kernel was measured to have; on macOS 11.6 to 27.0 it gives the same 14, and on 11.0.1 twelve of them, which no kernel was there to check. The three newest releases have a second such table after it, of profiles for the extension's own tests, which the kernel of macOS 27.2.1 was measured not to give by name: the first is taken, on that one measurement. A kernelcache that could not be read is read again and said again by each run that asks. On the six iOS releases no table is found, an iPhone's being made otherwise, and a guest there that asks for a profile by a name with no file stops the run as unimplemented.

The answers were measured on a Mac; iOS guests get the same ones, which is not known to be what an iPhone's kernel answers.

## Preferences on iOS 16 and later

The prefs guest, run beside the release's own cfprefsd, prints `synchronized: 0` on every iOS release from 16.0 that was run (16.0, 18.0, 18.7.12, 26.0, 26.6.2, 27.0, 27.2.1): CoreFoundation never looks up `com.apple.cfprefsd.daemon`, so `CFPreferencesAppSynchronize` returns false, while what was set still reads back from the process itself. On iOS 14 and 15 it looks the daemon up (launchd's routine 804) and synchronizes. Why it doesn't is not known yet.

## The I/O Registry

The registry is the device tree the restore image has for the `--device`, shown as the plane `IODeviceTree`, with the registry's root above it ([examples/registry.c](examples/registry.c), [native.txt](examples/expected/registry/native.txt)). The image holds the tree as the boot loader is given it, not as the boot loader leaves it. What is left out, so as not to be invented:

- **A value the image marks as one for the boot loader to fill,** and **a value that is all zeros**, which is how the image holds the place of most of the others. Measured against a Mac15,9's live registry, over the 42 entries `ioreg` shows of its tree: of the 505 unmarked values the image has, 312 are not zeros, and all but one of those are what the Mac has (the one, `pmap-io-ranges` of `/defaults`, is longer on the Mac); 189 are zeros, of which the Mac has 140 as they are and 49 filled in (`chip-id`, `board-id`, `firmware-version` and the like in `/chosen`). So a property that really is zero (`has-exclaves`) is not there to read either.
- **What only a running device has:** its serial number and platform UUID, everything in `/options` (the NVRAM, which is there as an entry with no properties), the entries the boot loader adds under `/chosen`, and the properties drivers add.
- **Classes, locations and identifiers.** An entry's class is its driver's, so only the device's own (`IOPlatformExpertDevice`, which the kernel itself makes of the tree) is answered; a search for that class finds it, and for any other class finds nothing. `io_object_get_class` of another entry, an entry's location, its registry ID and its path are not answered: the guest gets a send error, and the request is listed when the run fails or under `--missing`. A path with a location in it (`cpu0@0`) stops as unimplemented.
- **The properties of the root, and of an entry the kernel keeps to itself** (`kernel-only`, such as `/chosen/memory-map`), which a Mac shows emptied: they stop as unimplemented.
- **The order of the entries under one.** They come in the tree's order. A Mac's kernel has the device's in another (`chosen`, `aliases`, `memory`, `options`, `cpus`, where the tree has `chosen`, `options`, `cpus`, `aliases`, `memory`).
- **Every plane but the tree's.** Among the services there is the root and the device and nothing under it, as no driver is here: `IOService:/IOResources/AppleKeyStore`, which is there on every device, is not. A search or walk that would leave the device tree's plane from any other entry stops as unimplemented.

To be told of services arriving (`io_service_add_notification_bin`) is to be handed those already there, and no message after: the services of a machine here are all it will have.

Not known to hold on an iPhone: the rule for zeros and the device's name (the first of its `compatible` strings) were measured on a Mac. The iOS runs were read, not compared with a device.

## Survey of a release's own binaries

Every Mach-O executable in `/bin`, `/sbin`, `/usr/bin`, `/usr/sbin` and `/usr/libexec` of macOS 27.2.1 (26B131), 1,293 of them, was run once with no arguments under `--missing` and a 60-second limit (commit `a400cd4`). With no arguments most only start up, or print their usage, so this counts what a program needs to start, not to do its work: what is left of the ranked list (signals, sockets, the daemons) shows in programs that do their work. It was not run on iOS or on another release.

1,215 make no call that is not implemented, and 78 make at least one. What is asked for and not there, by how many programs:

| Programs | What is not there | What it is |
| --- | --- | --- |
| 15 | launchd's routine in message 1073742225 | event streams (routine 401, for a stream such as `com.apple.remoted.service`); launchd's own protocol, not measured |
| 8 | the properties of the registry's root | the kext tools, and `say` |
| 5 | launchd's routine in message 1073742624 | not looked into |
| 4 | `memorystatus_control`, commands 14 (three) and 9 (one) | by daemons that have the entitlement for it, which nothing here can measure with; a program without it is refused, as measured |
| 3 each | `necp_client_action`; a sandbox check of the caller's own pid; the kqueue filter for a file's changes (-4), whose events never happen; the properties of the registry's `memory-map` | |
| 2 each | `sem_open` (findmydeviced, sandboxd); `proc_pid_rusage` flavour 6 (secd, securityd_system); `thread_policy` (3616; TouchBarServer, kernelmanagerd); six IOKit requests (2800, 2801, 2813, 2830, 2854, 2871); host requests 225 and 227 (meminfo, zprint, diagnosticd, logd); three more of launchd's; `__mac_syscall` Quarantine 84 and AMFI 102; the registry's entries in the plane IOService | |
| 1 each | `acct`, `vfs_purge`, `change_fdguard_np` (spotlightknowledged.updater), `quotactl`, `nfssvc`, `posix_spawn` with flag 0x40 (sshd-keygen-wrapper), `proc_info` call 4 (dmesg, the kernel's messages), a sandbox check with no flags, thread requests 3614, 3617, 3618 and 3624, `thread_info` in a flavour that stops (`say`), IOKit request 2860 | |

The names of the Mach routines are from the kernel's source (xnu's `.defs`) and were not checked against a run. A message to no port at all (a lookup had found nothing) is refused as a Mac's kernel refuses it, and is not listed.

Not a call, and so not in the table:

- **`confstr(_CS_DARWIN_USER_CACHE_DIR)` and the like**, which 15 programs end on. The directory is made by the release's `dirhelper`. Started by hand (`--daemon com.apple.bsd.dirhelper=system:/usr/libexec/dirhelper`) it checks in and takes the request. It needs `/var/folders`, which is on a device's data volume and so in no restore image (`--mount /var/folders=<dir>` gives one). It then makes `/var/folders/zz` and marks it with a call of the sandbox's (number 261 on macOS 27.2.1, given the directory and the word `folders`), which on a Mac leaves the directory with the extended attribute `com.apple.rootless` set to `folders`. That call is one of System Integrity Protection's: its number is a release's own, and what it answers can only be measured by a process that is root and entitled by Apple, so it stays unimplemented and dirhelper gives up there. Starting dirhelper when it is looked up would not change that.
- **A guest's output that has to be kept** (several releases, whose outputs are compared, and the guests started beside the first) stops the run, and says so, when it comes to more than 64 MB. The output of a run against one release is passed on as it is written.

## Everyday tools

141 commands that do a tool's work were each run by the release's own `/bin/sh` on macOS 27.2.1 and by a Mac's, in a new directory, and what they printed compared: file tools (`mkdir -p`, `cp -R`, `find`, `grep`, `sed`, `awk`, `tar`, `gzip` and the like), the shell's own language (loops, traps, `read`, here-documents, `wait`, `time`), `zsh` and `bash`, and tools that ask more of the machine (`ps`, `ditto`, `rsync`, `sqlite3`, `defaults`, `osascript`, `swift`). As a run is started by default, 125 print the same, 15 differ and 1 stops. It was one release, and macOS only.

The one that stops: `say`, which asks the properties of the registry's root ([above](#the-io-registry)).

What the 15 want:

- **A daemon or a service** (7): `pgrep` (sysmond), `defaults` of a file by its path (cfprefsd), `host` (`socket` is refused: there is no network), `mdfind`, `osascript`, `swift`, and `nohup`, which asks launchd to detach it from the console and is told there is no such thing. Run with `--on-demand '*'`, none gets as far as its own work: `/bin/sh` itself asks opendirectoryd who the user is as it starts, and opendirectoryd stops the run at `identitysvc` ([below](#daemons-on-demand)).
- **The Mac it was measured on, and not a Mac's kernel** (7): `touch -t` and the time zone that Mac is set to; `logname`, which is root there for how the measuring shell was started; `xattr`'s `com.apple.provenance` and `pax`'s `._` file for it, an attribute that Mac puts on the files the measuring programs write (why it does for these was not looked into; the guests' own attribute calls print the same there); `strings`, which is Xcode's and wants it installed; `whatis`, whose database a Mac builds for itself after it is installed; and `time`, which measures how fast the machine is. None of these is known to be a limit to fix, and the provenance attribute and pax's `._` files are set aside until a Mac is found to add the attribute for every program.
- **`ps`** lists the processes that have a terminal, and here none has one: it prints its heading alone, where the measuring Mac's lists that Mac's own shells.

## Files and volumes

- **Owners and modes are said and not enforced.** What the release's template has for a file or a directory of the data volume and the system volume has for its own is what `stat` and `getattrlist` say, read from the image's inodes by a go-apfs that gives them (pinned in `go.mod` to a checkout beside this one, on its branch `inode-owners`, which is not upstream). Nobody is refused by them: the host's file is the host's user's, left readable and writable (a directory fillable). A guest that is no root may so write where a Mac would refuse it, and a guest's root is refused nothing. Not kept for the next run of a kept volume: an owner a guest's root gave a file. A volume that was copied loses the template's owners, which are known by the host's number for each file. `/etc/master.passwd` is made up here and is said to be the user's, mode 755, where a Mac's is root's and 600.
- **Access control lists are kept and not enforced** ([examples/acls.c](examples/acls.c), the same natively, on the oldest macOS here and on iOS): `chmod_extended` and `fchmod_extended` set a file's list or take it off, the stat calls that tell of a file's security say it back, and what is made in a directory is handed the entries of the directory's list that are for its kind, as the kernel's source has it and as measured of a file. Nobody is refused by an entry. Kept in memory only, so not for the next run of a kept volume, and the lists the image has for its own files are not read. From the source and not measured: a list is its owner's and root's to set, a symbolic link is handed none, and a signal ends a resolver's worker's wait. Stopping: `getattrlist` asked for the security of a file that has a list, a copy (`clonefile`) of one, and a security record that says "no list" given for a file that has one.
- **File flags** (`chflags`, `fchflags`, and the flags `setattrlist` sets; [examples/bsdflags.c](examples/bsdflags.c)): a user's flags are kept and said back, and the system's are refused a user (EPERM). Kept in memory only, so not for the next run of a kept volume, and those the image has for its own files are not read. From the source and not measured: a user is refused of a file that is not its own. Stopping: the flags the kernel acts on (a file that may not be changed or only added to, compressed, tracked, in a vault) and any of the system's set by root.
- **Links** ([examples/links.c](examples/links.c)). `link` of a symbolic link, and `linkat` following one with a whole path, stop as unimplemented. A link that leads out of where it is kept (one of the system volume into the data volume, one under the home or a mount to a path from the root or that climbs out) is followed by `open`, `stat` and its kin, and the calls that change a file by its path; `access`, `chdir` and starting a program through such a link are still refused as the host refuses them (EACCES).
- **`setattrlistat` told to keep beneath the directory** (FSOPT_RESOLVE_BENEATH, as `cp -a` asks): a path that goes up further in, or ends in a link to be followed, stops as unimplemented.
- **`mkfifo`.** The pipe is a name the machine knows to be one, for the run: the host keeps an empty file. It can be seen and removed. Opening it stops as unimplemented, and `mkfifoat` is not there.
- **A copy made by the file system** (`clonefile`, `clonefileat`, `fclonefileat`; [examples/clone.c](examples/clone.c)) is a copy of the bytes, which nothing here can tell from blocks that are shared. A directory, which a Mac clones with all that is in it, stops as unimplemented, and so do the flags that follow no link or keep beneath a directory.
- **A process's descriptors as files** (`/dev/fd/3`, `/dev/stdin` and the like; [examples/devfd.c](examples/devfd.c), [examples/devfdpaths.c](examples/devfdpaths.c)). `statfs` of one answers with the root's, where a Mac answers with its device file system (512-byte blocks, `devfs` on `/dev`, with counts of its own that are not known here). The calls that take a directory to start from (`fchmodat`, `unlinkat`, `renameat` and the like) were not measured with these names and say there is no such file. Whether `access` to read or write follows what the descriptor may do was measured only of one that may do both. Listing `/dev/fd` stops as unimplemented, and `/dev` itself is not there to enter or list.
- **A process's own descriptors, listed** (`proc_pidinfo`'s PROC_PIDLISTFDS, [examples/openfds.c](examples/openfds.c)). The table is larger here than a Mac's as a program starts, dyld having opened the shared cache's files, which a Mac's kernel has mapped already; of a process that began with a descriptor above the first 25, and of another process's descriptors, it stops.
- **A file found by its number** (`fsgetpath`) is one some guest has looked at; a Mac's kernel finds any file there is.
- **The volumes** ([examples/volumes.c](examples/volumes.c)). `statfs`, `getattrlist` and `getfsstat` say three: the system's on `/`, `Data` on `/System/Volumes/Data`, and `Preboot`. A Mac has more mounted (`/dev`, and the volumes it keeps for itself), which are left out. Not known, and left as they are: how an iPhone's volumes are mounted (its `statfs` names one volume, with no flags, only the root is listed, and it is called `Macintosh HD`); the devices' names, which are an older Mac's (`/dev/disk1`, where the Mac measured has `/dev/disk3s1s1`, `/dev/disk3s5` and `/dev/disk3s2`); what is under `/System/Volumes/Data`, which is there as a directory and no more; the flags on the releases before macOS 27.2.1, which are taken to be the same; and `/System/Volumes/Preboot` on macOS 11 and 12, which is not there. A path given with `--mount` is a volume of its own, which `statfs` says is the root's. A directory of the home or the data volume that the host keeps on another file system of its own stays a volume of its own (EXDEV).

### The data volume

`--data <dir>` keeps one, `--no-data` gives none, and by default a macOS run has a new temporary one, filled from the release's `/System/Library/Templates/Data` at the paths its `/usr/share/firmlinks` names ([examples/datavolume.c](examples/datavolume.c)). What is not as on a Mac:

- **iOS has none.** It has no `firmlinks` and no template: its data volume is `/private/var`, and what a new one has was not looked for. `--data` says so and stops.
- **A new Mac's time zone is the template's.** `/etc/localtime` is a link to `US/Pacific` there, which Foundation takes, and libc, which looks for `/var/db/timezone/localtime` (a Mac's `timed` makes it, and nothing here does), says UTC.
- **The files of a directory are copied when it is first entered,** read or not. A directory whose files come to more than 64 MB stops the run and says so; none that was entered here does.
- **`/private/var/select/sh`,** which says which shell `/bin/sh` is, is not there: the release's `sh` says on its standard error that it cannot open it, and runs `bash`, as it does on a Mac where the link is not there.

## Processes, tasks and ports

- **The processes** (`kern.proc`, [examples/procs.c](examples/procs.c)). A Mac lists the kernel too and whatever else it runs; a record's priorities, sizes and terminal are left as zeros, not having been read off a real one; and those of a session or a terminal stop as unimplemented.
- **What a process was started with** (`kern.procargs2`, [examples/procargs.c](examples/procargs.c), [examples/stacktop.c](examples/stacktop.c)) is answered for a process of the caller's user. The kernel's strings are those this machine gives and not all a Mac's (a Mac of macOS 27.2.1 has `security_config` among them, and its values are its own). `kern.procargs`, the older form, stops as unimplemented.
- **`task_read_for_pid`** refuses a user every process but its own (EPERM, as a Mac of macOS 27.2.1 does a program with no entitlement); what Apple's own tools, which have such entitlements, are told is not known. So `ps -A` has launchd as `(launchd)` where a Mac's ps, which is root's by its file's mode, has its path, and zeros for every process but its own.
- **The ports the kernel answers on** ([examples/machports.c](examples/machports.c), [examples/taskport.c](examples/taskport.c)). Not there: the bootstrap port is still a number; the task a call names (the first argument of `mach_port_allocate` and the like) is not looked at; the port a task may be read by is not refused what only the task's own may do (a Mac refuses it memory, KERN_INVALID_ARGUMENT); how often a name whose port is gone is held, and a name with a request to be told when it dies, which may have another bit set on a Mac, were not measured; and `task_threads` is not there. `host_get_special_port` refuses a user, as a Mac does; what root is given is not known and stops.
- **`task_info`** ([examples/taskinfo.c](examples/taskinfo.c)). A flavour that is not answered stops the run and names itself. A thread's time is what the process used while it was the one running, charged when another takes over; a Mac counts an ended thread's time into the task's with a lag, which is not kept.
- **`proc_pidinfo`** ([examples/procinfo.c](examples/procinfo.c), [examples/unique.c](examples/unique.c)). A flavour that is not answered stops the run, and so does any other call of `proc_info` that is not there (listing the pids, a descriptor's record, the usage of a pid). What it says of launchd to root, launchd's flags beyond the two measured, and the flavours ps asks next (a process's task, its threads) are not there. A process's unique number and version are this machine's own count and not a Mac's, which has had a million processes before the first one here; of a program with no UUID, which was not measured, it stops.
- **A thread naming itself, and a daemon keeping the kernel told of work in hand** ([examples/procctl.c](examples/procctl.c)). The name is kept for the thread. Memory is never short here, so nothing is ended for being idle. Stopping, not having been measured: `proc_track_dirty` and the calls beside it of any other process or of one that has ended, and to be ended as soon as it has no work (0x20), which the source has the kernel do at once to one that has none. What a process that becomes another program keeps of it was not measured, and here it keeps nothing.
- **The three ports a task keeps for whoever asks** (`mach_ports_register`, `mach_ports_lookup`; [examples/registered.c](examples/registered.c)). Which of the two forms to answer in is told by how much room the library has to receive, 72 bytes for the one and 64 for the other. A library that asked with any other room would stop.
- **`thread_info`** ([examples/threadinfo.c](examples/threadinfo.c)) says a thread is scheduled as a Mac schedules the threads of a program started plainly (sharing time, at priority 31 of at most 63), which is not how threads take turns here; and its share of the processor, which a Mac works out over time, is said to be none. The thread's ids (flavour 4) and the other kinds stop.
- **`task_policy_set` and `task_policy_get`** ([examples/taskpolicy.c](examples/taskpolicy.c)). Nothing here runs a task sooner or later for it, so what is asked is kept and said back and no more. Not run on iOS, where the source has the kernel refuse a task that says of itself what kind of program it is, which no iPhone was there to measure, so that it stops there. Stopping: what root may say it is, what is usual of the tiers, being told into more room than the words take, the port a task is only read by, and the flavours the kernel has besides (suppression, state, and the tiers one at a time).
- **`proc_info_extended_id`** ([examples/daemonasks.c](examples/daemonasks.c)). launchd's count of the programs a process has run (`rec_execcnt`), which is libxpc's and in no source here, is given the version in the process's audit token, which was not confirmed to be what a Mac's launchd says.
- **`mach_port_get_attributes`** ([examples/portstatus.c](examples/portstatus.c), [native.txt](examples/expected/portstatus/native.txt)) answers a receive right's status and its queue's limit. What is not known for sure stops as unimplemented: the other flavors (the owner, importance, the guard's own info, throttling), and the status of a name that is no port's here. Not checked against a Mac: unguarding a port with a value that is not its guard, for which a Mac's kernel ends the process, also stops as unimplemented. The limit of a queue is kept and read back, and holds no sender to it.
- **A process's number is not given twice in a run.**

## Daemons on demand

`--on-demand <service>` has launchd start the build's own provider of a Mach service when the service is first looked up and no guest provides it: the daemon or agent whose launchd plist names the service, as root or as the user the plist names (by the build's own `/etc/passwd`), or for an agent as the guests' user. A pattern names several services, and `*` every one.

- **Only a lookup starts a daemon.** The other keys of a plist (`LaunchEvents`, `Sockets`, `KeepAlive`, the session an agent is limited to, `RunAtLoad`) are not acted on. A daemon whose plist names a user the build's password file does not have, or whose program is not on the system volume, is not started, and its service is not there.
- **iOS has no plist for cfprefsd as an agent,** so there cfprefsd is started by hand (`--agent` and `--daemon` with its path).
- **Nothing subscribes to a daemon's event stream.** notifyd ([examples/notify.c](examples/notify.c), [native.txt](examples/expected/notify/native.txt)) asks for the events of its stream (routine 403, answered with none) and sends routine 406 with a port to be answered on, which libxpc's own code shows is answered when what subscribes has changed: launchd keeps the right to answer and never does.
- **It is not the default, and `'*'` does not get far:** a daemon that makes a call that is not implemented stops the run and names it, as any guest does.
- **logd** (`com.apple.logd`) runs on macOS, on a data volume and with the notifyd it asks ([examples/oslog.c](examples/oslog.c)). Three things it asks are not there, each named by `--missing`, and it goes on without them: two requests of the host's port (messages 225 and 425, the second for the kernel's extensions, which it says on its standard error it could not read) and a kqueue's watch of a file (`EVFILT_VNODE`). Not on iOS, which has no data volume here.
- **opendirectoryd** (`com.apple.system.opendirectoryd.libinfo`) starts with the defaults (the open system volume gives it `/System/Library/OpenDirectory`, and the data volume says `/private/var/db/dslocal` is root's and mode 700), writes its own preferences, and makes itself the kernel's resolver of users and groups (`identitysvc`). That call was measured in a virtual machine (macOS 27.2, 26B123, with SIP off there and a probe signed with the entitlement `com.apple.private.identitysvc`): without the entitlement everything is refused, a user's and root's alike (EPERM); with it a user is refused, root registers, and only the process that registered is then told how many answers the kernel keeps (500 of groups' members, 250 of users), may set them (EINVAL above 10,000, which is from the source) or deregister. Its workers wait for a question from the kernel, and none comes: nothing here asks who a user is by anything but a number. Stopping: a worker's answer to a question. A shell does not yet get its answer: as it starts, opendirectoryd asks dirhelper something (Mach message 47213) and holds the requests it is sent until it has started, and dirhelper, before it answers, asks opendirectoryd who user 0 is. A Mac's do not wait on each other so, and what differs here before that was not found.
- **The kernel's service for the system's power** (IOPMrootDomain; [examples/power.c](examples/power.c), the same natively and on every macOS release here; not on iOS, whose SDK has no library to register with and where none was measured) is there by its path in the plane of power, by where it is among the services and by its class, can be opened, and takes a program's registration to be told of the system's power, of its own state or of its being busy, none of which changes here. diskarbitrationd, which gave up without it, runs. Its properties are a machine's state and are not given: a few that a Mac's does not have are said to be not there (and, from the source and not measured, the lid's on a machine that is no portable), and asking for any other stops the run, which is where powerd stops (`SleepWakeUUID`, `Supported Features`, `SystemPowerProfileOverrideDict`). The platform's own entry it hangs from among the services is not here, so a walk of that plane stops as before.
- **A kqueue watching a process for the program it becomes** (`NOTE_EXEC`, with its end; [examples/procwatch.c](examples/procwatch.c), the same natively; not on iOS, which has no program for the guest's child to become) is told each time, and goes on watching. Watching for that alone, or for a copy being made (`NOTE_FORK`), stops.
- **What launchd gives a job it starts**, as measured on macOS 27.2.1 from the environment of a Mac's own launch agents: `XPC_SERVICE_NAME`, the job's name, and `XPC_FLAGS=1`, whose first bit libxpc takes to mean launchd manages the process. Without them `configd` took itself for a program started by hand, made a copy of itself to be the daemon, and that copy was refused the service. A guest given with `--daemon` or `--agent` has neither, having no name of a job. What launchd answers a program started from a shell that asks for a job's values (routine 301) is as measured, key by key; what it answers a job of its own was not measured, and such a question is left unanswered, after which libxpc asks launchd nothing more.

## Starting programs

`fork`, `posix_spawn`, `execve` and `wait4` are there ([examples/spawn.c](examples/spawn.c), [native.txt](examples/expected/spawn/native.txt); [examples/fork.c](examples/fork.c), [native.txt](examples/expected/fork/native.txt); [examples/shell.m](examples/shell.m), [examples/inherit.c](examples/inherit.c), [examples/sessions.c](examples/sessions.c), [examples/spawngroup.c](examples/spawngroup.c), [examples/groups.c](examples/groups.c)). What is not there, or not as a Mac has it:

- **`vfork`** stops as unimplemented.
- **A copy's memory is copied when it is made,** every page its parent has touched, where a Mac's kernel copies a page only when one of the two writes to it: a fraction of a second, and more for a parent that has touched more.
- **A script whose program is itself a script** stops as unimplemented ([examples/script.c](examples/script.c)).
- **A thread's own I/O policy** is handed to a copy only, and whether a started program should have it was not measured.
- **A session has no terminal,** and `POSIX_SPAWN_SETSID` stops as unimplemented.
- **No child is ever stopped,** so `wait4` asking for children that went on again (WCONTINUED) finds none.
- **What was not measured stops as unimplemented:** the flags of `posix_spawn` other than those for the signals, the process group and the descriptors to close (starting suspended, `POSIX_SPAWN_SETEXEC`, a new session), its port actions, `wait4` for a process group or with options other than `WNOHANG`, `WUNTRACED` and `WCONTINUED`, and `execve` by a process with more than one thread.
- **The memory of a process that has ended is kept until the run ends.** Closing its engine while others run took memory of theirs with it (a fault inside Unicorn on the next stretch of emulation), so the engines are closed together at the end.

## What a process has used

`getrusage` answers with the machine's own account of the process ([examples/usage.c](examples/usage.c), [examples/everyday.c](examples/everyday.c)). The numbers are not a Mac's, and cannot be: the emulated CPU is many times slower, brings pages in sixteen at a time, and its clock stands still while the host fetches. Signals are counted as zero, and so are the times a thread was made to give up the CPU, as none is. `thread_selfusage` and `proc_pid_rusage` are not there.

## Pipes

`pipe` and its two ends are as on a Mac ([examples/pipes.c](examples/pipes.c), [native.txt](examples/expected/pipes/native.txt); [examples/pipeevents.c](examples/pipeevents.c), [native.txt](examples/expected/pipeevents/native.txt)). What is not there, or not as a Mac has it:

- **The number `fstat` gives each end** is made up by a Mac's kernel, differently every time, and is another made-up number here.
- **A pipe's end is not sent in a message.** A started program or a copy has its parent's descriptors, and that is all.
- **`O_ASYNC`, and the size of a pipe on a system short of memory for pipes**, where a Mac's kernel gives them less. What a kqueue is told of the end of a pipe that is not the one read, or written, was not measured, and stops as unimplemented.

## Pairs of sockets

`socketpair` makes two connected stream sockets of the local domain ([examples/socketpair.c](examples/socketpair.c), [native.txt](examples/expected/socketpair/native.txt)). What is not there stops as unimplemented and names itself:

- **To receive no more** (`shutdown` with `SHUT_RD` or `SHUT_RDWR`, where it would not be ENOTCONN). On a Mac what is then sent to the socket is taken and lost, `poll` no longer says the sender can write, and a sender of more than fits waits for good; how much is taken before that was not measured.
- **Pairs of datagram sockets.** How much one holds depends on how the kernel keeps each datagram (32 one-byte datagrams fill one, or 4000 bytes in four), which is its memory's own arithmetic.
- **`sendmsg` and `recvmsg`,** and with them descriptors sent over a socket; `recvfrom` asking whom it is from, `sendto` with an address, a send of nothing, `MSG_WAITALL` on a socket that does not wait (which waits all the same on a Mac), any option of `getsockopt` and `setsockopt` other than the type, the sizes, the low-water marks, the error, `SO_NREAD`, `SO_NWRITE` and `SO_NOSIGPIPE`, and O_ASYNC.
- **Any other socket:** `socket` answers EACCES, so there is no `bind`, `connect` or `listen`, and no network.

## Signals

Signals are sent, end a process, and have handlers run as a Mac's kernel runs them ([examples/signals.c](examples/signals.c), [examples/handlers.c](examples/handlers.c), [native.txt](examples/expected/handlers/native.txt), [examples/faults.c](examples/faults.c)). What is not as a Mac has it:

- **A signal is taken when a thread comes back from the kernel,** from a system call or a trap, and by no thread that does not: nothing takes a thread off the CPU (limit 7), so a handler for a thread that spins waits with it.
- **A thread that waits for a lock, a Mach message or work** (`pthread_mutex_lock`, `mach_msg`, a parked thread of the work queue) is not brought back for a signal: it takes it when it comes back in its own time. A Mac's kernel interrupts some of these.
- **An instruction the emulated CPU does not know** stops the run and is no SIGILL: it may be the emulator's lack and not the guest's fault (AMX, limit 4). What the chip says of the exception in the context (`esr`) is left out for a fault.
- **The timers of a process's own time on the CPU** are read as not running, and starting one stops as unimplemented.
- **What the context says of the machine beyond its registers** is what was measured on one Mac at a system call: the exception the thread last took is that call, and the state is said to hold no signed pointers, as dyld is told too. One bit a Mac15,9 has set in the saved flags (0x1000) is left out, as it is the chip's.
- **What stops the run as unimplemented:** the signals that stop a process, `kill` of every process (-1), a signal to launchd from root, and a `sigreturn` with a context the kernel did not make. `sigwait` waits for good. No thread is ever cancelled.

## kqueue, poll and select

- **A `poll` that will not wait and finds nothing ready lets the other threads run.** A program that asks again and again until something is ready (`rsync`, of what its other half sends) would otherwise keep the CPU for good, as no thread is taken off it here (limit 7). A loop of any other call that never waits still does.
`kqueue` and `kevent` (and `kevent64`) are there for what can happen here ([examples/kqueue.c](examples/kqueue.c), [native.txt](examples/expected/kqueue/native.txt)): an event the program makes happen itself (`EVFILT_USER`), timers, an end of a pipe or of a socket of a pair that can be read or written, and a process ending. A file (`EVFILT_VNODE`), a signal, a Mach port, any other descriptor, and the end of a pipe that is not the one read or written are accepted and never happen; a `kevent` that would wait while one of those is watched stops as unimplemented. `kevent_qos` on a kqueue with a descriptor answers nothing.

`poll` and `select` answer for the ends of a pipe, a socket of a pair and a file, which can always be read and written ([examples/poll.c](examples/poll.c), [native.txt](examples/expected/poll/native.txt)). Of a terminal `select` finds nothing ready, and a `poll` that would wait for a terminal or a kqueue stops as unimplemented.

## Sysctls left out

A sysctl whose value is not known for sure for the release and device is not answered (ENOENT), rather than answered with another chip's or release's value:

- `hw.cpufamily` of a device not in `cpuFamilies`: of the Macs, only those with an M1, M1 Pro, Max or Ultra are there. The commpage's family is then 0 too.
- `hw.l1icachesize`, `hw.l1dcachesize` and `hw.l2cachesize`, except on an M1 Mac: an iPhone's or iPad's chip has its own.
- `hw.cpusubfamily` and the `hw.perflevel0` fields other than its core counts (`name`, `l2cachesize`, `cpusperl2`, `physicalcpu_max`), which libBLAS asks for and does without.
- `kern.version`, which names the build's kernel and when it was built; `kern.iossupportversion`, which is a Mac release's own (in its `SystemVersion.plist`); `kern.osproductversioncompat`, which a Mac of macOS 27 leaves empty; `kern.maxfilesperproc`, which follows the device's memory.
- `hw.cpubrand`, which no kernel has (a Mac's brand is `machdep.cpu.brand_string`).

What describes the emulated machine itself stays: one CPU (`hw.ncpu` and the `perflevel0` counts), 8 GB, half of it free (`kern.memorystatus_level`, and 50 from `memorystatus_get_level` and `host_statistics`). `kern.boottime` is when the machine's own counter was at nothing; a Mac's kernel says a time some seconds after that (9.6 on the Mac measured), those it took to come up. `kern.bootsessionuuid` is the same on every run, as the machine's own UUID and its entropy are.

What a sysctl is called and is (`{0, 1, ...}` and `{0, 4, ...}`) was measured on macOS 27.2.1 for the names answered here, and the kinds carry flags measured on that one release. How wide an integer sysctl is still follows the room it is asked into where its kind is not in the table.

## AMX emulation

Apple's matrix coprocessor is reached by instructions in an encoding Arm leaves undefined (`0x00201000`, an operation in bits 5–9 and a register in bits 0–4): set and clear, loads and stores of its X, Y and Z registers, extracts, and the multiply-accumulate, vector and matrix operations in 16-, 32- and 64-bit floating point and integers. Reverse-engineered descriptions of all of them are public. Emulating them in Go, from the hook that now stops the run, would let Accelerate run: a register file per thread, kept across context switches, and each instruction checked against the host's own results on an Apple silicon Mac. Every AMX instruction would leave Unicorn for Go, so it would be correct but slow.

## Out of reach under the current rules

Each of these needs a kernel driver, so with no kext emulation they stay out. Offscreen work that is pure CPU should still run.

- **GUI.** AppKit's `NSApplication` needs WindowServer, and WindowServer needs the framebuffer, IOSurface and GPU drivers. UIKit needs SpringBoard and backboardd on top of that. CoreGraphics bitmap contexts and ImageIO should work. CoreText probably needs fontd.
- **Metal and the GPU.** With no GPU driver, `MTLCreateSystemDefaultDevice` returns nil. CoreImage and CoreML fall back to CPU paths where they have them, and anything built on IOSurface fails.
- **Keychain and data protection.** The data-protection keychain sits on AppleKeyStore. Only non-permanent, iOS-style keys work today, as the readme says.
- **Sandbox, AMFI and TCC enforcement.** Nothing is checked, so sandbox profiles and privacy prompts can't be observed. tccd could run as a daemon, but most of what it guards leads to more drivers.
- **macOS before 11.** Those releases have no arm64 code.

## Suspicions not yet tested

These come from reading the code and are the ones most worth a run on a Mac.

- **JavaScriptCore's JIT.** `MAP_JIT` and `pthread_jit_write_protect_np` use system registers that only Apple's cores have, which is probably an undefined instruction here. `JSC_useJIT=0` should still run JavaScriptCore as an interpreter.
- **SQLite WAL and Core Data.** Core Data opens WAL stores and may hold two connections to one. With `-shm` mapped privately, each connection sees its own copy.
- **os_log output.** Without logd, logs go nowhere; with it ([above](#daemons-on-demand)) they are in its files, which nothing here reads back. `os_log_type_enabled` says debug messages are logged, with logd or without, where a Mac says they are not; why was not looked for. `--env OS_ACTIVITY_DT_MODE=1` may copy them to standard error, which would help when debugging daemons.
- **Speed.** Only one guest thread runs at a time, on one host core. A CPU-bound benchmark, such as 10 MB through CommonCrypto's SHA-256, would put a number on the slowdown against native.
