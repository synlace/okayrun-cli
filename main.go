package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

var (
	APIBaseURL     = "https://okayrun.io"
	WSBaseURL      = "wss://okayrun.io/v1"
	ConfigFileName = ".okay.json"
	globalProxy   string
	globalNoVerify bool
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
	Token         string `json:"token"`
	Email         string `json:"email"`
	Proxy         string `json:"proxy,omitempty"`
	TLSSkipVerify bool   `json:"tls_skip_verify,omitempty"`
}

// ProxyConfig holds the resolved proxy configuration for HTTP clients.
type ProxyConfig struct {
	ProxyURL      *url.URL
	TLSSkipVerify bool
}

var resolvedProxy *ProxyConfig

// resolveProxyConfig merges CLI flags, environment variables, and config file
// settings. Precedence: CLI flag > env var > config file.
func resolveProxyConfig(cfg *Config) *ProxyConfig {
	var proxyURL *url.URL
	var skipVerify bool

	// Start with config file values
	if cfg != nil && cfg.Proxy != "" {
		if u, err := url.Parse(cfg.Proxy); err == nil {
			proxyURL = u
		}
	}
	if cfg != nil && cfg.TLSSkipVerify {
		skipVerify = true
	}

	// Environment variables override config file
	if envProxy := os.Getenv("OKAY_PROXY"); envProxy != "" {
		if u, err := url.Parse(envProxy); err == nil {
			proxyURL = u
		}
	}
	if os.Getenv("OKAY_TLS_SKIP_VERIFY") == "1" || os.Getenv("OKAY_TLS_SKIP_VERIFY") == "true" {
		skipVerify = true
	}

	// CLI flags override everything
	if globalProxy != "" {
		if u, err := url.Parse(globalProxy); err == nil {
			proxyURL = u
		}
	}
	if globalNoVerify {
		skipVerify = true
	}

	return &ProxyConfig{ProxyURL: proxyURL, TLSSkipVerify: skipVerify}
}

func getProxyConfig() *ProxyConfig {
	if resolvedProxy != nil {
		return resolvedProxy
	}
	cfg, _ := loadConfig()
	resolvedProxy = resolveProxyConfig(cfg)
	return resolvedProxy
}

// newHTTPClient returns an *http.Client configured with the resolved proxy
// and TLS settings. Each call returns a new client with its own transport.
func newHTTPClient(timeout time.Duration) *http.Client {
	pc := getProxyConfig()

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: pc.TLSSkipVerify,
		},
	}
	if pc.ProxyURL != nil {
		transport.Proxy = http.ProxyURL(pc.ProxyURL)
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// newWebSocketDialer returns a *websocket.Dialer that uses the same proxy
// and TLS configuration as the HTTP clients.
func newWebSocketDialer() *websocket.Dialer {
	pc := getProxyConfig()

	dialer := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: pc.TLSSkipVerify,
		},
	}
	if pc.ProxyURL != nil {
		dialer.Proxy = http.ProxyURL(pc.ProxyURL)
	}

	return dialer
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

// parseGlobalFlags extracts --proxy and --no-proxy-verify from os.Args and
// returns the cleaned args without those flags. Must be called before any
// command-level argument parsing.
func parseGlobalFlags(args []string) []string {
	var cleaned []string
	for i := 1; i < len(args); i++ {
		a := args[i]
		if a == "--proxy" {
			if i+1 < len(args) {
				globalProxy = args[i+1]
				i++
			}
		} else if strings.HasPrefix(a, "--proxy=") {
			globalProxy = strings.TrimPrefix(a, "--proxy=")
		} else if a == "--no-proxy-verify" {
			globalNoVerify = true
		} else {
			cleaned = append(cleaned, a)
		}
	}
	return append([]string{args[0]}, cleaned...)
}

