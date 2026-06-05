package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

var (
	APIBaseURL     = "https://okayrun.io"
	WSBaseURL      = "wss://okayrun.io/v1"
	ConfigFileName = ".okay.json"
)

func init() {
	if envAPI := os.Getenv("OKAY_API_URL"); envAPI != "" {
		APIBaseURL = strings.TrimSuffix(envAPI, "/")
		if strings.HasPrefix(APIBaseURL, "https://") {
			WSBaseURL = "wss://" + strings.TrimPrefix(APIBaseURL, "https://") + "/v1"
		} else {
			WSBaseURL = "ws://" + strings.TrimPrefix(APIBaseURL, "http://") + "/v1"
		}
	}
}

type Config struct {
	Token string `json:"token"`
	Email string `json:"email"`
}

func getConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ConfigFileName
	}
	return filepath.Join(home, ConfigFileName)
}

func loadConfig() (*Config, error) {
	// Support environment override first
	if envToken := os.Getenv("OKAY_TOKEN"); envToken != "" {
		return &Config{Token: envToken, Email: "env-override"}, nil
	}

	path := getConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveConfig(token, email string) error {
	cfg := Config{Token: token, Email: email}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), data, 0600)
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	cmd := strings.ToLower(os.Args[1])
	switch cmd {
	case "help":
		printUsage()
	case "auth":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing token argument.")
			fmt.Println("Usage: okay auth <token>")
			return
		}
		handleManualAuth(os.Args[2])
	case "login":
		handleLoginFlow()
	case "balance":
		handleBalance()
	case "list":
		handleList()
	case "run":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing distro argument.")
			fmt.Println("Usage: okay run <alpine|ubuntu|debian|arch> [command...]")
			return
		}
		handleRun(os.Args[2], os.Args[3:])
	case "stop":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing session ID argument.")
			fmt.Println("Usage: okay stop <session-id>")
			return
		}
		handleStop(os.Args[2])
	case "save":
		if len(os.Args) < 4 {
			fmt.Println("Error: Missing session ID or target snapshot name.")
			fmt.Println("Usage: okay save <session-id> <new-image-name>")
			return
		}
		handleSave(os.Args[2], os.Args[3])
	case "images":
		handleImages()
	case "rmi":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing image name.")
			fmt.Println("Usage: okay rmi <image-name>")
			return
		}
		handleRMI(os.Args[2])
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
	}
}

func printUsage() {
	fmt.Print(`⚡ OKAY RUN - Ephemeral Firecracker microVM CLI Tool
 
Usage:
  okay <command> [arguments]

Commands:
  login              Trigger secure web browser authentication loop (recommended)
  auth <token>       Manually save an authentication token (JWT)
  balance            Display your available credit balance
  list               List your currently active microVM sessions
  run <distro>       Provision and enter an interactive console session (alpine|ubuntu|debian|arch)
  stop <session-id>  Stop and terminate a running microVM session cleanly
  save <id> <name>   Save a running microVM session's active disk as a custom image snapshot
  images             List your base and custom virtual machine images
  rmi <name>         Remove a custom image snapshot
  help               Show this manual page
`)
}

// --- Auth Manual Command ---

