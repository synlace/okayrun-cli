package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)

type EnvList []string

func (e *EnvList) UnmarshalYAML(value *yaml.Node) error {
	var list []string
	if err := value.Decode(&list); err == nil {
		*e = list
		return nil
	}

	var m map[string]string
	if err := value.Decode(&m); err == nil {
		for k, v := range m {
			list = append(list, fmt.Sprintf("%s=%s", k, v))
		}
		*e = list
		return nil
	}

	return fmt.Errorf("failed to unmarshal environment at line %d", value.Line)
}

type DependsOnList []string

func (d *DependsOnList) UnmarshalYAML(value *yaml.Node) error {
	var list []string
	if err := value.Decode(&list); err == nil {
		*d = list
		return nil
	}

	var m map[string]interface{}
	if err := value.Decode(&m); err == nil {
		for k := range m {
			list = append(list, k)
		}
		*d = list
		return nil
	}

	return fmt.Errorf("failed to unmarshal depends_on at line %d", value.Line)
}

type ComposeService struct {
	Image       string        `yaml:"image"`
	Ports       []string      `yaml:"ports"`
	DependsOn   DependsOnList `yaml:"depends_on"`
	Environment EnvList       `yaml:"environment"`
	Volumes     []string      `yaml:"volumes"`
	DiskSize    string        `yaml:"x-okay-disk,omitempty"`
}

type ComposeFile struct {
	Version  string                    `yaml:"version"`
	Services map[string]ComposeService `yaml:"services"`
}

type StackSpawnRequest struct {
	StackID  string                `json:"stack_id,omitempty"`
	Services []StackServicePayload `json:"services"`
}

type StackServicePayload struct {
	Name     string   `json:"name"`
	Image    string   `json:"image"`
	DiskSize string   `json:"disk_size,omitempty"`
	Ports    []string `json:"ports,omitempty"`
}

func ParseComposeFile(path string) (*ComposeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var comp ComposeFile
	if err := yaml.Unmarshal(data, &comp); err != nil {
		return nil, err
	}

	return &comp, nil
}

func TranslateImageToDistro(image string) string {
	if image == "" {
		return "alpine"
	}
	return image
}