func main() {
	// Parse global flags before command dispatch
	os.Args = parseGlobalFlags(os.Args)

	if len(os.Args) < 2 {
		printUsage()
		return
	}

	cmd := strings.ToLower(os.Args[1])
	switch cmd {
	case "help":
		printUsage()
	case "compose":
		if len(os.Args) < 3 {
			printComposeUsage()
			return
		}
		handleCompose(os.Args[2:])
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
	case "ps":
		handlePS(parsePSArgs(os.Args[2:]))
	case "run":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing image or --compose argument.")
			fmt.Println("Usage:")
			fmt.Println("  okay run [--verbose] [-e/--env <key=val>] [--name <name>] [-d] [-v/--volume <name:mount>] <image> [command...]")
			fmt.Println("  okay run [--verbose] --compose [compose-file-path]")
			return
		}
		// First detect if --compose is passed anywhere in the args
		isCompose := false
		for _, arg := range os.Args[2:] {
			if arg == "--compose" {
				isCompose = true
				break
			}
		}

		if isCompose {
			verbose, _, composePath := parseComposeArgs(os.Args[2:])
			if composePath == "" {
				if _, err := os.Stat("docker-compose.yaml"); err == nil {
					composePath = "docker-compose.yaml"
				} else if _, err := os.Stat("docker-compose.yml"); err == nil {
					composePath = "docker-compose.yml"
				} else {
					fmt.Println("Error: No docker-compose.yaml or docker-compose.yml file found in current directory.")
					return
				}
			}
			handleComposeRun(composePath, verbose)
		} else {
			verbose, ports, memory, cpus, disk, envVars, name, detach, volumes, image, cmdArgs := parseRunArgs(os.Args[2:])
			if image == "" {
				fmt.Println("Error: Missing image argument.")
				fmt.Println("Usage: okay run [--verbose] [-e/--env <key=val>] [--name <name>] [-d] [-v/--volume <name:mount>] [-p/--publish <port>] [--memory <size>] [--cpus <count>] [--disk <size>] <image> [command...]")
				return
			}
			handleRun(image, cmdArgs, verbose, ports, memory, cpus, disk, envVars, name, detach, volumes)
		}
	case "stop":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing session ID argument.")
			fmt.Println("Usage: okay stop <session-id>")
			fmt.Println("Note: 'okay stop' is deprecated. Use 'okay compose down' for new workflows.")
			return
		}
		fmt.Println("Note: 'okay stop' is deprecated. Use 'okay compose down' for new workflows.")
		handleStop(os.Args[2])
	case "exec":
		if len(os.Args) < 3 {
			fmt.Println("Error: Missing session ID argument.")
			fmt.Println("Usage: okay exec <session-id> [command...]")
			return
		}
		handleExec(os.Args[2], os.Args[3:])
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
	case "volume":
		if len(os.Args) < 3 {
			printVolumeUsage()
			return
		}
		handleVolume(os.Args[2:])
	default:
		fmt.Printf("Unknown command: %s\n", os.Args[1])
		printUsage()
	}
}

// parseMemoryMB parses a memory string like "2g", "512m", "1024" to MB.
func parseMemoryMB(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	var val int
	var unit string
	for i, c := range s {
		if c >= '0' && c <= '9' {
			continue
		}
		for _, ch := range s[:i] {
			if ch >= '0' && ch <= '9' {
				val = val*10 + int(ch-'0')
			}
		}
		unit = s[i:]
		break
	}
	if val == 0 {
		val = 512
	}
	switch unit {
	case "g", "gb":
		return val * 1024
	case "m", "mb", "":
		return val
	default:
		return val
	}
}

// calculateHourlyRate computes the hourly cost in cents based on resource specs.
func calculateHourlyRate(vcpus, memoryMB int, diskSize string) float64 {
	diskGB := parseDiskSizeGB(diskSize)
	rate := float64(vcpus)*0.005 + (float64(memoryMB)/256.0)*0.002 + float64(diskGB)*0.001
	return rate * 100
}