func handleManualAuth(token string) {
	// Verify token is valid with the API
	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: Unable to connect to backend server: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("Error: The provided authentication token is invalid or expired.")
		return
	}

	var user struct {
		Email        string `json:"email"`
		BalanceCents int    `json:"balance_cents"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&user)

	err = saveConfig(token, user.Email)
	if err != nil {
		fmt.Printf("Error saving config file: %v\n", err)
		return
	}

	fmt.Println("✓ Token applied and saved successfully!")
	fmt.Printf("Logged in as: %s\n", user.Email)
	fmt.Printf("Available Balance: $%.2f\n", float64(user.BalanceCents)/100.0)
}

// --- Web Auth Loop (Loopback Server) ---

func handleLoginFlow() {
	// Start ephemeral server on dynamic/static localhost port
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Println("Error starting local callback listener. Re-trying dynamic port...")
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Printf("Error: Cannot bind local loopback port for login handshake: %v\n", err)
			return
		}
	}
	defer ln.Close()

	// Capture chosen address
	addr := ln.Addr().String()
	_, actualPortStr, _ := net.SplitHostPort(addr)

	loginURL := fmt.Sprintf("%s/?port=%s", APIBaseURL, actualPortStr)
	fmt.Println("Opening your default browser to authenticate...")
	fmt.Printf("Redirecting to: %s\n\n", loginURL)

	// Open browser dynamically
	openBrowser(loginURL)

	fmt.Println("Waiting for browser authentication to complete... [Press Ctrl+C to abort]")

	// Web server block to listen for token callback
	tokenChan := make(chan string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/callback" {
				token := r.URL.Query().Get("token")
				if token != "" {
					tokenChan <- token
					w.Header().Set("Content-Type", "text/html")
					w.Write([]byte(`
						<!DOCTYPE html>
						<html>
						<head>
							<title>Okay Run Authentication Successful</title>
							<style>
								body { background: #09090b; color: #10b981; font-family: sans-serif; text-align: center; padding-top: 15%; }
								.box { border: 1px solid #14532d; background: #022c22; padding: 2.5rem; display: inline-block; border-radius: 12px; box-shadow: 0 10px 15px -3px rgba(0,0,0,0.5); }
								h1 { margin-bottom: 0.5rem; }
								p { color: #a1a1aa; font-size: 0.9rem; }
							</style>
						</head>
						<body>
							<div class="box">
								<h1>✓ Authenticated Successfully</h1>
								<p>You can safely close this browser window and return to your CLI terminal.</p>
							</div>
						</body>
						</html>
					`))
					return
				}
			}
			w.WriteHeader(http.StatusBadRequest)
		}),
	}

	go func() {
		_ = srv.Serve(ln)
	}()

	// Read token
	select {
	case token := <-tokenChan:
		// Shut down local callback listener
		_ = srv.Shutdown(context.Background())

		// Fetch profile to verify email
		req, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			var user struct {
				Email        string `json:"email"`
				BalanceCents int    `json:"balance_cents"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&user); err == nil {
				_ = saveConfig(token, user.Email)
				fmt.Println("✓ Login Handshake Successful!")
				fmt.Printf("Logged in as: %s\n", user.Email)
				fmt.Printf("Balance: $%.2f (Estimated time: %.1f hours)\n", float64(user.BalanceCents)/100.0, float64(user.BalanceCents)/60.0)
				return
			}
		}
		fmt.Println("Error: Received token is invalid.")
	case <-time.After(5 * time.Minute):
		_ = srv.Shutdown(context.Background())
		fmt.Println("Error: Authentication timed out after 5 minutes.")
	}
}

func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	case "windows":
		err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	}
	if err != nil {
		fmt.Printf("Failed to open browser automatically. Please open this link manually:\n%s\n", url)
	}
}

// --- Balance Command ---