func PackDirectoryToTarGz(dirPath string) ([]byte, error) {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	absPath, err := filepath.Abs(dirPath)
	if err != nil {
		return nil, err
	}

	err = filepath.Walk(absPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(absPath, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()

			if _, err := io.Copy(tw, file); err != nil {
				return err
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func parseComposeArgs(args []string) (verbose bool, isCompose bool, composePath string) {
	for i := 0; i < len(args); i++ {
		if args[i] == "--verbose" {
			verbose = true
		} else if args[i] == "--compose" {
			isCompose = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				composePath = args[i+1]
				i++
			}
		}
	}
	return
}

func handleComposeRun(composePath string, verbose bool) {
	_ = verbose
	comp, err := ParseComposeFile(composePath)
	if err != nil {
		fmt.Printf("Error parsing compose file %s: %v\n", composePath, err)
		return
	}

	// 1. Volumes packaging step
	for name, svc := range comp.Services {
		for _, vol := range svc.Volumes {
			parts := strings.Split(vol, ":")
			if len(parts) >= 2 {
				localPath := parts[0]
				if strings.HasPrefix(localPath, ".") || strings.HasPrefix(localPath, "/") {
					fmt.Printf("✓ Discovered local volume asset: %s for service %s. Packaging...\n", localPath, name)
					_, err := PackDirectoryToTarGz(localPath)
					if err != nil {
						fmt.Printf("Warning: Failed to package volume %s: %v\n", localPath, err)
					} else {
						fmt.Printf("✓ Volume asset %s packaged cleanly into in-memory buffer.\n", localPath)
					}
				}
			}
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	fmt.Printf("[1/3] Checking account balance and credentials...\n")
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

	fmt.Printf("[2/3] Translating and spawning multi-VM orchestrator stack...\n")
	var payload StackSpawnRequest
	for name, svc := range comp.Services {
		img := TranslateImageToDistro(svc.Image)
		payload.Services = append(payload.Services, StackServicePayload{
			Name:     name,
			Image:    img,
			DiskSize: svc.DiskSize,
			Ports:    svc.Ports,
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error preparing stack payload: %v\n", err)
		return
	}
	fmt.Printf("DEBUG: JSON Payload: %s\n", string(body))

	req, _ := http.NewRequest("POST", APIBaseURL+"/v1/sessions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error running stack: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("Error Spawning Stack: %s\n", errData["error"])
		return
	}

	var stackResp struct {
		StackID  string    `json:"stack_id"`
		Sessions []Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stackResp); err != nil {
		fmt.Printf("Error decoding response: %v\n", err)
		return
	}

	fmt.Printf("[3/3] Establishing console/log multiplexer for stack: %s\n\n", stackResp.StackID)
	for _, s := range stackResp.Sessions {
		fmt.Printf("  - Service: %-10s | Subnet IP: %-30s | ID: %s\n", s.ServiceName, s.VMIPv6, s.ID)
	}
	fmt.Printf("\nPress Ctrl+C to stop all services and terminate the stack.\n\n")

	stopChan := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n\n⚡ Interrupt detected! Shutting down remote services in stack...")
		close(stopChan)

		var cleanupWg sync.WaitGroup
		for _, s := range stackResp.Sessions {
			cleanupWg.Add(1)
			go func(sess Session) {
				defer cleanupWg.Done()
				req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sess.ID), nil)
				req.Header.Set("Authorization", "Bearer "+cfg.Token)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Printf("Error stopping service %s: %v\n", sess.ServiceName, err)
					return
				}
				resp.Body.Close()
				fmt.Printf("✓ Terminated service %s cleanly.\n", sess.ServiceName)
			}(s)
		}
		cleanupWg.Wait()
		fmt.Println("✓ All services shut down. Goodbye!")
		os.Exit(0)
	}()

	colors := []string{
		"\033[1;36m", // Cyan
		"\033[1;35m", // Magenta
		"\033[1;32m", // Green
		"\033[1;33m", // Yellow
		"\033[1;34m", // Blue
		"\033[1;31m", // Red
	}

	var wg sync.WaitGroup
	for i, s := range stackResp.Sessions {
		wg.Add(1)
		color := colors[i%len(colors)]
		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		go StreamLogs(s.ServiceName, color, wsURL, &wg, stopChan, true)
	}

	wg.Wait()
	fmt.Println("All log streams closed. Exiting.")
}

func StreamLogs(serviceName string, color string, wsURL string, wg *sync.WaitGroup, stopChan chan struct{}, follow bool) {
	defer wg.Done()

	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	var ws *websocket.Conn
	var err error

	// Retry loop for the WebSocket connection to allow asynchronous VM provisioning
	for attempt := 0; attempt < 30; attempt++ {
		select {
		case <-stopChan:
			return
		default:
		}

		ws, _, err = dialer.Dial(wsURL, nil)
		if err == nil {
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	if err != nil {
		fmt.Printf("%s[%s]%s Failed to stream logs: %v\n", color, serviceName, "\033[0m", err)
		return
	}
	defer ws.Close()

	msgChan := make(chan []byte)
	errChan := make(chan error)

	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			msgChan <- msg
		}
	}()

	var lineBuf strings.Builder
	readyMarker := "===OKAYRUN_READY==="
	resetColor := "\033[0m"

	var timeoutChan <-chan time.Time
	if !follow {
		timeoutChan = time.After(1500 * time.Millisecond)
	}

	for {
		select {
		case msg := <-msgChan:
			if !follow {
				timeoutChan = time.After(1500 * time.Millisecond)
			}
			msgStr := string(msg)
			msgStr = strings.ReplaceAll(msgStr, readyMarker, "")

			for _, char := range msgStr {
				if char == '\n' {
					line := lineBuf.String()
					line = strings.TrimRight(line, "\r")
					if len(strings.TrimSpace(line)) > 0 {
						fmt.Printf("%s[%s]%s %s\n", color, serviceName, resetColor, line)
					}
					lineBuf.Reset()
				} else {
					lineBuf.WriteRune(char)
				}
			}
		case <-errChan:
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				line = strings.TrimRight(line, "\r")
				if len(strings.TrimSpace(line)) > 0 {
					fmt.Printf("%s[%s]%s %s\n", color, serviceName, resetColor, line)
				}
			}
			return
		case <-timeoutChan:
			if lineBuf.Len() > 0 {
				line := lineBuf.String()
				line = strings.TrimRight(line, "\r")
				if len(strings.TrimSpace(line)) > 0 {
					fmt.Printf("%s[%s]%s %s\n", color, serviceName, resetColor, line)
				}
			}
			return
		case <-stopChan:
			_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stopping"))
			return
		}
	}
}

func printComposeUsage() {
	fmt.Print(`⚡ OKAY RUN - Docker Compose Compatibility Layer

Usage:
  okay compose [-p <project-name>] [-f <compose-file>] <command> [arguments]

Options:
  -p, --project-name    Specify an alternate project name (defaults to directory name + stable path hash)
  -f, --file            Specify an alternate compose file

Commands:
  up          Translate, spawn, and start services (attached mode by default)
  down        Stop and cleanly terminate all services in the active stack
  logs        View or stream log output from active services
`)
}

func sanitizeStackID(s string) string {
	s = strings.ToLower(s)
	var sb strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			sb.WriteRune(c)
		} else if c == ' ' || c == '.' {
			sb.WriteRune('_')
		}
	}
	res := sb.String()
	if res == "" {
		return "stack"
	}
	return res
}