// parseDiskSizeGB parses a disk size string like "4G" or "512M" to GB.
func parseDiskSizeGB(diskSize string) float64 {
	if diskSize == "" {
		return 1.0
	}
	var val float64
	var unit string
	for i, c := range diskSize {
		if c >= '0' && c <= '9' {
			continue
		}
		for _, ch := range diskSize[:i] {
			if ch >= '0' && ch <= '9' {
				val = val*10 + float64(ch-'0')
			}
		}
		unit = diskSize[i:]
		break
	}
	if val == 0 {
		val = 1
	}
	switch unit {
	case "M", "m":
		return val / 1024.0
	default:
		return val
	}
}

func printUsage() {
	fmt.Print(`⚡ OKAY RUN - Ephemeral Firecracker microVM CLI Tool
 
Usage:
  okay [--proxy <url>] [--no-proxy-verify] <command> [arguments]

Global Flags:
  --proxy <url>           Route all requests through an HTTP proxy (e.g., http://127.0.0.1:8080)
  --no-proxy-verify       Skip TLS certificate verification (required for MITM proxies)

  These can also be set via:
    OKAY_PROXY env var           (e.g., export OKAY_PROXY=http://127.0.0.1:8080)
    OKAY_TLS_SKIP_VERIFY=1      (e.g., export OKAY_TLS_SKIP_VERIFY=1)
    Config file (~/.okay.json):  {"proxy": "http://127.0.0.1:8080", "tls_skip_verify": true}

Commands:
  login              Trigger secure web browser authentication loop (recommended)
  auth <token>       Manually save an authentication token (JWT)
  balance            Display your available credit balance
  ps                 List your microVM sessions (use -a to show terminated)
  compose            Docker Compose compatibility layer (up|down|logs)
  run <image>        Provision and enter an interactive console session (alpine|ubuntu|debian|arch|fedora|void...)
  run --verbose      Show raw boot console output instead of suppressing it (useful for diagnostics)
  exec <id> [cmd...] Spawn a concurrent SSH shell or command inside a running microVM
  stop <session-id>  [DEPRECATED] Stop and terminate a running microVM session (use 'compose down' instead)
  save <id> <name>   Save a running microVM session's active disk as a custom image snapshot
  images             List your base and custom virtual machine images
  rmi <name>         Remove a custom image snapshot
  volume             Manage persistent volumes (list|create|mount|unmount|inspect|delete|prune)
  help               Show this manual page

Resource Flags (for 'run'):
  -e, --env <key=val>     Set environment variables (repeatable)
  --name <name>           Assign a name to the session (default: derived from image)
  -d, --detach            Run in background, print session ID and exit
  -v, --volume <name:mt>  Mount a named volume (repeatable, e.g., -v wp_data:/var/lib/html)
  -p, --publish <port>    Publish a port (e.g., 3000:3000)
  --memory, -m <size>     Memory limit (e.g., 512m, 2g). Default: 512m
  --cpus <count>          Number of CPUs (e.g., 1, 2, 4). Default: 1
  --disk <size>           Disk size (e.g., 1g, 10g). Default: 1g

Examples:
  okay run ubuntu                           # Default: 1 CPU, 512MB, 1G
  okay run ubuntu --memory 2g               # 1 CPU, 2GB, 1G
  okay run ubuntu --cpus 2 --memory 4g      # 2 CPUs, 4GB, 1G
  okay run ubuntu --cpus 2 --memory 4g --disk 10g
  okay run -e NODE_ENV=production ubuntu    # With environment variable
  okay run --name my-app ubuntu             # Custom session name
  okay run -d ubuntu                        # Detach, print session ID
  okay run -e PORT=3000 -e DEBUG=1 -d ubuntu
  okay run -v wp_data:/var/lib/html ubuntu  # Mount a named volume

MITM Proxy Examples:
  okay --proxy http://127.0.0.1:8080 --no-proxy-verify run ubuntu
  export OKAY_PROXY=http://127.0.0.1:8080
  export OKAY_TLS_SKIP_VERIFY=1
  okay run ubuntu
`)
}

// --- Auth Manual Command ---