func handleBalance() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error connecting to server: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("Error: Your session has expired. Please run: okay login")
		return
	}

	var user struct {
		Email        string `json:"email"`
		BalanceCents int    `json:"balance_cents"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&user)

	fmt.Printf("Account: %s\n", user.Email)
	fmt.Printf("Available Credits: $%.2f\n", float64(user.BalanceCents)/100.0)
	fmt.Printf("Runtime Equivalent: %.1f minutes of active microVM usage.\n", float64(user.BalanceCents))
}

// --- List Command ---

type Session struct {
	ID                string    `json:"id"`
	Distro            string    `json:"distro"`
	Status            string    `json:"status"`
	VMIP              string    `json:"vm_ip"`
	StartedAt         time.Time `json:"started_at"`
	TotalChargedCents float64   `json:"total_charged_cents"`
}

func handleList() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var sessions []Session
	_ = json.NewDecoder(resp.Body).Decode(&sessions)

	if len(sessions) == 0 {
		fmt.Println("No active microVM sessions found.")
		return
	}

	fmt.Printf("%-15s %-12s %-10s %-12s %-10s\n", "SESSION ID", "DISTRO", "STATUS", "IP ADDRESS", "CHARGED")
	fmt.Println(strings.Repeat("-", 65))
	for _, s := range sessions {
		fmt.Printf("%-15s %-12s %-10s %-12s $%.4f\n", s.ID, s.Distro, s.Status, s.VMIP, s.TotalChargedCents/100.0)
	}
}

// --- Stop Command ---

func handleStop(sessionID string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sessionID), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error stopping session: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("Error: %s\n", errData["error"])
		return
	}

	var s Session
	_ = json.NewDecoder(resp.Body).Decode(&s)

	fmt.Printf("✓ Session %s stopped successfully.\n", sessionID)
	fmt.Printf("Total Elapsed cost: $%.4f\n", s.TotalChargedCents/100.0)
}

// --- Save Command ---

func handleSave(sessionID, name string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	fmt.Printf("[1/2] Syncing guest filesystem memory buffer...\n")
	// Make request to API
	url := fmt.Sprintf("%s/v1/sessions/%s/save?name=%s", APIBaseURL, sessionID, name)
	req, _ := http.NewRequest("POST", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	fmt.Printf("[2/2] Creating copy-on-write snapshot disk...\n\n")

	client := &http.Client{Timeout: 30 * time.Second} // Snapshots can take slightly longer
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error communicating with server: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		if errData["error"] != "" {
			fmt.Printf("Error: %s\n", errData["error"])
		} else {
			fmt.Printf("Error: Snapshot save failed with status code %d\n", resp.StatusCode)
		}
		return
	}

	fmt.Printf("✓ Snapshot '%s' saved successfully!\n", name)
	fmt.Println("To run this environment again:")
	fmt.Printf("  okay run %s\n", name)
}

// --- Terminal Bridge Seam & Implementation (Candidate 3) ---

type TerminalBridge interface {
	ConnectInteractive(wsURL string) error
	ExecuteCommand(wsURL, commandStr string) error
}

type RawOSTerminalBridge struct{}

func (r *RawOSTerminalBridge) ConnectInteractive(wsURL string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("Error opening terminal socket bridge: %v", err)
	}
	defer ws.Close()

	stdinFd := int(syscall.Stdin)
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("Error configuring terminal to RAW mode: %v", err)
	}
	defer term.Restore(stdinFd, oldState)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		interruptCount := 0
		for {
			select {
			case <-sigChan:
				interruptCount++
				if interruptCount >= 2 {
					term.Restore(stdinFd, oldState)
					_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "Hard interrupt close"))
					fmt.Println("\nHard exit triggered. Terminating session.")
					os.Exit(0)
				}
				_ = ws.WriteMessage(websocket.BinaryMessage, []byte{3})
			}
		}
	}()

	go func() {
		var bootBuf string
		isBooting := true
		readyMarker := "===OKAYRUN_READY==="
		bootStart := time.Now()

		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				term.Restore(stdinFd, oldState)
				fmt.Print("\r\n\r\n⚡ Session closed cleanly. Thank you!\r\n")
				os.Exit(0)
			}

			if isBooting {
				var display string
				display, isBooting, bootBuf = CleanBootOutput(bootBuf, string(msg), readyMarker)
				isBooting = !isBooting // CleanBootOutput returns true if ready, so we invert it for isBooting
				if !isBooting {
					// We just booted! Stop buffering and write the flushed message.
					_, _ = os.Stdout.Write([]byte(display))
					bootBuf = "" // Clear memory
				} else if shouldExitBootMode(len(bootBuf), bootStart) {
					isBooting = false
					_, _ = os.Stdout.Write([]byte(bootBuf))
					bootBuf = "" // Clear memory
				}
			} else {
				_, _ = os.Stdout.Write(msg)
			}
		}
	}()

	buf := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if n > 0 {
			err = ws.WriteMessage(websocket.BinaryMessage, buf[:n])
			if err != nil {
				break
			}
		}
	}

	term.Restore(stdinFd, oldState)
	fmt.Print("\r\n\r\n⚡ Session closed cleanly. Thank you!\r\n")
	return nil
}

func (r *RawOSTerminalBridge) ExecuteCommand(wsURL, commandStr string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	ws, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("Error opening terminal socket bridge: %v", err)
	}
	defer ws.Close()

	var outputBuf bytes.Buffer
	commandSent := false
	var mu sync.Mutex
	done := make(chan struct{})

	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				close(done)
				return
			}

			mu.Lock()
			outputBuf.Write(msg)

			if commandSent {
				fullOutput := outputBuf.String()
				if strings.Contains(fullOutput, "OKAY_CMD_START") && strings.Contains(fullOutput, "OKAY_CMD_END") {
					startIndex := strings.Index(fullOutput, "OKAY_CMD_START")
					endIndex := strings.Index(fullOutput, "OKAY_CMD_END")
					if startIndex != -1 && endIndex > startIndex {
						content := fullOutput[startIndex+len("OKAY_CMD_START") : endIndex]
						content = strings.TrimPrefix(content, "\r")
						content = strings.TrimPrefix(content, "\n")
						content = strings.TrimPrefix(content, "\r")
						content = strings.TrimPrefix(content, "\n")
						content = strings.TrimSuffix(content, "\n")
						content = strings.TrimSuffix(content, "\r")
						content = strings.TrimSuffix(content, "\n")
						content = strings.TrimSuffix(content, "\r")

						fmt.Println(content)
						os.Exit(0)
					}
				}
			}
			mu.Unlock()
		}
	}()

	_ = ws.WriteMessage(websocket.BinaryMessage, []byte("\n"))

	startTime := time.Now()
	for {
		if time.Since(startTime) > 15000*time.Millisecond {
			break
		}

		mu.Lock()
		ready := isShellReady(outputBuf.String())
		mu.Unlock()

		if ready {
			time.Sleep(100 * time.Millisecond)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	commandSent = true
	outputBuf.Reset()
	mu.Unlock()

	payload := fmt.Sprintf("\necho 'OKAY_CMD_S''TART'; %s; echo 'OKAY_CMD_E''ND'\nexit\n", commandStr)
	_ = ws.WriteMessage(websocket.BinaryMessage, []byte(payload))

	select {
	case <-done:
		mu.Lock()
		content := outputBuf.String()
		mu.Unlock()
		if len(content) > 0 {
			fmt.Print(content)
		}
	case <-time.After(15 * time.Second):
		return fmt.Errorf("Error: Command timed out after 15 seconds.")
	}

	return nil
}

// CleanBootOutput buffers the VM's console output until the ready marker is found.
// It returns:
// - the output that should be displayed (if any)
// - a boolean indicating if the ready marker has been matched and booting is complete
// - the updated buffered string
func CleanBootOutput(buffer string, newData string, marker string) (string, bool, string) {
	updatedBuffer := buffer + newData
	if idx := strings.Index(updatedBuffer, marker); idx != -1 {
		afterMarker := updatedBuffer[idx+len(marker):]
		// Strip any leading newlines/carriage returns
		afterMarker = strings.TrimPrefix(afterMarker, "\r")
		afterMarker = strings.TrimPrefix(afterMarker, "\n")
		afterMarker = strings.TrimPrefix(afterMarker, "\r")
		afterMarker = strings.TrimPrefix(afterMarker, "\n")
		return afterMarker, true, ""
	}
	return "", false, updatedBuffer
}

// shouldExitBootMode returns true when the boot buffering phase should be abandoned
// and raw output should be passed directly to the terminal. This prevents a permanent
// hang when the VM boots successfully but never emits the OKAYRUN_READY marker (e.g.
// because the image uses an unsupported init system or shell that does not source
// the injected profile). Two conditions trigger exit:
//   - bufSize exceeds 512 KB  (original safety net — plenty of kernel output)
//   - bootTimeout has elapsed since bootStart (time-based safety net for quiet VMs)
const bootTimeout = 30 * time.Second
const bootSizeLimit = 512 * 1024

func shouldExitBootMode(bufSize int, bootStart time.Time) bool {
	return bufSize > bootSizeLimit || time.Since(bootStart) > bootTimeout
}

func isShellReady(output string) bool {
	idx := strings.LastIndex(output, "firecracker-")
	if idx == -1 {
		return false
	}
	suffix := output[idx:]
	return strings.Contains(suffix, "#") || strings.Contains(suffix, "$")
}

var termBridge TerminalBridge = &RawOSTerminalBridge{}

// --- Run Command (Terminal Raw WebSocket connection) ---

func handleRun(distro string, cmdArgs []string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	isInteractive := len(cmdArgs) == 0

	if isInteractive {
		fmt.Printf("[1/3] Checking account balance and credentials...\n")
	}
	// Make sure balance exists
	profileReq, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
	profileReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 5 * time.Second}
	pResp, err := client.Do(profileReq)
	if err != nil {
		fmt.Printf("Error connecting to server: %v\n", err)
		return
	}
	defer pResp.Body.Close()

	if pResp.StatusCode != http.StatusOK {
		fmt.Println("Error: Expired session token. Please run: okay login")
		return
	}

	var user struct {
		BalanceCents int `json:"balance_cents"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&user)

	if user.BalanceCents <= 0 {
		fmt.Println("Error: Insufficient balance. Please open the web console and add credits first!")
		fmt.Printf("Dashboard: %s\n", APIBaseURL)
		return
	}

	if isInteractive {
		fmt.Printf("[2/3] Requesting dynamic microVM spawn... (%s rootfs overlay)\n", distro)
	}
	body := []byte(fmt.Sprintf(`{"distro":"%s"}`, distro))
	req, _ := http.NewRequest("POST", APIBaseURL+"/v1/sessions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error running VM: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("Error Spawning VM: %s\n", errData["error"])
		return
	}

	var s Session
	_ = json.NewDecoder(resp.Body).Decode(&s)

	if isInteractive {
		fmt.Printf("[3/3] Establishing interactive console bridge to virtual machine...\n\n")
		fmt.Printf("Session ID:  %s\n", s.ID)
		fmt.Printf("Subnet IP:   %s\n", s.VMIP)
		fmt.Printf("Billing:     $0.01 / hour, billed dynamically per second\n")
		fmt.Printf("Instruction: Standard distro credentials apply. Simply run 'exit/logout' to close and stop the VM.\n\n")
		fmt.Printf("⚡ MicroVM booting...\n\n")

		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		err = termBridge.ConnectInteractive(wsURL)
		if err != nil {
			fmt.Println(err)
		}
	} else {
		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		err = termBridge.ExecuteCommand(wsURL, strings.Join(cmdArgs, " "))
		if err != nil {
			fmt.Println(err)
		}
	}
}