func getStackID(projectOverride string) string {
	if projectOverride != "" {
		return sanitizeStackID(projectOverride)
	}
	if envProj := os.Getenv("COMPOSE_PROJECT_NAME"); envProj != "" {
		return sanitizeStackID(envProj)
	}

	dir, err := os.Getwd()
	if err != nil {
		dir = "default"
	}
	absPath, err := filepath.Abs(dir)
	if err != nil {
		absPath = dir
	}

	dirName := filepath.Base(absPath)
	if dirName == "" || dirName == "." || dirName == "/" {
		dirName = "stack"
	}

	hash := sha256.Sum256([]byte(absPath))
	hashHex := hex.EncodeToString(hash[:])[:8]

	return sanitizeStackID(dirName + "_" + hashHex)
}

func resolveComposePath(providedPath string) (string, error) {
	if providedPath != "" {
		if _, err := os.Stat(providedPath); err != nil {
			return "", fmt.Errorf("specified compose file not found: %s", providedPath)
		}
		return providedPath, nil
	}
	if _, err := os.Stat("docker-compose.yaml"); err == nil {
		return "docker-compose.yaml", nil
	}
	if _, err := os.Stat("docker-compose.yml"); err == nil {
		return "docker-compose.yml", nil
	}
	return "", fmt.Errorf("no docker-compose.yaml or docker-compose.yml file found in current directory")
}

func fetchActiveSessions(token string) ([]Session, error) {
	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var sessions []Session
	if err := json.NewDecoder(resp.Body).Decode(&sessions); err != nil {
		return nil, err
	}
	return sessions, nil
}

func handleCompose(args []string) {
	var projectName string
	var composePath string
	var subcommand string
	var commandIdx int = -1

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-p" || arg == "--project-name" {
			if i+1 < len(args) {
				projectName = args[i+1]
				i++
			} else {
				fmt.Println("Error: Missing project name after " + arg)
				return
			}
		} else if arg == "-f" || arg == "--file" {
			if i+1 < len(args) {
				composePath = args[i+1]
				i++
			} else {
				fmt.Println("Error: Missing compose file after " + arg)
				return
			}
		} else if !strings.HasPrefix(arg, "-") {
			subcommand = strings.ToLower(arg)
			commandIdx = i
			break
		} else {
			fmt.Printf("Unknown compose flag: %s\n", arg)
			printComposeUsage()
			return
		}
	}

	if subcommand == "" {
		fmt.Println("Error: Missing subcommand.")
		printComposeUsage()
		return
	}

	subArgs := args[commandIdx+1:]

	switch subcommand {
	case "up":
		handleComposeUp(projectName, composePath, subArgs)
	case "down":
		handleComposeDown(projectName, subArgs)
	case "logs":
		handleComposeLogs(projectName, subArgs)
	default:
		fmt.Printf("Unknown compose subcommand: %s\n", subcommand)
		printComposeUsage()
	}
}