func handleManualAuth(token string) {
	// Verify token is valid with the API
	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := newHTTPClient(5 * time.Second)
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
		client := newHTTPClient(5 * time.Second)
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

	client := newHTTPClient(5 * time.Second)
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
	ID                string            `json:"id"`
	Image             string            `json:"image"`
	Status            string            `json:"status"`
	VMIP              string            `json:"vm_ip"`
	VMIPv6            string            `json:"vm_ipv6"`
	V6Domain          string            `json:"v6_domain"`
	StartedAt         time.Time         `json:"started_at"`
	TotalChargedCents float64           `json:"total_charged_cents"`
	StackID           string            `json:"stack_id,omitempty"`
	ServiceName       string            `json:"service_name,omitempty"`
	Siblings          map[string]string `json:"siblings,omitempty"`
	Entrypoint        []string          `json:"entrypoint,omitempty"`
	Cmd               []string          `json:"cmd,omitempty"`
}

func handlePS(all bool) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := newHTTPClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var sessions []Session
	_ = json.NewDecoder(resp.Body).Decode(&sessions)

	var displayed []Session
	if all {
		displayed = sessions
	} else {
		for _, s := range sessions {
			if s.Status == "RUNNING" || s.Status == "PROVISIONING" {
				displayed = append(displayed, s)
			}
		}
	}

	if len(displayed) == 0 {
		fmt.Println("No active microVM sessions found.")
		return
	}

	fmt.Printf("%-15s %-12s %-10s %-40s %-10s\n", "SESSION ID", "NAME", "STATUS", "DOMAIN", "CHARGED")
	fmt.Println(strings.Repeat("-", 93))
	for _, s := range displayed {
		name := s.ServiceName
		if name == "" {
			name = s.Image
		}
		fmt.Printf("%-15s %-12s %-10s %-40s $%.4f\n", s.ID, name, s.Status, s.V6Domain, s.TotalChargedCents/100.0)
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

	client := newHTTPClient(5 * time.Second)
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

	client := newHTTPClient(30 * time.Second) // Snapshots can take slightly longer
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
	ConnectInteractive(wsURL string, verbose bool, token, sessionID string, entrypoint, cmd []string) error
	ConnectInteractiveSerial(wsURL string, token, sessionID string) error
	ExecuteCommand(wsURL, commandStr string, token, sessionID string) error
}

type RawOSTerminalBridge struct{}

func terminateSession(sessionID, token string) {
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sessionID), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "terminate: build request failed: %v\n", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := newHTTPClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "terminate: request failed: %v\n", err)
		return
	}
	_ = resp.Body.Close()
}

