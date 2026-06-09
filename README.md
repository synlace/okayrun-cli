<h1 align="center">
  <img src="assets/logo.svg" alt="Okay Run" width="48" align="absmiddle" />
  okay run
</h1>

<p align="center">
  <strong>Disposable VMs in less than 1s, billed per second.</strong>
</p>

<p align="center">
  Spawn ephemeral, fully-isolated Firecracker microVMs instantly from your command line.
</p>

<p align="center">
  <a href="https://github.com/synlace/okayrun-cli/releases/latest"><img src="https://img.shields.io/github/v/release/synlace/okayrun-cli?label=Release&color=brightgreen" alt="Latest Release" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/synlace/okayrun-cli?label=License" alt="License: MIT" /></a>
  <a href="https://github.com/synlace/okayrun-cli/actions/workflows/release.yml"><img src="https://img.shields.io/github/actions/workflow/status/synlace/okayrun-cli/release.yml?label=Build&logo=github" alt="Build Status" /></a>
  <a href="https://github.com/synlace/okayrun-cli/stargazers"><img src="https://img.shields.io/github/stars/synlace/okayrun-cli?style=flat&label=Stars&color=FFD700&logo=github" alt="GitHub Stars" /></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a>
  ·
  <a href="#overview">Overview</a>
  ·
  <a href="#features">Features</a>
  ·
  <a href="#commands">Commands</a>
  ·
  <a href="#configuration">Configuration</a>
  ·
  <a href="#architecture">Architecture</a>
  ·
  <a href="#contributing">Contributing</a>
</p>

---

## Quick start

### Install

Install the latest pre-compiled binary via the automatic shell script:

```bash
curl -sSf https://raw.githubusercontent.com/synlace/okayrun-cli/main/scripts/install.sh | sh
```

For custom install directories, you can define `BINDIR` (defaults to `~/.local/bin` for non-root):

```bash
curl -sSf https://raw.githubusercontent.com/synlace/okayrun-cli/main/scripts/install.sh | BINDIR=$HOME/.local/bin sh
```

### Authenticate

Run the secure browser authentication loop to log in:

```bash
okay login
```

This launches your default web browser to authorize the CLI, then establishes a local loopback callback handshake to receive and store your API token in `~/.okay.json`.

Alternatively, if you already have a token, you can save it manually:

```bash
okay auth <your-token>
```

### Spawn a MicroVM

Spawning an interactive console is as fast as:

```bash
okay run ubuntu
```

To run a single headless command and pipe the output back to your terminal:

```bash
okay run alpine "echo 'hello from firecracker!'"
```

---

## Overview

`okayrun-cli` is a lightweight CLI client for [okayrun.io](https://okayrun.io), a platform for spawning ephemeral, fully-isolated Firecracker microVMs on-demand in seconds.

By utilizing AWS Firecracker under the hood, `okayrun-cli` lets you spin up clean, secure sandboxes for debugging, running untrusted scripts, or testing commands. Every machine boots in under 3 seconds and is billed dynamically per second of active usage.

---

## Features

| Feature | Description |
|---|---|
| **Firecracker sandboxing** | Spawns secure, hypervisor-isolated microVMs with dedicated kernels. |
| **Instant booting** | Go from command invocation to an interactive bash shell in less than 3 seconds. |
| **Interactive console** | Full terminal bridge to interactive `alpine`, `ubuntu`, `debian`, or `arch` environments. |
| **Headless command execution**| Run one-off commands and pipe standard output back to your local terminal. |
| **Secure auth loop** | Zero-copy browser-to-terminal OAuth callback loop over a local TCP socket. |
| **Flexible billing** | Billed dynamically per second of active VM usage ($0.01 / hour equivalent). |
| **Local state cache** | Keeps active sessions and authentication details stored cleanly in `~/.okay.json`. |
| **Zero external dependencies**| Compiled as a single static Go binary with minimal resource footprint. |

---

## Usage Example

```text
$ okay run alpine
[1/3] Checking account balance and credentials...
[2/3] Requesting dynamic microVM spawn... (alpine rootfs overlay)
[3/3] Establishing interactive console bridge to virtual machine...

Session ID:  sec_8f9a2b7c
Subnet IP:   172.16.42.105
Billing:     $0.01 / hour, billed dynamically per second
Instruction: Standard distro credentials apply. Simply run 'exit/logout' to close and stop the VM.

⚡ MicroVM booting...

Welcome to Alpine Linux!
firecracker-alpine:~# uname -a
Linux firecracker-alpine 6.1.0-21-amd64 #1 SMP PREEMPT_DYNAMIC Debian 6.1.90-1 x86_64 Linux
firecracker-alpine:~# apk add curl
fetch https://dl-cdn.alpinelinux.org/alpine/v3.20/main/x86_64/APKINDEX.tar.gz
...
firecracker-alpine:~# exit
logout

⚡ Session closed cleanly. Thank you!
```

---

## Commands

| Command | Usage | Description |
|---|---|---|
| **login** | `okay login` | Launches the secure web browser OAuth authentication flow. |
| **auth** | `okay auth <token>` | Saves an authentication token manually in `~/.okay.json`. |
| **balance** | `okay balance` | Displays your active account email and current credits balance. |
| **ps** | `okay ps` | Lists all of your microVM sessions (use `-a` to show terminated). |
| **run** | `okay run <distro> [command...]` | Spawns and logs into a microVM (supports `alpine`, `ubuntu`, `debian`, `arch`). |
| **stop** | `okay stop <session-id>` | Cleanly shuts down and terminates an active microVM session. |
| **help** | `okay help` | Displays the terminal help guide and usage instructions. |

---

## Configuration

All configuration is stored locally inside your home directory:

```text
~/.okay.json
```

You can customize the CLI's behavior using the following environment variables:

| Variable | Default | Description |
|---|---|---|
| `OKAY_API_URL` | `https://okayrun.io` | Overrides the target API and WebSocket controller endpoint. |
| `OKAY_TOKEN` | - | Forces the CLI to use a static token rather than reading `~/.okay.json`. |

---

## Architecture

```text
  Local Terminal
        │
        ▼
   okay-cli (Go)  ◄─── WebSocket Console Connection (wss://okayrun.io/v1) ───►  okayrun.io API
        │                                                                             │
        │ (Interactive TTY raw mode)                                                  │ (Host Control)
        ▼                                                                             ▼
  Interactive VM Terminal ◄───────────────────────────────────────────── AWS Firecracker microVM
                                                                         (Alpine / Ubuntu / Debian / Arch)
```

---

## Contributing

Contributions, issues, and feature requests are welcome! Feel free to check the [issues page](https://github.com/synlace/okayrun-cli/issues).

To build the project locally, ensure you have Go 1.26+ installed:

```bash
go build -o okay main.go
```

To run tests:

```bash
go test ./...
```

---

## License

This project is licensed under the [MIT License](LICENSE).
