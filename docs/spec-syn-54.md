# Specification: SYN-54 - Align 'okay' and 'okay compose' command syntax with Docker / Docker Compose

## Goal
Align the `okay` CLI with standard `docker compose` expectations. Users can spin up multi-service stacks, run them detached/attached, view logs separately, and tear them down. Additionally, the existing image runner `okay run` will accept any standard OCI/Docker-style image reference.

## Requirements

### 1. `okay compose up` (Attached / Attached Log Stream)
- Parse command-line options. Find and read `docker-compose.yaml` or `docker-compose.yml`.
- Generate a unique, stable `stack_id` from a hash of the absolute path of the directory. Support project override via `-p` / `--project-name` or `COMPOSE_PROJECT_NAME` environment variable.
- Submit stack spawn request containing `stack_id` to `POST /v1/sessions`.
- Stream logs for all spawned services of the stack.
- Handle `Ctrl+C` (SIGINT) by cleanly deleting all remote services in the stack, blocking CLI process exit until all `DELETE` requests complete.

### 2. `okay compose up -d` / `--detach` (Detached)
- Same as above, but start services in the background.
- Print spawned Session IDs and IPs, then exit immediately.

### 3. `okay compose down`
- Identify the active stack for the current directory using the stable path-hash project ID (or override).
- Retrieve active sessions and terminate all remote sessions/services belonging to this stack.

### 4. `okay compose logs [-f / --follow]`
- Stream or print log streams for all active services of the stack.
- If `--follow` is specified, stream logs continuously. `Ctrl+C` ONLY detaches the log viewer (does not terminate remote services).
- If `--follow` is not specified, print all currently accumulated logs (detecting pause in stream) and exit cleanly.

### 5. `okay run <image>`
- Accept any standard OCI/Docker-style image reference (e.g., `ubuntu`, `ubuntu:24.04`, `redis:latest`) and pass it as the `distro` parameter to the API.

---

## Technical Design & Implementation

### A. Directory Path Hash
The stable stack ID will be generated as follows:
- Project Name: `-p` argument, `COMPOSE_PROJECT_NAME` env, or sanitized lowercased directory name.
- Path Hash: SHA-256 hash of the absolute path to the directory (8 hex chars).
- Format: `<projectName>_<hash>` (or exactly `<projectName>` if overridden).

### B. Command routing
In `main.go`, we will add support for the `compose` subcommand:
```go
case "compose":
    handleCompose(os.Args[2:])
```

### C. Options Parsing
A custom command-line parser will look for `-p`/`--project-name` and `-f`/`--file` globally (before the subcommand) and then route to the subcommand (`up`, `down`, `logs`) along with its respective options.

### D. Ctrl+C & SIGINT Separation
- For `up` attached: SIGINT triggers parallel DELETE requests to `/v1/sessions/<id>` and blocks until all finish.
- For `logs --follow`: SIGINT cleanly exits the process without triggering DELETE requests.