func (r *RawOSTerminalBridge) ConnectInteractive(wsURL string, verbose bool, token, sessionID string, entrypoint, cmd []string) error {
	dialer := newWebSocketDialer()
	ws, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("Error opening terminal socket bridge: websocket handshake failed (HTTP %d %s)", resp.StatusCode, resp.Status)
		}
		return fmt.Errorf("Error opening terminal socket bridge: %v", err)
	}
	defer ws.Close()

	wsConn := &WSConn{conn: ws}

	stdinFd := int(syscall.Stdin)
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("Error configuring terminal to RAW mode: %v", err)
	}
	defer func() {
		term.Restore(stdinFd, oldState)
		fmt.Print("\033[0m\033[?25h")
	}()

	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(""),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	conn, chans, reqs, err := ssh.NewClientConn(wsConn, "localhost:22", sshConfig)
	if err != nil {
		return fmt.Errorf("ssh handshake failed: %v", err)
	}
	defer conn.Close()

	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %v", err)
	}
	defer session.Close()

	w, h, err := term.GetSize(stdinFd)
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "Warning: could not detect terminal size, using default 80x24\n")
		}
		w, h = 80, 24
	}

	modes := ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}

	termEnv := os.Getenv("TERM")
	if termEnv == "" {
		termEnv = "xterm-256color"
	}

	if err := session.RequestPty(termEnv, h, w, modes); err != nil {
		return fmt.Errorf("failed to request pty: %v", err)
	}

	stdinWrapper := &SSHStdinWrapper{
		reader: os.Stdin,
		onHardExit: func() {
			term.Restore(stdinFd, oldState)
			fmt.Println("\nTerminating session...")
			// Discard any incoming bytes (PTY cleanup sequences)
			// so the terminal screen doesn't clear.
			wsConn.SetClosing(true)
			// Close the SSH client to interrupt session.Wait()
			// so ConnectInteractive returns and exec sessions are dropped.
			_ = client.Close()
		},
	}

	session.Stdin = stdinWrapper
	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	sigWin := make(chan os.Signal, 1)
	signal.Notify(sigWin, syscall.SIGWINCH)
	go func() {
		for range sigWin {
			if nw, nh, err := term.GetSize(stdinFd); err == nil {
				_ = session.WindowChange(nh, nw)
			}
		}
	}()
	defer func() {
		signal.Stop(sigWin)
	}()

	// Assemble command line if entrypoint or cmd are specified
	var cmdList []string
	cmdList = append(cmdList, entrypoint...)
	cmdList = append(cmdList, cmd...)

	if len(cmdList) == 0 {
		if err := session.Shell(); err != nil {
			return fmt.Errorf("failed to start shell: %v", err)
		}
	} else {
		var quotedArgs []string
		for _, arg := range cmdList {
			escaped := strings.ReplaceAll(arg, "\\", "\\\\")
			escaped = strings.ReplaceAll(escaped, "\"", "\\\"")
			quotedArgs = append(quotedArgs, `"`+escaped+`"`)
		}
		commandStr := strings.Join(quotedArgs, " ")
		if err := session.Start(commandStr); err != nil {
			return fmt.Errorf("failed to run command: %v", err)
		}
	}

	err = session.Wait()
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			term.Restore(stdinFd, oldState)
			os.Exit(exitErr.ExitStatus())
		}
	}
	return nil
}

func (r *RawOSTerminalBridge) ConnectInteractiveSerial(wsURL string, token, sessionID string) error {
	dialer := newWebSocketDialer()
	ws, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("Error opening terminal socket bridge: websocket handshake failed (HTTP %d %s)", resp.StatusCode, resp.Status)
		}
		return fmt.Errorf("Error opening terminal socket bridge: %v", err)
	}
	defer ws.Close()

	wsConn := &WSConn{conn: ws}

	stdinFd := int(syscall.Stdin)
	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("Error configuring terminal to RAW mode: %v", err)
	}
	defer func() {
		term.Restore(stdinFd, oldState)
		fmt.Print("\033[0m\033[?25h")
	}()

	done := make(chan struct{})
	defer close(done)

	// stdin -> WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				_, _ = wsConn.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// WebSocket -> stdout
	closeChan := make(chan struct{})
	go func() {
		defer close(closeChan)
		buf := make([]byte, 4096)
		for {
			n, err := wsConn.Read(buf)
			if n > 0 {
				_, _ = os.Stdout.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	// Block until connection closes
	<-closeChan

	return nil
}

func (r *RawOSTerminalBridge) ExecuteCommand(wsURL, commandStr string, token, sessionID string) error {
	dialer := newWebSocketDialer()
	ws, resp, err := dialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("Error opening terminal socket bridge: websocket handshake failed (HTTP %d %s)", resp.StatusCode, resp.Status)
		}
		return fmt.Errorf("Error opening terminal socket bridge: %v", err)
	}
	defer ws.Close()

	wsConn := &WSConn{conn: ws}

	sshConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(""),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	done := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		select {
		case <-sigChan:
			fmt.Println("\n⚡ Interrupt detected! Terminating remote session...")
			terminateSession(sessionID, token)
			os.Exit(1)
		case <-done:
			// Normal execution completed
		}
	}()
	defer func() {
		close(done)
		signal.Stop(sigChan)
	}()

	conn, chans, reqs, err := ssh.NewClientConn(wsConn, "localhost:22", sshConfig)
	if err != nil {
		return fmt.Errorf("ssh handshake failed: %v", err)
	}
	defer conn.Close()

	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create ssh session: %v", err)
	}
	defer session.Close()

	session.Stdout = os.Stdout
	session.Stderr = os.Stderr

	if err := session.Run(commandStr); err != nil {
		return fmt.Errorf("failed to run command: %v", err)
	}

	return nil
}

