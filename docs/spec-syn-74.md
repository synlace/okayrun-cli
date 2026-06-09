# Spec: SYN-74 - Fixing okayrun-cli Terminal Sizing & Enter Key Reliability

## 1. Introduction
This specification addresses two critical terminal UX issues in the `okayrun-cli` client:
1. **Terminal doesn't size to shell**: The guest VM's serial console (`/dev/ttyS0`) defaults to a standard 80x24 size, which does not match the host terminal dimensions.
2. **Enter key stays on the same line**: Under raw terminal mode, pressing Enter occasionally fails to execute commands and instead stays on the same line (requiring `Ctrl+L` to redraw the prompt).

## 2. Proposed Changes

### A. Terminal Sizing on Startup
Since Firecracker uses a serial console over `/dev/ttyS0`, it does not automatically receive window size updates (`SIGWINCH`) like a standard PTY. The terminal dimensions must be set inside the guest VM using the `stty` utility.

We will implement automatic initial terminal sizing:
1. When the interactive WebSocket connection is established and the VM has booted (either when the ready marker `===OKAYRUN_READY===` is received, or when the boot mode exits), we query the host terminal's width and height.
2. We construct a command ` stty cols <width> rows <height> && clear\r`.
3. We send this command directly to the guest serial console over the WebSocket.
4. Prepending a space ensures the command is ignored by shell history (if `HISTCONTROL=ignorespace` is configured). This cleanly resizes the TTY and clears the screen, leaving the user with a correctly-dimensioned, pristine terminal prompt.

### B. Enter Key Reliability
In raw mode, pressing Enter sends a carriage return (`\r` or ASCII 13). If the guest terminal's `icrnl` (Input Carriage Return to Newline) setting is lost or disabled (which frequently happens during command aborts or interactive program exits), the shell treats `\r` as a standard cursor movement to column 0 rather than a command executor.

To resolve this permanently and robustly:
1. In the terminal stdin reading loop of `okayrun-cli`, we will detect any carriage return (`\r`, ASCII 13) bytes sent by the host.
2. We will translate `\r` to a newline (`\n`, ASCII 10) before transmitting it over the WebSocket.
3. This ensures that the guest kernel/shell always receives `\n` on Enter, executing commands reliably regardless of the state of the guest's `icrnl` flag.

## 3. Verification Plan
- **Unit Tests**: Add tests verifying the input translation and checking that boot exit triggers the terminal sizing sequence.
- **Manual Verification**: Run `okay run alpine` and verify that standard commands like `htop` or `nano` fit the host terminal dimensions on startup, and that Enter works flawlessly.