type APIImage struct {
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

func handleImages() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/images", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		if errData["error"] != "" {
			fmt.Printf("Error: %s\n", errData["error"])
		} else {
			fmt.Printf("Error: List images failed with status %d\n", resp.StatusCode)
		}
		return
	}

	var images []APIImage
	_ = json.NewDecoder(resp.Body).Decode(&images)

	fmt.Printf("%-20s %-10s %-12s %-20s\n", "IMAGE NAME", "TYPE", "SIZE", "CREATED AT")
	fmt.Println(strings.Repeat("-", 65))
	for _, img := range images {
		sizeStr := "N/A"
		if img.Type == "custom" {
			sizeStr = formatSize(img.SizeBytes)
		}
		dateStr := "N/A"
		if img.Type == "custom" {
			dateStr = img.CreatedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-20s %-10s %-12s %-20s\n", img.Name, img.Type, sizeStr, dateStr)
	}
}

func handleRMI(imageName string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/images/%s", APIBaseURL, imageName), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		if errData["error"] != "" {
			fmt.Printf("Error: %s\n", errData["error"])
		} else {
			fmt.Printf("Error: Delete image failed with status %d\n", resp.StatusCode)
		}
		return
	}

	fmt.Printf("✓ Image '%s' removed successfully.\n", imageName)
}

func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