// parseRunArgs splits the os.Args slice passed after "run" into its components.
// A --verbose flag appearing anywhere in args is extracted; the first remaining
// positional argument is the image; any remaining arguments are cmdArgs.
func parseRunArgs(args []string) (verbose bool, ports []string, memory string, cpus int, disk string, envVars []string, name string, detach bool, volumes []string, image string, cmdArgs []string) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--verbose" {
			verbose = true
		} else if a == "-e" || a == "--env" {
			if i+1 < len(args) {
				envVars = append(envVars, args[i+1])
				i++
			}
		} else if strings.HasPrefix(a, "--env=") {
			envVars = append(envVars, strings.TrimPrefix(a, "--env="))
		} else if a == "--name" {
			if i+1 < len(args) {
				name = args[i+1]
				i++
			}
		} else if strings.HasPrefix(a, "--name=") {
			name = strings.TrimPrefix(a, "--name=")
		} else if a == "-d" || a == "--detach" {
			detach = true
		} else if a == "-v" || a == "--volume" {
			if i+1 < len(args) {
				volumes = append(volumes, args[i+1])
				i++
			}
		} else if a == "-p" || a == "--publish" {
			if i+1 < len(args) {
				ports = append(ports, args[i+1])
				i++
			}
		} else if strings.HasPrefix(a, "-p") && len(a) > 2 {
			ports = append(ports, a[2:])
		} else if strings.HasPrefix(a, "--publish=") {
			ports = append(ports, strings.TrimPrefix(a, "--publish="))
		} else if a == "--memory" || a == "-m" {
			if i+1 < len(args) {
				memory = args[i+1]
				i++
			}
		} else if strings.HasPrefix(a, "--memory=") {
			memory = strings.TrimPrefix(a, "--memory=")
		} else if a == "--cpus" {
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &cpus)
				i++
			}
		} else if strings.HasPrefix(a, "--cpus=") {
			fmt.Sscanf(strings.TrimPrefix(a, "--cpus="), "%d", &cpus)
		} else if a == "--disk" {
			if i+1 < len(args) {
				disk = args[i+1]
				i++
			}
		} else if strings.HasPrefix(a, "--disk=") {
			disk = strings.TrimPrefix(a, "--disk=")
		} else {
			positional = append(positional, a)
		}
	}
	if len(positional) > 0 {
		image = positional[0]
		cmdArgs = positional[1:]
	}
	return
}

// parsePSArgs parses the arguments after "ps" to see if the -a or --all flag is passed.
func parsePSArgs(args []string) bool {
	for _, arg := range args {
		if arg == "-a" || arg == "--all" {
			return true
		}
	}
	return false
}

type WSConn struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	reader  io.Reader
	closing int32 // atomic boolean: 0 = false, 1 = true
}

func (c *WSConn) SetClosing(closing bool) {
	var val int32
	if closing {
		val = 1
	}
	atomic.StoreInt32(&c.closing, val)
}

