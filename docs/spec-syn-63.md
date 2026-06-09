# Specification: SYN-63 - Support okay ps, replaces okay list

## Goal
To align the Okay Run command line interface with standard Docker patterns by replacing the `okay list` command with a new `okay ps` command.

## Requirements

1. **Rename Command:**
   - Replace the `list` command inside `main.go` with `ps`.
   - Update `printUsage()` in `main.go` to list `ps` instead of `list`.
   - Update any documentation or comments referencing `okay list`.

2. **Default Filtering & Flags:**
   - By default, `okay ps` should filter out terminated sessions and only display sessions that are currently `RUNNING` or `PROVISIONING`.
   - Support a `-a` or `--all` flag (e.g. `okay ps -a` or `okay ps --all`) to disable filtering and display all sessions (including `TERMINATED` ones).

3. **Remove `list` command:**
   - Completely remove support for the `list` command from the `switch` statement in `main.go` so it is no longer accepted.

## Technical Design

### Arguments Parsing
Within `main()`, under `case "ps"`, we will parse arguments to look for `-a` or `--all`.

### Filtration Logic
Inside `handlePS(all bool)`:
- Retrieve all sessions from the API as before.
- If `all` is false, filter the session slice to keep only those with `Status == "RUNNING"` or `Status == "PROVISIONING"`.
- Print the table utilizing the exact layout/columns from the previous list command.
