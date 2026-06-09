# Specification: SYN-60 - Restrict exposed TCP/UDP ports on microVMs unless explicitly published

## Goal
To align with standard `docker` and `docker compose` behavior, all spawned microVMs should block incoming public IPv6 traffic by default, except for ports explicitly published by the user using the `-p` / `--publish` flag (for `okay run`) or the `ports` block (for `okay compose`).

## Requirements

### 1. `okay run` Port Publishing
- Support parsing `-p` and `--publish` options in `okay run` (e.g. `okay run -p 3000:3000 bkimminich/juice-shop`).
- Support multiple `-p` / `--publish` declarations.
- Extract published ports and include them in the `ports` field of the `POST /v1/sessions` request payload:
  ```json
  {
    "distro": "bkimminich/juice-shop",
    "ports": ["3000:3000"]
  }
  ```

### 2. `okay compose` Port Publishing
- Send the specified service `ports` under the `ports` field in the stack spawn request:
  ```json
  {
    "stack_id": "...",
    "services": [
      {
        "name": "web",
        "distro": "...",
        "ports": ["3000:3000"]
      }
    ]
  }
  ```

### 3. Control Plane Routing
- Receive `ports` in `POST /v1/sessions` for both individual VMs and Stack service definitions.
- Pass `ports` from context to the Agent during the `provision` mTLS RPC command as a `ports` JSON array.

### 4. Agent Host-Level Filtering (Firewall)
- Parse `ports` during VM provisioning on the Agent.
- For each VM TAP interface, set up `ip6tables` forwarding firewall rules:
  1. Allow established/related traffic.
  2. Allow `icmpv6` traffic (essential for IPv6 ND/RS/RA).
  3. Allow TCP/UDP traffic to explicitly published guest ports.
  4. Block all other incoming traffic to the VM's TAP interface.
- Cleanly delete these rules when tearing down the VM.

---

## Technical Design & Implementation

### A. CLI Changes (`okayrun-cli`)
- Update `parseRunArgs` in `main.go` to extract `-p` and `--publish` flags and return them.
- Send `ports` in the `POST /v1/sessions` payload inside `handleRun` and `handleComposeRun`.

### B. Control Plane Changes (`okayrun`)
- Update request models in `api/handlers.go` and `api/session_manager.go` to accept `ports`.
- Use Go `context` to propagate `ports` list down to `vmEngine.Provision`.
- Forward `ports` in `payload["ports"]` inside `api/vm.go`.

### C. Agent Changes (`okayrun-agent`)
- Update the provision frame struct in `main.go` to parse `ports`.
- In `vm.go`, introduce `setupV6FirewallRules` and `teardownV6FirewallRules` that use `ip6tables` to configure/delete rules.
- Integrate these hooks into `Provision` and `Teardown` inside `vm.go`.
