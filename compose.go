package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	Name     string `json:"name"`
	Distro   string `json:"distro"`
	DiskSize string `json:"disk_size,omitempty"`
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
		dist := TranslateImageToDistro(svc.Image)
		payload.Services = append(payload.Services, StackServicePayload{
			Name:     name,
			Distro:   dist,
			DiskSize: svc.DiskSize,
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
		fmt.Printf("  - Service: %-10s | Subnet IP: %-12s | ID: %s\n", s.ServiceName, s.VMIP, s.ID)
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
		go StreamLogs(s.ServiceName, color, wsURL, &wg, stopChan)
	}

	wg.Wait()
	fmt.Println("All log streams closed. Exiting.")
}

func StreamLogs(serviceName string, color string, wsURL string, wg *sync.WaitGroup, stopChan chan struct{}) {
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

	for {
		select {
		case msg := <-msgChan:
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
		case <-stopChan:
			_ = ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "stopping"))
			return
		}
	}
}
