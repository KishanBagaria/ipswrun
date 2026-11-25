# Limits

What ipswrun cannot run or do yet, beyond the short list in the [readme](readme.md#what-does-not-work), ranked by how much real-world code each limit blocks. "Fixable" means a fix keeps to the project's rules: no guessed success for unimplemented calls, nothing keyed to one build, no kext emulation, and no replacing Apple's libraries.

The biggest gap is that a guest cannot start another process, and nothing starts Apple's daemons on demand, so most frameworks quietly lose the services they lean on.

This list was made by reading the code, not by running guests against releases. The [last section](#suspicions-not-yet-tested) lists what most needs a run to confirm.

## Ranked limits

| # | Limit | What it blocks | Where in the code | Fixable? |
| --- | --- | --- | --- | --- |
| 1 | No process creation: `fork`, `posix_spawn` and `execve` have no handler and stop the run | `NSTask`, `system()`, `popen`, every command-line tool or daemon that starts a helper, XPC services embedded in frameworks | `emulator/bsd.go`, handler table | Yes. `posix_spawn` maps onto the existing multi-guest `Machine`; `fork` is harder (it copies an engine's pages) |
| 2 | No launch on demand: a Mach service exists only if it is named with `--service`, `--agent` or `--daemon` | Frameworks silently lose notifyd, distnoted, logd, trustd, securityd, tccd, fontd, opendirectoryd and cfprefsd unless each is wired by hand; daemons that need other daemons | `emulator/launchd.go` | Yes. Read the release's own LaunchDaemons and LaunchAgents plists and start the daemon on the first lookup of its service |
| 3 | No signal delivery: `sigaction`, `sigprocmask` and `sigaltstack` are kept and read back, but a handler never runs; `kill` and `pthread_kill` end the run, as does any CPU fault (a guard page included); `setitimer` that starts a timer stops as unimplemented | Crash handlers, `SIGALRM` timeouts, guard-page tricks, Mach exception ports; `abort()` makes ipswrun exit 70 instead of the guest exiting 134 | `emulator/bsd.go` (`sysSigaction`, `sysSetitimer`, `sysKill`), `emulator/process.go` (`interrupt`) | Yes. Build a signal frame and return through `__sigtramp` and `sigreturn` |
| 4 | No descriptor I/O beyond files: no `pipe`, `socketpair` or sockets (`socket` answers EACCES); `kevent` and `kevent64` on a guest's own kqueue take changes and answer `EV_RECEIPT`, but a wait for events stops as unimplemented; `select` finds nothing ready; `EVFILT_READ`, `EVFILT_WRITE`, `EVFILT_VNODE` and `EVFILT_PROC` never fire | `NSPipe`, `dispatch_io`, `NSFileHandle` readability, `CFSocket`, Unix-socket clients (syslog, mDNSResponder), any network code | `emulator/files.go` (descriptor kinds), `emulator/kevent.go` (`change`) | Pipes and socketpairs, yes. A real network is a policy call, since it breaks reproducibility |
| 5 | Accelerate's BLAS and LAPACK: every release's libBLAS runs `cblas_sgemm` and the like on Apple's matrix coprocessor (AMX), whatever chip the guest is told it has, and Unicorn has no AMX; the run stops at the first AMX instruction and names it | Linear algebra through Accelerate, and what is built on it (CoreML's and BNNS's CPU paths, likely vDSP's matrix calls) | `emulator/process.go` (`amx`, `interrupt`) | Yes, [below](#amx-emulation): emulate the coprocessor in the invalid-instruction hook |
| 6 | No hardware at all: IOKit finds nothing, and no IORegistry properties are served | MobileGestalt, serial number and platform UUID, power, HID, and on iOS most facts about the device | `emulator/iokit.go` | Partly: a read-only IODeviceTree from the restore image's own DeviceTree file, without drivers |
| 7 | Shared file memory: `MAP_SHARED` of a real file is a private copy, and there is no `msync`. Protections are kept per region (`mmap`, `mprotect`, `mach_vm_protect`, the `mach_vm_map` trap, the shared cache's own) and a forbidden access faults, but memory from the MIG `vm_map` and `mach_vm_remap` is mapped with every protection | SQLite WAL `-shm` files shared by two connections or processes; anything that writes a mapped file | `emulator/bsd.go` (`sysMmap`), `emulator/mach.go` | Shared file mappings, yes, through the `shm_open` path |
| 8 | Threads take turns on one CPU, with no preemption, and `hw.ncpu` is 1 | Spin-waits without a system call hang; nothing runs in parallel, so compute-heavy guests are slow | `emulator/threads.go` | Hard: Unicorn does not resume reliably after a stop from outside a hook |
| 9 | The system volume is closed by default: only feature flags, `SystemVersion.plist`, ICU data and the Objective-C trampolines can be read, and the directories (and the symbolic links among them) on the way to the shared cache's images, which dyld walks, listing only what leads there | Frameworks' `Resources` (Info.plist, `.strings`, `.car`), fonts and other `/System/Library` data: frameworks behave differently with no error | `emulator/build.go` (`DefaultSystem`) | Yes, it is a default; `--system` already opens more |
| 10 | Device and OS coverage: iPhones and iPads from the A12 only; no tvOS, watchOS, visionOS or bridgeOS; no macOS before 11 | iOS 14 to 16 on A9 to A11 devices; the other Apple platforms | `cmd/ipswrun` device tables | A9 to A11 (plain arm64 caches) and tvOS look reachable; watchOS is arm64_32; macOS before 11 has no arm64 code |
| 11 | Hosts: an Apple silicon Mac, or Linux on arm64 or x86-64 with glibc; guests have to be built on a Mac | Intel Macs, Windows, musl Linux, CI without a Mac | `emulator/artifact.go` (pinned libunicorn builds) | Intel Macs and musl need a libunicorn build to pin; building guests on Linux needs an SDK |
| 12 | What the system says of itself is left out where it isn't known for sure ([below](#sysctls-left-out)) | A guest that needs one of those sysctls gets ENOENT, as from a kernel without it | `emulator/bsd.go` (`sysctlValue`, `cpuFamilies`) | Yes, one value at a time, from a real device or the restore image |

## What to fix first

These four would unlock the most code for the least work, and each keeps to the rules above.

1. **Launch on demand from the release's own plists.** On a lookup of an unknown name, find the plist under `/System/Library/LaunchDaemons` or `LaunchAgents` whose `MachServices` holds it, and start its `Program` as a daemon or agent. This makes the hand-wired `prefs` and `adi` setups the default, and shows for each release which daemon a framework actually needed. Each daemon that starts will surface missing calls of its own, so expect follow-up work.
2. **`posix_spawn` of system binaries.** A spawned child becomes another `Process` on the same `Machine`, with its own pid, its standard descriptors wired to the parent's, and `wait4` for its exit. It needs pipes (the next item) to be useful for `NSTask`. `fork` can wait.
3. **Pipes and socketpairs, with `EVFILT_READ` and `EVFILT_WRITE`.** An in-emulator byte buffer per pair, read and written through the existing descriptor table, with kevent readiness and blocking reads that wait like the other kinds of wait. This also lets `select` and plain `kevent` answer truthfully instead of returning nothing.
4. **Signal delivery.** Run the handlers `sigaction` now keeps, deliver `kill` and `pthread_kill` to the target thread, and turn a CPU fault into SIGSEGV or SIGBUS when a handler is installed. Without a handler the run still ends, but with the exit status a real process gets (134 for `abort`).

## Stubs that answer success

The `trivial` map in `emulator/bsd.go` used to answer 0 for calls that did nothing. These now do what they say, or stop as unimplemented where they cannot:

| Call | Now |
| --- | --- |
| `chmod`, `fchmod`, `chown`, `lchown`, `fchown` | Change the host file under the home or a mount. A file of the system volume is refused as a real one is: EPERM for the user, who does not own it, EROFS for root. A guest's executable and devices are EPERM. `chown` to the owner a file already has succeeds, to another is EPERM for the user, and stops as unimplemented for root |
| `setxattr`, `getxattr`, `listxattr`, `removexattr` and their `f` forms | Kept in a table in the `Machine`, keyed by the host file, so they read back alike on macOS and Linux hosts and between guests; dropped with the file's last link. System files have none and take none |
| `utimes`, `futimes`, `setattrlist`, `fsetattrlist` | Set the host file's dates; `setattrlist` also owner, group and mode, and stops as unimplemented for any other attribute |
| `flock` | The host's `flock`, so guests sharing a mount lock each other, and so do host processes. A blocking lock waits like the other kinds of wait |
| `mprotect`, `vm_protect` | Tracked per region; a forbidden access faults (see limit 7). It costs 2–4% on the example guests |
| `setrlimit`, `getrlimit` | Start as launchd gives them (no core files; an 8 MB stack, less a guard page, of at most 64 MB; 256 descriptors), and change as the kernel allows: a soft limit no higher than the hard one, a hard one raised only by root. The descriptors' limit is kept to (EMFILE). RLIMIT_NPROC's real value depends on the device's memory and is not known, so it stays unlimited |
| `fsync`, `sync` | The host's, for host files and the standard descriptors (EINVAL for a pipe, as on a Mac); 0 for what is read from the system volume. `fsync` of shared memory or a kqueue stops as unimplemented |
| `umask` | Kept, from launchd's 022, and applied to the files and directories a guest creates in place of the host's own umask |
| `getpriority`, `setpriority` | The process's nice value; a user may raise it but not lower it again. Darwin's own kinds (`PRIO_DARWIN_THREAD` and the like) stop as unimplemented |
| `iopolicysys` | Kept and read back by scope and type. Setting a policy that would change what files show (access times, skipped modification times, ignored permissions) stops as unimplemented |
| `memorystatus_get_level` | 50, as `kern.memorystatus_level` and `host_statistics` say |
| `setsid` | EPERM, as for a process launchd started, which already leads its session and group |
| `sigaction`, `sigprocmask`, `sigaltstack`, `setitimer` | See limit 3 |
| `kevent`, `kevent64` | See limit 4 |

The rest of what the `trivial` map answered 0 now has no handler, and stops the run: `memorystatus_control`, `proc_rlimit_control`, `thread_selfusage`, `kqueue_workloop_ctl`, `audit_session_self`, `debug_syscall_reject_config`, `debug_syscall_reject`, `record_system_event`, `log_data`, `oslog_coproc_reg` and `oslog_coproc`. None of the example guests calls them on the releases tested. `objc_bp_assist_cfg_np`, which libobjc of iOS 16.0 calls as it starts, fails with ENOTSUP, as on a core without the hardware that assists Objective-C breakpoints.

Still to check against a real kernel: the map's constant errors (`necp_open` and `getaudit_addr`, `coalition_info` and `persona` answer ENOSYS, `socket` EACCES, `work_interval_ctl` and `crossarch_trap` ENOTSUP), which fail but perhaps not as a Mac does..

## `__mac_syscall`

The calls guests make are answered as a Mac's kernel answers a process that is not sandboxed, as [examples/macsyscall.c](examples/macsyscall.c) measures them (run natively on macOS 27.2.1, with the arguments the guests pass). How cfprefsd uses them was checked by running a re-signed copy of this Mac's own cfprefsd under lldb.

| Call | Answer |
| --- | --- |
| AMFI 90, `amfi_check_dyld_policy_self` | 0, with the flags 0x1df |
| Sandbox 2, a check, of any process (even one that is not there) | 0, with a result of 0, allowed, for the filter types the guests use (none, a path, 5, 6, 0x1c, 0x41, a descriptor), except a path that does not exist, whose result is 2. That 2 was measured on this Mac only. A descriptor (0xf0) is EBADF, and the result left as it was, if it is not open |
| Sandbox 4, a query of a process | ENOTSUP for its container (kind 0), ESRCH for kind 1; the buffer is not written |
| Sandbox 12, `sandbox_passthrough_access`, as CFPreferences calls it for a plist it opened | 0 for a regular file with the flags 0x2 and 0x100 or neither; EACCES for one with 0x1 and for a directory; EBADF for a descriptor that is not open |
| Sandbox 22, a profile the kernel has by name | ENOENT, as on this Mac for cfprefsd's: libsandbox then compiles the profile itself |
| Sandbox 0, applying a compiled profile | 0, and nothing is confined (below) |
| Sandbox 67, `sandbox_get_container_expected` (iOS 18 and later, as every process starts) | 0, with both words it is given set to 0: no container is expected |
| Quarantine 87, `_qtn_proc_apply_to_pid` | 0, nothing written |

**A deliberate exception: a profile that is applied confines nothing.** cfprefsd enters its sandbox as it starts, and a real kernel would hold it to its profile from then on. Here applying the profile succeeds and changes nothing, so a guest can do whatever its profile forbids, and sandbox behaviour can't be studied with ipswrun. This was chosen over stopping, so that cfprefsd, and the prefs guest beside it, run.

Everything else stops as unimplemented: applying a profile by any other call (Sandbox 1, the kernel's own profiles), issuing or consuming a sandbox extension (Sandbox 5 and 6 on this Mac, whose token is the kernel's), and any other policy, call, filter type, flags, descriptor or process not measured.

The examples' runs let cfprefsd read its profile (`--system /System/Library/Sandbox/Profiles/` beside the defaults), so it compiles and applies it (Sandbox 0) as on a Mac. Without that, its `sandbox_init` fails with "profile not found" and the prefs guest can't synchronize on macOS 15 and later.

The answers were measured on a Mac; iOS guests get the same ones, which is not known to be what an iPhone's kernel answers.

## Preferences on iOS 16 and later

The prefs guest, run beside the release's own cfprefsd, prints `synchronized: 0` on every iOS release from 16.0 that was run (16.0, 18.0, 18.7.12, 26.0, 26.6.2, 27.0, 27.2.1): CoreFoundation never looks up `com.apple.cfprefsd.daemon`, so `CFPreferencesAppSynchronize` returns false, while what was set still reads back from the process itself. On iOS 14 and 15 it looks the daemon up (launchd's routine 804) and synchronizes. Why it doesn't is not known yet; it was so before the sandbox calls were answered.

## Sysctls left out

A sysctl whose value is not known for sure for the release and device is not answered (ENOENT), rather than answered with another chip's or release's value:

- `hw.cpufamily` of a device not in `cpuFamilies`: of the Macs, only those with an M1, M1 Pro, Max or Ultra are there. The commpage's family is then 0 too.
- `hw.l1icachesize`, `hw.l1dcachesize` and `hw.l2cachesize`, except on an M1 Mac: an iPhone's or iPad's chip has its own.
- `hw.cpusubfamily` and the `hw.perflevel0` fields other than its core counts (`name`, `l2cachesize`, `cpusperl2`, `physicalcpu_max`), which libBLAS asks for and does without.
- `kern.version`, which names the build's kernel and when it was built; `kern.iossupportversion`, which is a Mac release's own (in its `SystemVersion.plist`); `kern.osproductversioncompat`, which a Mac of macOS 27 leaves empty; `kern.maxfilesperproc`, which follows the device's memory.
- `hw.cpubrand`, which no kernel has (a Mac's brand is `machdep.cpu.brand_string`).

What describes the emulated machine itself stays: one CPU (`hw.ncpu` and the `perflevel0` counts), 8 GB, half of it free (`kern.memorystatus_level`).

## AMX emulation

Apple's matrix coprocessor is reached by instructions in an encoding Arm leaves undefined (`0x00201000`, an operation in bits 5–9 and a register in bits 0–4): set and clear, loads and stores of its X, Y and Z registers, extracts, and the multiply-accumulate, vector and matrix operations in 16-, 32- and 64-bit floating point and integers. Reverse-engineered descriptions of all of them are public. Emulating them in Go, from the hook that now stops the run, would let Accelerate run: a register file per thread, kept across context switches, and each instruction checked against the host's own results on an Apple silicon Mac. Every AMX instruction would leave Unicorn for Go, so it would be correct but slow.

## Out of reach under the current rules

Each of these needs a kernel driver, so with no kext emulation they stay out. Offscreen work that is pure CPU should still run.

- **GUI.** AppKit's `NSApplication` needs WindowServer, and WindowServer needs the framebuffer, IOSurface and GPU drivers. UIKit needs SpringBoard and backboardd on top of that. CoreGraphics bitmap contexts and ImageIO should work. CoreText also needs `/System/Library/Fonts` opened with `--system`, and probably fontd.
- **Metal and the GPU.** With no GPU driver, `MTLCreateSystemDefaultDevice` returns nil. CoreImage and CoreML fall back to CPU paths where they have them, and anything built on IOSurface fails.
- **Keychain and data protection.** The data-protection keychain sits on AppleKeyStore. Only non-permanent, iOS-style keys work today, as the readme says.
- **Sandbox, AMFI and TCC enforcement.** Nothing is checked, so sandbox profiles and privacy prompts can't be observed. tccd could run as a daemon, but most of what it guards leads to more drivers.
- **macOS before 11.** Those releases have no arm64 code.

## Suspicions not yet tested

These come from reading the code and are the ones most worth a run on a Mac.

- **JavaScriptCore's JIT.** `MAP_JIT` and `pthread_jit_write_protect_np` use system registers that only Apple's cores have, which is probably an undefined instruction here. `JSC_useJIT=0` should still run JavaScriptCore as an interpreter.
- **IORegistry calls.** `emulator/iokit.go` answers only matching, iterators, opening, method calls and closing. A guest that reads the root entry or a property directly probably stops as unimplemented instead of getting an empty answer.
- **SQLite WAL and Core Data.** Core Data opens WAL stores and may hold two connections to one. With `-shm` mapped privately, each connection sees its own copy.
- **os_log output.** Without logd, logs go nowhere. `--env OS_ACTIVITY_DT_MODE=1` may copy them to standard error, which would help when debugging daemons.
- **Speed.** Only one guest thread runs at a time, on one host core. A CPU-bound benchmark, such as 10 MB through CommonCrypto's SHA-256, would put a number on the slowdown against native.
