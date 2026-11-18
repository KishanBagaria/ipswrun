# ipswrun

Run an arm64 Mach-O inside the userland of any macOS build, taken straight from that build's restore image (IPSW) over HTTP range requests. No Mac running that release, no VM, and no download of the 13–26 GB image.

```sh
go build -o ipswrun ./cmd/ipswrun
./ipswrun 'https://…/UniversalMac_15.6.1_24G90_Restore.ipsw' ./guest arg1 arg2
```

The guest is an ordinary executable built on the host (`clang -arch arm64 -mmacosx-version-min=11.0 -Wl,-no_fixup_chains …`). It is loaded by the build's own dyld against the build's own dyld shared cache, so `dlopen` of any system framework gets that release's code. Its stdout and stderr are passed through and its exit status becomes ipswrun's (70 means the run itself failed).

Requires macOS on Apple silicon, Go, and a C compiler (the Xcode command line tools): the image-format libraries it builds on (`go-apfs` and `ipsw`, for their LZFSE and disassembler code) use cgo, so `CGO_ENABLED=0` does not build. ipswrun's own code has no C; Unicorn is bound at run time through [purego](https://github.com/ebitengine/purego). The first run downloads a pinned, hash-verified `libunicorn` into the user cache directory. macOS 15 and later images also need Apple's image key service.

## Options

| Flag | |
| --- | --- |
| `--home <dir>` | host directory the guest sees as its home, `/Users/emu`; files the guest creates there are the usual way to get results out |
| `--env KEY=VALUE` | added to the guest environment (repeatable) |
| `--system <path>` | system-volume file, or directory ending in `/`, the guest may read (repeatable; default: feature flags and `SystemVersion.plist`) |
| `--cache <dir>` | keep what was downloaded between runs |
| `--stream-compressed` | allow downloading one compressed disk image member front to back (see below) |
| `--report <file>` | JSON summary: version, build, shared cache UUID, exit code, transfer |
| `--device`, `--timeout`, `--trace`, `--unicorn`, `--setup` | see `--help` |

A run typically fetches 150–600 MB and takes from twenty seconds to a few minutes, nearly all of it download.

## How it works

- `rangefs` opens the restore image over HTTP ranges: ZIP directory → disk image (AEA-encrypted or UDIF) → GPT → APFS → file. A read fetches exactly the bytes it needs; only a run of small consecutive reads is read ahead (256 KB). A response body that breaks is continued from the byte it reached. Any response that is not exactly the requested range is refused. `--cache` keeps each fetched range as a file named by its offset and length; directories written by earlier versions (fixed 128 KB blocks) are still read.
- `dyldcache` parses the shared cache and produces pages rebased the way the kernel's shared-region pager would (slide info v2, v3, v5).
- `emulator` is a small user-mode Darwin "kernel" on [Unicorn](https://www.unicorn-engine.org). It starts the build's `/usr/lib/dyld` the way XNU does, lets it map its own shared cache, and pages contents in on first touch. BSD system calls, Mach traps and a few MIG routines are implemented just far enough for libSystem, CoreFoundation, Foundation and SQLite. Anything unimplemented stops the run with a message and a guest backtrace instead of guessing. So does `--timeout`: Unicorn is asked whether its timer fired, and a run it interrupted is never resumed.

## Limits

- One thread. There are no other threads, no daemons and no network: work queued to other threads never runs, and a blocking Mach receive is answered with a dead-reply notification, which callers see as an interrupted XPC connection.
- Guest files outside `--home` never reach the host. System files come only from the `--system` allowlist.
- The emulated core has a 44-bit address space, and reports no DIT, SHA-3 or SHA-512 instructions.
- macOS 13 and 14 images keep the shared cache in a deflate-compressed ZIP member. Deflate has no exact entry point other than its start, so it cannot be read by range; `--stream-compressed` downloads that one member (3.6–4.5 GB), checks its size and CRC-32, and reads from the inflated copy. Without the flag the run stops instead.
- Verified on restore images from macOS 11.0.1 to 27.2.1 with one guest, [`examples/guest.c`](examples/guest.c) (loading a private framework and creating a SQLite database); [`examples/matrix.sh`](examples/matrix.sh) repeats that across the releases and diffs each run against what is recorded in `examples/expected`. Other guests will find system calls that are not implemented yet.
