# agents.md

ipswrun runs Apple's own userland code from macOS and iOS restore images.

## Design rules

- **Never guess.** An unimplemented syscall, Mach trap, MIG routine or sysctl stops the run and names itself. Never return success because it was enough once. Use `--missing` to survey gaps without stopping.
- **No fake stubs.** A call that does nothing must not report success. Implement it, or fail it the way the real kernel would.
- **Nothing keyed to one build.** No hard-coded addresses, symbol names, struct offsets or vtable slots for a particular release. If it needs a per-release profile, it's the wrong approach.
- **Run Apple's code as shipped.** Don't replace Apple's libraries or rewrite their instructions. Emulate the kernel boundary, not the frameworks above it.
- **Match real hardware.** Values the guest sees (sysctls, home directory, CPU family) are what a real device reports. If a value isn't known for sure, leave it out rather than invent one.
- **macOS and iOS are equal.** Code and docs treat both as first-party.

## Workflow

- Verify against real restore images, and record expected outputs from real runs.
- Read the kernel's own source (Apple's xnu, github.com/apple-oss-distributions/xnu) for which cases a call has before probing it, and then measure each on a real kernel: the source is a release behind and has none of the extensions, so it says where to look and a measurement says what is so. Say in `limits.md` when something is from the source and was not measured.
- Update `limits.md` when a limit is found, fixed or ruled out.
