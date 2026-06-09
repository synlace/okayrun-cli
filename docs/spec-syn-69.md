# Specification: SYN-69 - Support Ctrl+C to kill container

## Goal
To allow users of `okay run` (both interactive and non-interactive) to terminate the remote microVM session cleanly on the backend using Ctrl+C, rather than needing to run `okay stop <session-id>` manually.

## Requirements

1. **Update `TerminalBridge` Interface:**
   - Update the method signatures of `ConnectInteractive` and `ExecuteCommand` to accept `token` and `sessionID` strings.
   - Update `RawOSTerminalBridge` and any callers in `main.go`.

2. **Interactive Run Cleanup (`ConnectInteractive`):**
   - On the second Ctrl+C (hard exit), terminate the remote session cleanly on the backend via a `DELETE /v1/sessions/<session-id>` request before exiting.

3. **Non-interactive Run Cleanup (`ExecuteCommand`):**
   - Set up an interrupt signal handler (`os/signal`) inside `ExecuteCommand`.
   - On receipt of SIGINT/SIGTERM, print a termination message, send a `DELETE /v1/sessions/<session-id>` request to cleanly terminate the remote session, and exit the CLI.

4. **Verify Implementation:**
   - Verify that compilation is successful.
   - Run unit tests to ensure no regressions are introduced.

## Technical Design

### Interface Signatures
```go
type TerminalBridge interface {
	ConnectInteractive(wsURL string, verbose bool, token, sessionID string) error
	ExecuteCommand(wsURL, commandStr string, token, sessionID string) error
}
```

### Session Termination Helper
We will define a helper function to send the HTTP DELETE request to stop the session:
```go
func terminateSession(sessionID, token string) {
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sessionID), nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}
```
