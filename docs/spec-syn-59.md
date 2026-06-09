# Specification: SYN-59 - v4 is for egress only

## Goal
VMs should only expose on their v6 address in the CLI commands. This means updating `okay list`, `okay run`, and `okay compose` print statements to display `VMIPv6` instead of `VMIP`.

## Requirements

### 1. `okay list`
- Instead of printing `s.VMIP`, print `s.VMIPv6`.
- Adjust column padding/width to accommodate longer IPv6 addresses (up to 39 characters, though typically `2a01:4f9:c010:3f02::xx` is ~30 chars).

### 2. `okay run`
- When running `okay run <image>`, print the VM's IPv6 address (`s.VMIPv6`) instead of `s.VMIP`.

### 3. `okay compose`
- Update `compose.go` to display IPv6 address (`s.VMIPv6`) instead of IPv4 for services in all service/subnet IP list outputs.

## Technical Design

### CLI Outputs
We will change occurrences of `s.VMIP` to `s.VMIPv6` in formatting prints within `main.go` and `compose.go` of `okayrun-cli`.
We will also adjust column layout widths to ensure IPv6 addresses are not clipped and look clean in terminal output.