func handleComposeUp(projectName string, composePath string, subArgs []string) {
	var detach bool
	var verbose bool
	for _, arg := range subArgs {
		if arg == "-d" || arg == "--detach" {
			detach = true
		} else if arg == "--verbose" {
			verbose = true
		} else {
			fmt.Printf("Unknown up flag: %s\n", arg)
			return
		}
	}
	_ = verbose

	resolvedPath, err := resolveComposePath(composePath)
	if err != nil {
		fmt.Println("Error:", err)
		return
	}

	comp, err := ParseComposeFile(resolvedPath)
	if err != nil {
		fmt.Printf("Error parsing compose file %s: %v\n", resolvedPath, err)
		return
	}

	stackID := getStackID(projectName)

	// Volumes packaging step
	for name, svc := range comp.Services {
		for _, vol := range svc.Volumes {
			parts := strings.Split(vol, ":")
			if len(parts) >= 2 {
				localPath := parts[0]
				if strings.HasPrefix(localPath, ".") || strings.HasPrefix(localPath, "/") {
					fmt.Printf("✓ Discovered local volume asset: %s for service %s. Packaging...\n", localPath, name)
					_, err := PackDirectoryToTarGz(localPath)
					if err != nil {
						fmt.Printf("Warning: Failed to package volume %s: %v\n", localPath, err)
					} else {
						fmt.Printf("✓ Volume asset %s packaged cleanly into in-memory buffer.\n", localPath)
					}
				}
			}
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	fmt.Printf("[1/3] Checking account balance and credentials...\n")
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

	fmt.Printf("[2/3] Translating and spawning multi-VM orchestrator stack...\n")
	var payload StackSpawnRequest
	payload.StackID = stackID
	for name, svc := range comp.Services {
		img := TranslateImageToDistro(svc.Image)
		payload.Services = append(payload.Services, StackServicePayload{
			Name:     name,
			Image:    img,
			DiskSize: svc.DiskSize,
		})
	}

	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error preparing stack payload: %v\n", err)
		return
	}

	req, _ := http.NewRequest("POST", APIBaseURL+"/v1/sessions", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	spawnClient := &http.Client{Timeout: 120 * time.Second}
	resp, err := spawnClient.Do(req)
	if err != nil {
		fmt.Printf("Error running stack: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("Error Spawning Stack: %s\n", errData["error"])
		return
	}

	var stackResp struct {
		StackID  string    `json:"stack_id"`
		Sessions []Session `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stackResp); err != nil {
		fmt.Printf("Error decoding response: %v\n", err)
		return
	}

	if detach {
		fmt.Printf("Spawning services in detached mode for stack: %s\n", stackResp.StackID)
		for _, s := range stackResp.Sessions {
			fmt.Printf("  - Service: %-10s | Subnet IP: %-30s | ID: %s\n", s.ServiceName, s.VMIPv6, s.ID)
		}
		return
	}

	fmt.Printf("[3/3] Establishing console/log multiplexer for stack: %s\n\n", stackResp.StackID)
	for _, s := range stackResp.Sessions {
		fmt.Printf("  - Service: %-10s | Subnet IP: %-30s | ID: %s\n", s.ServiceName, s.VMIPv6, s.ID)
	}
	fmt.Printf("\nPress Ctrl+C to stop all services and terminate the stack.\n\n")

	stopChan := make(chan struct{})
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\n\n⚡ Interrupt detected! Shutting down remote services in stack...")
		close(stopChan)

		var cleanupWg sync.WaitGroup
		for _, s := range stackResp.Sessions {
			cleanupWg.Add(1)
			go func(sess Session) {
				defer cleanupWg.Done()
				req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sess.ID), nil)
				req.Header.Set("Authorization", "Bearer "+cfg.Token)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Printf("Error stopping service %s: %v\n", sess.ServiceName, err)
					return
				}
				resp.Body.Close()
				fmt.Printf("✓ Terminated service %s cleanly.\n", sess.ServiceName)
			}(s)
		}
		cleanupWg.Wait()
		fmt.Println("✓ All services shut down. Goodbye!")
		os.Exit(0)
	}()

	colors := []string{
		"\033[1;36m", // Cyan
		"\033[1;35m", // Magenta
		"\033[1;32m", // Green
		"\033[1;33m", // Yellow
		"\033[1;34m", // Blue
		"\033[1;31m", // Red
	}

	var wg sync.WaitGroup
	for i, s := range stackResp.Sessions {
		wg.Add(1)
		color := colors[i%len(colors)]
		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		go StreamLogs(s.ServiceName, color, wsURL, &wg, stopChan, true)
	}

	wg.Wait()
	fmt.Println("All log streams closed. Exiting.")
}

func handleComposeDown(projectName string, subArgs []string) {
	if len(subArgs) > 0 {
		fmt.Printf("Warning: ignoring extra flags/arguments passed to down: %v\n", subArgs)
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	stackID := getStackID(projectName)
	fmt.Printf("Stopping all services for stack: %s...\n", stackID)

	sessions, err := fetchActiveSessions(cfg.Token)
	if err != nil {
		fmt.Printf("Error retrieving active sessions: %v\n", err)
		return
	}

	var targetSessions []Session
	for _, s := range sessions {
		if s.StackID == stackID && s.Status != "TERMINATED" {
			targetSessions = append(targetSessions, s)
		}
	}

	if len(targetSessions) == 0 {
		fmt.Printf("No active services found for stack: %s\n", stackID)
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	var cleanupWg sync.WaitGroup
	for _, s := range targetSessions {
		cleanupWg.Add(1)
		go func(sess Session) {
			defer cleanupWg.Done()
			req, _ := http.NewRequest("DELETE", fmt.Sprintf("%s/v1/sessions/%s", APIBaseURL, sess.ID), nil)
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("Error stopping service %s: %v\n", sess.ServiceName, err)
				return
			}
			resp.Body.Close()
			fmt.Printf("✓ Terminated service %s cleanly.\n", sess.ServiceName)
		}(s)
	}
	cleanupWg.Wait()
	fmt.Println("✓ All services stopped and removed.")
}

func handleComposeLogs(projectName string, subArgs []string) {
	var follow bool
	for _, arg := range subArgs {
		if arg == "-f" || arg == "--follow" {
			follow = true
		} else {
			fmt.Printf("Unknown logs flag: %s\n", arg)
			return
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	stackID := getStackID(projectName)

	sessions, err := fetchActiveSessions(cfg.Token)
	if err != nil {
		fmt.Printf("Error retrieving active sessions: %v\n", err)
		return
	}

	var targetSessions []Session
	for _, s := range sessions {
		if s.StackID == stackID && s.Status != "TERMINATED" {
			targetSessions = append(targetSessions, s)
		}
	}

	if len(targetSessions) == 0 {
		fmt.Printf("No active services found for stack: %s\n", stackID)
		return
	}

	stopChan := make(chan struct{})
	if follow {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigChan
			fmt.Println("\n\n⚡ Detaching log viewer... (remote services will continue running)")
			close(stopChan)
			os.Exit(0)
		}()
	}

	colors := []string{
		"\033[1;36m", // Cyan
		"\033[1;35m", // Magenta
		"\033[1;32m", // Green
		"\033[1;33m", // Yellow
		"\033[1;34m", // Blue
		"\033[1;31m", // Red
	}

	var wg sync.WaitGroup
	for i, s := range targetSessions {
		wg.Add(1)
		color := colors[i%len(colors)]
		wsURL := fmt.Sprintf("%s/sessions/%s/console?token=%s", WSBaseURL, s.ID, cfg.Token)
		go StreamLogs(s.ServiceName, color, wsURL, &wg, stopChan, follow)
	}

	wg.Wait()
}