func (c *WSConn) Read(b []byte) (n int, err error) {
	if atomic.LoadInt32(&c.closing) == 1 {
		return 0, io.EOF
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for {
		if atomic.LoadInt32(&c.closing) == 1 {
			return 0, io.EOF
		}
		if c.reader == nil {
			mt, r, err := c.conn.NextReader()
			if err != nil {
				return 0, err
			}
			if mt != websocket.BinaryMessage && mt != websocket.TextMessage {
				continue
			}
			c.reader = r
		}

		n, err = c.reader.Read(b)
		if err == io.EOF {
			c.reader = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

func (c *WSConn) Write(b []byte) (n int, err error) {
	err = c.conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *WSConn) Close() error {
	return c.conn.Close()
}

func (c *WSConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *WSConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *WSConn) SetDeadline(t time.Time) error {
	_ = c.conn.SetReadDeadline(t)
	return c.conn.SetWriteDeadline(t)
}

func (c *WSConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

func (c *WSConn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

type SSHStdinWrapper struct {
	reader     io.Reader
	mu         sync.Mutex
	onHardExit func()
}

func (s *SSHStdinWrapper) Read(p []byte) (n int, err error) {
	n, err = s.reader.Read(p)
	if err != nil {
		return n, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < n; i++ {
		if p[i] == 3 { // Ctrl+C
			if s.onHardExit != nil {
				s.onHardExit()
			}
		}
	}
	return n, nil
}

var termBridge TerminalBridge = &RawOSTerminalBridge{}

// --- Run Command (Terminal Raw WebSocket connection) ---

// deriveServiceName extracts a human-readable service name from a Docker image name.
// e.g. "bkimminich/juice-shop" -> "juice-shop", "nginx" -> "nginx", "ubuntu:24.04" -> "ubuntu"
func deriveServiceName(image string) string {
	name := image
	// Strip tag (e.g. "nginx:latest" -> "nginx")
	if idx := strings.Index(name, ":"); idx != -1 {
		name = name[:idx]
	}
	// Take last segment (e.g. "bkimminich/juice-shop" -> "juice-shop")
	if idx := strings.LastIndex(name, "/"); idx != -1 {
		name = name[idx+1:]
	}
	return name
}

// hasIPv6 checks if any active non-loopback interface has a global unicast IPv6 address.
func hasIPv6() bool {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			if ip.To4() == nil && !ip.IsLinkLocalUnicast() {
				return true
			}
		}
	}
	return false
}

func handleRun(image string, cmdArgs []string, verbose bool, ports []string, memory string, cpus int, disk string, envVars []string, name string, detach bool, volumeRefs []string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	isInteractive := len(cmdArgs) == 0 && !detach

	if isInteractive {
		fmt.Print("\033[0m\033[?25h\n")
		fmt.Printf("[1/3] Checking account balance and credentials...\n")
	}
	// Make sure balance exists
	profileReq, _ := http.NewRequest("GET", APIBaseURL+"/v1/users/me", nil)
	profileReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := newHTTPClient(5 * time.Second)
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

	if isInteractive && !hasIPv6() {
		fmt.Println("[1/3] Warning: No IPv6 detected on this system. okayrun.net domains are IPv6-only and may not be accessible from your connection. SSH via CLI will still work.")
	}

	// Resolve volumes
	var resolvedVolumes []map[string]string
	if len(volumeRefs) > 0 {
		if isInteractive {
			fmt.Printf("[2/3] Resolving volumes...\n")
		}
		volumeMap := make(map[string]string) // name -> id (dedup)
		for _, ref := range volumeRefs {
			parts := strings.SplitN(ref, ":", 2)
			if len(parts) != 2 {
				fmt.Printf("Error: Invalid volume format: %s (expected name:mountpoint)\n", ref)
				return
			}
			source := parts[0]
			mountPoint := parts[1]

			if strings.HasPrefix(source, ".") || strings.HasPrefix(source, "/") {
				fmt.Printf("Error: Bind mount not supported: %s. Use a named volume instead.\n", ref)
				return
			}

			// Deduplicate
			if volID, exists := volumeMap[source]; exists {
				resolvedVolumes = append(resolvedVolumes, map[string]string{
					"volume_id":   volID,
					"mount_point": mountPoint,
				})
				continue
			}

			// Look up volume by name
			checkReq, _ := http.NewRequest("GET", APIBaseURL+"/v1/volumes", nil)
			checkReq.Header.Set("Authorization", "Bearer "+cfg.Token)
			checkResp, err := client.Do(checkReq)
			if err != nil {
				fmt.Printf("Error checking volumes: %v\n", err)
				return
			}
			var volList struct {
				Volumes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"volumes"`
			}
			_ = json.NewDecoder(checkResp.Body).Decode(&volList)
			checkResp.Body.Close()

			found := false
			for _, v := range volList.Volumes {
				if v.Name == source {
					volumeMap[source] = v.ID
					resolvedVolumes = append(resolvedVolumes, map[string]string{
						"volume_id":   v.ID,
						"mount_point": mountPoint,
					})
					if isInteractive {
						fmt.Printf("  ✓ Volume %s -> %s\n", source, mountPoint)
					}
					found = true
					break
				}
			}

			if !found {
				fmt.Printf("Error: Volume '%s' not found. Create it first with: okay volume create %s\n", source, source)
				return
			}
		}
	}

	if isInteractive {
		fmt.Printf("[2/3] Requesting dynamic microVM spawn... (%s rootfs overlay)\n", image)
	}
	payload := map[string]interface{}{
		"image": image,
		"name":  deriveServiceName(image),
	}
	if name != "" {
		payload["name"] = name
	}
	if len(envVars) > 0 {
		payload["environment"] = envVars
	}
	if len(ports) > 0 {
		payload["ports"] = ports
	}
	if memory != "" {
		payload["memory"] = parseMemoryMB(memory)
	}
	if cpus > 0 {
		payload["vcpus"] = cpus
	}
	if disk != "" {
		payload["disk_size"] = disk
	}
	if detach {
		payload["detach"] = true
	}
	if len(resolvedVolumes) > 0 {
		payload["volumes"] = resolvedVolumes
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error encoding request body: %v\n", err)
		return
	}
	req, _ := http.NewRequest("POST", APIBaseURL+"/v1/sessions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	spawnClient := newHTTPClient(120 * time.Second)
	resp, err := spawnClient.Do(req)
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

	if detach {
		fmt.Printf("%s\n", s.ID)
		return
	}

	if isInteractive {
		fmt.Printf("[3/3] Establishing interactive console bridge to virtual machine...\n\n")
		fmt.Printf("Session ID:  %s\n", s.ID)
		fmt.Printf("Domain:      %s\n", s.V6Domain)
		fmt.Printf("Subnet IP:   %s\n", s.VMIPv6)
		fmt.Printf("Billing:     $%.2f/hour + $0.01 boot, billed per second\n", calculateHourlyRate(cpus, parseMemoryMB(memory), disk)/100.0)
		fmt.Printf("Instruction: Standard distro credentials apply. Simply run 'exit/logout' to close and stop the VM.\n\n")
		if verbose {
			fmt.Printf("(verbose boot mode: raw console output enabled)\n\n")
		}
		fmt.Printf("⚡ MicroVM booting...\n\n")

		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		err = termBridge.ConnectInteractive(wsURL, verbose, cfg.Token, s.ID, s.Entrypoint, s.Cmd)
		if err != nil {
			fmt.Println(err)
		}
		// Always terminate the session when the interactive console exits.
		// This kills the VM immediately, ensuring exec sessions are dropped
		// and volumes are torn down cleanly.
		terminateSession(s.ID, cfg.Token)
	} else {
		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		err = termBridge.ExecuteCommand(wsURL, strings.Join(cmdArgs, " "), cfg.Token, s.ID)
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

	client := newHTTPClient(5 * time.Second)
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

	client := newHTTPClient(5 * time.Second)
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

func handleExec(sessionID string, cmdArgs []string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sessionID), nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	client := newHTTPClient(5 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Printf("Error: Session %s not found or expired.\n", sessionID)
		return
	}

	var s Session
	_ = json.NewDecoder(resp.Body).Decode(&s)

	if s.Status != "RUNNING" {
		fmt.Printf("Error: Session %s is not running (status: %s).\n", sessionID, s.Status)
		return
	}

	wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
	
	execCmd := cmdArgs
	if len(execCmd) == 0 {
		execCmd = []string{"/bin/sh"}
	}

	if s.StackID != "" {
		wsURL = fmt.Sprintf("%s/sessions/%s/console?token=%s&console=ssh", WSBaseURL, s.ID, cfg.Token)
	}

	err = termBridge.ConnectInteractive(wsURL, false, cfg.Token, s.ID, nil, execCmd)
	if err != nil {
		fmt.Println(err)
	}
}
