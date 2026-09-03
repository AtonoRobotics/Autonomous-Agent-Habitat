# OS Build — Host, Workstation, and Machine Images

Status: draft 2. Supersedes draft 1 (Ubuntu). Base is NixOS.

> Read `DESIGN.md` first. This spec is a contract: the **Guarantees** are what other services and tests depend on and must hold exactly. Mechanisms described under them are the reference approach; a builder may choose differently if every guarantee and test still holds. Thresholds and defaults are in `habitat.config` and referenced by name here; never hard-code them.


## 1. Why NixOS

The habitat's thesis is that deterministic code should be exactly reproducible and that every runtime environment should be derivable from a declaration. NixOS is the operating system built on that thesis:

- The host is a derivation. Same flake, same bits, every host, every rebuild.
- A module's runtime is a closure. The manifest's dependency list *is* the sandbox contents, content-addressed, nothing else present. Replay against last year's journal runs on last year's libraries.
- Configuration changes are atomic generation switches with rollback. Package installation is not mutable state.
- Agent machines are images built from the same flake, so an agent's computer is as reproducible as the host.

Rejected: Arch (rolling, unpinned; "fast" comes from Hyprland and minimal services, both available here), Debian/Ubuntu (fine as package sources, but imperative configuration and mount-namespace sandboxes are an approximation of what Nix does natively), Alpine (no systemd), Flatcar/CoreOS (container-first; there are no containers here).

## 2. Targets

One flake, three outputs:

| target                | runs on             | what it is                                                                 |
|-----------------------|---------------------|-----------------------------------------------------------------------------|
| `habitat-host`        | bare metal          | headless: the five services, NATS, ZFS, microVM hypervisor. No compositor.  |
| `habitat-workstation` | bare metal          | host modules plus Hyprland and native `habitat-shell` as the desktop. For a human operating a local habitat or connecting to hosts. |
| `habitat-machine`     | microVM             | an agent's computer. Booted per job from the agent's image, discarded after. |

Multi-host is more `habitat-host` entries in the flake with per-host modules; nothing else changes.

## 3. Kernel

- nixpkgs `linuxPackages_latest` pinned by the flake lock, currently 6.12+. Required and verified at image test: Landlock ABI ≥ 5, cgroup v2 only, unprivileged user namespaces, seccomp TSYNC, KVM with vsock (`vhost_vsock`), ZFS module built against the pinned kernel.
- Kernel command line: `lsm=landlock,yama,apparmor` (AppArmor optional; Landlock is what sandboxes depend on).
- Kernel updates are flake updates, applied through `os/Rebuild` under policy, never unattended.

## 4. Filesystem: ZFS

Root on ZFS, `boot.zfs` first-class in NixOS. Datasets:

```
rpool/nix                  /nix                 the store; read-only bind at runtime
rpool/ROOT/<generation>    /                    small; everything real is /nix or a dataset below
rpool/home/<name>          /home/<name>         resident data; quota; snapshots by registryd
rpool/srv/ontd             /srv/ontd            change log and projections; snapshotted with homes
rpool/srv/commons/<ctx>    /srv/commons/<ctx>   shared per bounded context, setgid
rpool/machines/<name>      /machines/<name>     the agent's persistent data dataset, attached to its VMs on request
rpool/var/log              /var/log             journald namespaces
rpool/var/lib/habitat      /var/lib/habitat     NSS projection, NATS, backend caches, VM image cache
rpool/archive              /archive             retired homes and machine datasets, encrypted
```

Module source and manifests are *in the Nix store*, not under `/srv/modules`: a live module is a store path, immutable by construction. `/srv/modules/<ctx>/<name>/<version>` is a symlink into the store for readability.

Snapshot policy: `ontd` store and homes together every `ontd.snapshot_interval`, retained `ontd.snapshot_retain_short`; daily retained `ontd.snapshot_retain_daily`. Point-in-time restore is a generation rollback plus a ZFS rollback set.

## 5. The host (`habitat-host`)

NixOS modules, one per service, each defining its user, systemd unit with hardening, and its store closure:

```
services.habitat.ontd
services.habitat.registryd
services.habitat.wfd
services.habitat.cortexd
services.habitat.shell          (web only on the host)
services.habitat.nats
services.habitat.microvm        hypervisor: firecracker or cloud-hypervisor via microvm.nix, vsock enabled
```

Boot order as before: `zfs` → `nats` → `ontd` → `registryd` → `wfd` → `cortexd` → `shell`. Resident user sessions `After=registryd`.

Identity: `libnss-habitat` as a NixOS module writing `system.nssModules` and `nsswitch` entries; projection at `/var/lib/habitat/nss/`. No `/etc/subuid`; subids via the NSS `subid` interface. Residents' shells are `nologin`. SSH key-only for the operator account. No `sudo` in any habitat path; OS actions run as the invoker's uid and privilege comes from group membership (polkit rules generated from `os-admin` in the NixOS config).

Networking: `systemd-networkd`, configured from NixOS options. Netplan is gone. nftables outbound default drop for all uids except `cortexd` (allowlisted backends), `nix-daemon` (pinned substituters), `chrony`; per-action egress opened by `wfd` inside the action's namespace or VM. Inbound: SSH, shell, NATS cluster, backend API on GPU hosts.

GPU hosts: NVIDIA driver from nixpkgs pinned by the lock; `cortexd` backend unit gets `DeviceAllow`. GPU passthrough to agent VMs is a per-image option, off by default.

## 6. The workstation (`habitat-workstation`)

`habitat-host` modules plus:

- Hyprland via the nixpkgs/Hyprland flake, keyboard-first, Super-key bindings defined in the habitat module.
- `habitat-shell` native as the primary desktop surface: Admin, Access, Inbox as top-level workspaces; the ontology graph with live sensor state as the default view. Terminal and browser as ordinary windows.
- Bindings: jump to an agent, to inbox, to a run, to a gap, attach to an agent, open the policy file.
- Nothing else preinstalled. The workstation is a human's operating console, not a general desktop; a human who wants a general desktop configures one through their own NixOS module, which is the point of NixOS.

A workstation may run a local habitat (the host modules are present) or connect to remote hosts; a toggle in the flake, not two builds.

## 7. Agent machines (`habitat-machine`)

Each agent has an `os/Image`: a NixOS configuration in the flake, seeded from a template at creation and modifiable by the agent through `os/Rebuild` on its own image, under policy. The image is a derivation; its store path is the identity of the machine's software.

A job needing a machine (build, test, `interface/*`, or an agent-authored program) boots a microVM from the image:

- Firecracker or cloud-hypervisor via `microvm.nix`; boot under one second from a prebuilt image.
- Root filesystem read-only from the image; a tmpfs overlay for the job.
- Optional: the agent's data dataset attached read-write via virtiofs, only if the job declares it.
- Network: none by default; per-job egress rules if declared, enforced on the host side of the tap device.
- Control plane: vsock. `wfd` runs the job's executable inside via a tiny agent in the image, streams the journal out, collects the typed result.
- Discarded on completion. Nothing survives except what was written to the dataset and the journal.

Sub-agents run the same way from the parent's image with no dataset attached. Depth and TTL as specified in `registryd`.

Why disposable: a build or test always runs on a clean, known machine, so green means something; a browser session cannot carry state into the next job; replay is boot-the-same-image-run-the-same-inputs; an agent cannot accumulate undeclared state in its computer, only in its dataset.

## 8. Module sandboxes on the host

Steps and sensors are cheap and stay on the host. With Nix, `wfd`'s sandbox derivation simplifies:

- The module's closure is the entire visible filesystem, bind-mounted read-only from the store into a fresh mount namespace. There is no "declared paths" list to get wrong; if it isn't in the closure, it doesn't exist.
- Declared inputs mounted read-only at fixed paths; one writable output directory.
- Landlock as a second wall on top of the namespace; seccomp on realtime clock reads; no network namespace interfaces; seeded randomness; cgroup limits.
- Runs as the owner's subuid for steps and sensors, the invoker's uid for actions.

Anything that needs more than this goes to a machine (§7).

## 9. Image build and install

- `nix build .#habitat-host` / `.#habitat-workstation` produce disk images and installer ISOs via `nixos-generators`. `.#habitat-machine.<agent>` produces the agent's VM image.
- Everything pinned by `flake.lock`; the lock hash is recorded as `os/Image.revision` and shown in the shell per host and per machine.
- Install: ISO with a non-interactive install module reading hostname, management interface, operator SSH key, headcount and budget, disks. First boot runs `habitat-firstboot`: ZFS layout, load `seed-policy.conf`, `habitat-context.yaml`, `os-context.yaml`, seed modules, create the `triage` agent and its image, snapshot, disable itself.
- Updates: a new flake revision is an `os/Rebuild` action on `os/Configuration`; rollback is `os/Rollback` to a prior generation.

## 10. Security baseline

- Landlock, seccomp, namespaces, cgroups per §8; microVM boundary per §7.
- Secure Boot via lanzaboote with a machine-owner key generated at install (signs kernel, initrd, and ZFS module).
- `auditd` rules on `/etc/nixos`, `/nix/var/nix/profiles`, `/srv/ontd`, and `setuid` calls, so the host has a record of `wfd`'s actions independent of `wfd`.
- chrony; `HostState` degrades on clock skew > `host.clock_skew_degrade` because replay depends on injected timestamps.
- No unattended upgrades; `os/Rebuild` under policy.

## 11. What is not present

- No shell for agents anywhere except inside their own machine, where it is `interface/Shell` and an `InterfaceGap`.
- No container runtime on hosts or in machines.
- No display server on the host. Workstations have Hyprland. Machines have a headless Wayland compositor (cage) started only for `interface/*` jobs and captured over vsock for the shell's Display panel.
- No cron. Timers are systemd timers from NixOS options, owned by `wfd` or by residents through `os/Rebuild` on their own configuration.

## 12. Image test obligations

For every built host, workstation, and machine image:

- Landlock ABI ≥ 5; sandbox cannot open a path outside its closure or connect to an undeclared port.
- Unprivileged userns and nested depth 3.
- cgroup v2 only.
- ZFS create, snapshot, rollback, send/receive; quota enforced.
- NSS resolves residents with `ontd` up and down.
- Outbound default drop for resident uids; `cortexd` reaches its backend.
- Machine: boots from image in < 1s, runs a fixture job over vsock, returns a typed result, is discarded with no residual state; dataset attach is read-write only when declared.
- Workstation: Hyprland starts, native shell renders the seed ontology, keybindings reach each panel.
- Generation rollback restores the previous host and every service comes up in order.
- First boot unattended from the install module; seed loads green.

## 13. Decisions

1. **NixOS, flake-pinned, `linuxPackages_latest`.** Decided.
2. **Three targets: host, workstation, machine.** Decided.
3. **Agent machines are disposable microVMs booted per job from a per-agent image.** Decided.
4. **ZFS root and datasets.** Decided.
5. **Secure Boot via lanzaboote with an install-time key.** Decided.
6. **Headless Wayland (cage) for `interface/*` displays.** Decided.
7. **Hypervisor: Firecracker versus cloud-hypervisor.** Proposal: Firecracker default; cloud-hypervisor per image when `gpu = true`. Open.
