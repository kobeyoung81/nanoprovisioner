# Agent seccomp profile

`nanosec.json` is the seccomp profile the runner *can* pass to agent containers
via the `seccompProfile` config key.

**It is not enabled by default, and production should leave that key unset.**

## Why not

`nanosec.json` is a **denylist**: `"defaultAction": "SCMP_ACT_ALLOW"` with a
handful of syscalls (`chroot`, `mount`, `bpf`, `init_module`, `kexec_load`,
`userfaultfd`, …) switched to `SCMP_ACT_ERRNO`.

Docker's built-in profile — applied automatically to every container unless a
profile overrides it — works the other way round: an allowlist with argument
filtering that denies a few dozen syscalls by default.

The two are not composable. `--security-opt seccomp=<path>` **replaces** the
default profile; it does not add to it. Measured on the platform's own host, the
same probe container behaves like this:

| syscall | Docker default | with this profile |
|---------|----------------|-------------------|
| `keyctl` | blocked (EPERM) | **allowed** (EINVAL) |
| `add_key` | blocked (EPERM) | **allowed** (EFAULT) |
| `request_key` | blocked (EPERM) | **allowed** (EFAULT) |
| `setns` | blocked (EPERM) | **allowed** (EINVAL) |
| `unshare` | blocked (EPERM) | **allowed** (returns 0) |
| `clone3` | blocked (ENOSYS) | **allowed** (EINVAL) |
| `bpf`, `perf_event_open`, `userfaultfd`, `mount`, `swapon`, `kexec_load` | blocked | blocked |
| `process_vm_readv` | allowed | blocked |
| `ptrace` | allowed | allowed |

So enabling this file trades one restriction for six. (`ptrace` is allowed by
both — it is not, as an earlier version of this note claimed, something the
default profile constrains.) The kernel keyring and the namespace/mount
machinery are exactly the surfaces a sandbox is meant to keep shut, which is why
the recommendation below is "leave it unset".

## What to do instead

- **Default: leave `seccompProfile` empty.** Docker's own profile applies, and
  this file is inert.
- **If a custom profile is genuinely required:** start from Docker's official
  default profile for the target engine version, keep its allowlist structure,
  and *append* the extra denials you want. Do not build up from
  `SCMP_ACT_ALLOW`.

## Mechanics worth remembering

- The path is a **daemon-host** absolute path, resolved by the Docker daemon —
  not a path inside the agent container. The file must exist on the host that
  runs dockerd.
- When configured but missing on the host, container creation fails loudly
  rather than silently falling back to no profile.
- The agent user holds NOPASSWD sudo, so a profile that blocks `setuid`/`exec`
  machinery would break tool installation; test changes against a real agent
  container before rolling them out.
