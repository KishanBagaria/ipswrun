# Research: adid's wire protocol

Working notes and throwaway tooling from finding out how `adid` (the daemon behind an Apple
ID's one-time passwords) is asked things. None of this is product code. It lives on this branch
so that it is not lost, and is not meant to be merged.

Everything below was observed on **macOS 13.0 (22A380)** unless it says otherwise, by running the
release's own `adid` under ipswrun as a second guest.

## What adid is, by release

| Releases | Service | Driver calls (IOKit imports) | Asks about its client |
| --- | --- | --- | --- |
| 11.0.1 – 13.0 | plain Mach service, message 1200 answered by 1300 | none | nothing |
| 13.6 – 27.2.1 | XPC listener, dictionary with a `data` key | 7 (`IOServiceOpen`, `IOConnectCallStructMethod`, …) | code-signing status, DER entitlements, identity, path |

- From 27.2.1's strings and behaviour: it serves a client whose entitlement `adi-client` is a
  string it knows. `3184554750` (commerce's) and `347420559` (akd's) are accepted; other values,
  a boolean, or no entitlement get status -45070.
- State is kept in `/Users/Shared/adi/adi.pb` (11.0.1 also looks at `/var/db/fpsd/adi/adi.pb`).
- 13.6's adid traps inside libxpc under ipswrun; not diagnosed.
- There is no CoreADI library to load on macOS: the framework holds only the daemon. The client
  code is built into AOSKit and akd, with nothing exported.

## The request

The same bytes travel either way: out of line in Mach message 1200 (11.0.1 – 13.0), or as the
`data` of an XPC dictionary (13.6 on). All fields are big-endian.

    [u32 1] [u32 code] [u32 1] [payload]

The answer starts with a big-endian `i32` status. Every answer seen so far is those 4 bytes and
nothing else, because nothing has succeeded yet except code `72096fe6`.

| Status | Seen when |
| --- | --- |
| 0 | success |
| -45019 (`ffff5025`) | unknown code |
| -45020 (`ffff5024`) | third word is not 1 |
| -45034 (`ffff5016`) | payload shorter than the call needs, or a length field larger than the data |
| -45061 (`ffff4ffb`) | not provisioned |
| -45070 (`ffff4ff2`) | client not entitled (27.2.1) |

## The codes (13.0)

adid adds `0x3949f873` to the code and walks a comparison tree. Sending a made-up code and
logging what it was compared against, then probing each constant found, gave twelve codes.
Only the first row is confirmed against Apple's own client (AOSKit's
`+[AOSUtilities retrieveOTPHeadersForDSID:]` sends exactly this, on 11.0.1 and 27.2.1 alike).
The reading of the others is inference from argument shapes and error codes.

| Code | Payload accepted | Answer with no state | Reading |
| --- | --- | --- | --- |
| `cfe0b46a` | account id (u64; -2 is "none in particular") | -45061 | one-time password |
| `716bd86c` | account id, u32 length, blob | -45032 (16-byte blob), -45031 (200-byte blob) | start provisioning |
| `632b8d6e` | u32 session, u32 length, blob, u32 length, blob | -45001 | end provisioning |
| `55a637c0` | u32 session | -45025 | destroy session |
| `385b2ce8` | account id | -45061 | is provisioned |
| `a54951ec` | account id, u32 length, blob | -45061 | needs provisioning first; synchronize? |
| `72096fe6` | nothing | 0 | succeeds unconditionally |
| `85fe63b0` | account id | -45054 | unknown |
| `b0eda7af` | account id | -45054 | unknown |
| `7715488c` | account id | -45062 | unknown |
| `0b2b3196` | more than was tried | -45034 | unknown, long input |
| `9679d4b5` | account id | adid traps (`brk`) | unknown; erase? |

Not known yet: what a successful start-provisioning answer looks like (client blob, session),
whether `716bd86c`/`632b8d6e` really are start/end, and whether the codes are the same on other
releases. All three need a real server blob from Apple's provisioning service.

## Tooling

- `probe/probe-hook.patch` and `probe/probe_tmp.go.txt`: an instruction hook for the emulator.
  Apply the patch, copy the `.txt` to `emulator/probe_tmp.go`, build. With `PROBECMP=<process
  label>` and `PROBEMAGIC=<value>` set, once that process receives message 1200 it follows the
  value through arithmetic and prints what it is compared against. Never commit it to master.
- `probe/walk_codes.py`, `probe_layout.py`, `probe_len.py`: the scripts that walked the
  comparison tree and probed payload shapes. They expect the patched binary as `ipswrun-probe`,
  `guests/adiq.c` built as `adiq`, and a cache directory, beside the `tools` directory they are
  run from; adjust the paths at the top.
- `guests/adiq.c`: sends a request given as hex to adid by message 1200 and prints the answer.
- `guests/otp.c`: asks AOSKit for the anisette headers (the real client's path to adid).
- `guests/mirror.c`: checks that memory mapped twice is one memory.
- `extract`: copies files out of a restore image's system volume (`go run ./research/extract <ipsw url> <cache dir> <out dir> <path>...`).
- `sign`: ad hoc signs a Mach-O with one string entitlement (`go run ./research/sign <in> <out> <identity> <key> <value>`).
- `cross-build.sh`: builds a C guest for macOS arm64 on Linux.

Run with a daemon beside the guest:

    ipswrun --stream-compressed --daemon com.apple.adid=system:/System/Library/PrivateFrameworks/CoreADI.framework/Versions/A/adid 13.0 ./adiq 00000001cfe0b46a00000001fffffffffffffffe
