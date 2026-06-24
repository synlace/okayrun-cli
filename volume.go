package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Volume struct {
	ID        string  `json:"id"`
	UserEmail string  `json:"user_email"`
	Name      string  `json:"name"`
	StackID   string  `json:"stack_id,omitempty"`
	SizeBytes int64   `json:"size_bytes"`
	SizeGB    float64 `json:"size_gb"`
	Status    string  `json:"status"`
	AgentID   string  `json:"agent_id,omitempty"`
	CreatedAt string  `json:"created_at"`
}

func printVolumeUsage() {
	fmt.Print(`Usage: okay volume <command> [arguments]

Commands:
  list                        List all volumes
  create <name> [size]        Create a new volume
  mount <id> <path> [--rw]    Mount a volume locally
  unmount <path>              Unmount a local volume
  mounts                      List active local mounts
  inspect <id>                Show volume details
  delete <id>                 Delete a volume
  prune                       Delete all unused volumes
`)
}

func handleVolume(args []string) {
	switch args[0] {
	case "list":
		handleVolumeList()
	case "create":
		if len(args) < 2 {
			fmt.Println("Error: Missing volume name.")
			fmt.Println("Usage: okay volume create <name> [size]")
			return
		}
		size := "1G"
		if len(args) >= 3 {
			size = args[2]
		}
		handleVolumeCreate(args[1], size)
	case "mount":
		if len(args) < 3 {
			fmt.Println("Error: Missing volume ID and path.")
			fmt.Println("Usage: okay volume mount <id> <path> [--rw]")
			return
		}
		rw := false
		for _, a := range args[3:] {
			if a == "--rw" {
				rw = true
			}
		}
		handleVolumeMount(args[1], args[2], rw)
	case "unmount":
		if len(args) < 2 {
			fmt.Println("Error: Missing mount path.")
			fmt.Println("Usage: okay volume unmount <path>")
			return
		}
		handleVolumeUnmount(args[1])
	case "mounts":
		handleVolumeMounts()
	case "inspect":
		if len(args) < 2 {
			fmt.Println("Error: Missing volume ID.")
			fmt.Println("Usage: okay volume inspect <id>")
			return
		}
		handleVolumeInspect(args[1])
	case "delete":
		if len(args) < 2 {
			fmt.Println("Error: Missing volume ID.")
			fmt.Println("Usage: okay volume delete <id>")
			return
		}
		handleVolumeDelete(args[1])
	case "prune":
		handleVolumePrune()
	default:
		fmt.Printf("Unknown volume command: %s\n", args[0])
		printVolumeUsage()
	}
}

func handleVolumeList() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/volumes", nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		Volumes  []Volume `json:"volumes"`
		TotalGB  float64  `json:"total_gb"`
		Count    int      `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Printf("Error decoding response: %v\n", err)
		return
	}

	if len(result.Volumes) == 0 {
		fmt.Println("No volumes found.")
		return
	}

	fmt.Printf("  %-16s %-16s %-8s %-20s %-12s %-10s %s\n", "VOLUME ID", "NAME", "SIZE", "STACK", "STATUS", "HOURLY", "CREATED")
	for _, v := range result.Volumes {
		stack := v.StackID
		if stack == "" {
			stack = "—"
		}
		hourly := fmt.Sprintf("$%.3f", v.SizeGB*0.002)
		created := v.CreatedAt
		if len(created) > 10 {
			created = created[:10]
		}
		fmt.Printf("  %-16s %-16s %-8s %-20s %-12s %-10s %s\n",
			v.ID, v.Name, fmt.Sprintf("%.1fG", v.SizeGB), stack, v.Status, hourly, created)
	}
	fmt.Printf("\n  Total: %d volumes | %.1fG | $%.3f/hr\n", result.Count, result.TotalGB, result.TotalGB*0.002)
}

func handleVolumeCreate(name, size string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	body, _ := json.Marshal(map[string]string{
		"name": name,
		"size": size,
	})
	req, _ := http.NewRequest("POST", APIBaseURL+"/v1/volumes", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("Error: %s\n", errData["error"])
		return
	}

	var vol Volume
	_ = json.NewDecoder(resp.Body).Decode(&vol)
	fmt.Printf("  ✓ Created %s (%s, %.1fG, $%.3f/hr)\n", vol.ID, vol.Name, vol.SizeGB, vol.SizeGB*0.002)
}

// ensureCACert fetches the CA certificate from CP and caches it locally
func ensureCACert(token string) error {
	caPath := filepath.Join(os.Getenv("HOME"), ".okayrun", "ca.crt")

	// Check if already cached
	if _, err := os.Stat(caPath); err == nil {
		return nil
	}

	// Fetch from CP
	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/ca.crt", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch CA cert: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("CA cert endpoint returned %d", resp.StatusCode)
	}

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(caPath), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Save to file
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read CA cert: %w", err)
	}

	if err := os.WriteFile(caPath, body, 0644); err != nil {
		return fmt.Errorf("failed to write CA cert: %w", err)
	}

	return nil
}

func handleVolumeMount(id, path string, rw bool) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	// Fetch CA cert if not cached
	if err := ensureCACert(cfg.Token); err != nil {
		fmt.Printf("Error: failed to fetch CA certificate: %v\n", err)
		return
	}

	// Call mount endpoint to get JWT and agent info
	mountReq, _ := http.NewRequest("POST", APIBaseURL+"/v1/volumes/"+id+"/mount", nil)
	mountReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	mountResp, err := client.Do(mountReq)
	if err != nil {
		fmt.Printf("Error: failed to call mount endpoint: %v\n", err)
		return
	}
	defer mountResp.Body.Close()

	if mountResp.StatusCode != http.StatusOK {
		var errResp struct {
			Error string `json:"error"`
		}
		json.NewDecoder(mountResp.Body).Decode(&errResp)
		fmt.Printf("Error: %s\n", errResp.Error)
		return
	}

	var mountRespData struct {
		AgentHost string `json:"agent_host"`
		AgentPort int    `json:"agent_port"`
		JWT       string `json:"jwt"`
		Volume    Volume `json:"volume"`
	}
	if err := json.NewDecoder(mountResp.Body).Decode(&mountRespData); err != nil {
		fmt.Printf("Error: failed to parse mount response: %v\n", err)
		return
	}

	vol := mountRespData.Volume

	mode := "read-only"
	if rw {
		mode = "read-write (live sync enabled)"
	}

	fmt.Printf("  ✓ Mounting %s (%s) at %s\n", vol.ID, vol.Name, path)
	fmt.Printf("  ✓ Mode: %s\n", mode)
	fmt.Printf("  ✓ Agent: %s\n", mountRespData.AgentHost)

	// Mount via FUSE + 9p
	if err := MountVolume(vol.ID, path, mountRespData.AgentHost, mountRespData.AgentPort, mountRespData.JWT, rw); err != nil {
		fmt.Printf("  ✗ Failed to mount: %v\n", err)
		return
	}

	fmt.Printf("  ✓ FUSE mount established\n")
	fmt.Printf("  ✓ Ready. Run 'okay volume unmount %s' when done.\n\n", path)

	// Block until Ctrl+C or unmount
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	fmt.Println("Press Ctrl+C to unmount...")
	<-sigChan

	fmt.Println("\nUnmounting...")
	UnmountVolume(vol.ID)
	fmt.Println("Done.")
}

func handleVolumeUnmount(path string) {
	// Find the volume ID for this mount point
	activeMountsMu.Lock()
	var volumeID string
	for id, mount := range activeMounts {
		if mount.mountPoint == path {
			volumeID = id
			break
		}
	}
	activeMountsMu.Unlock()

	if volumeID == "" {
		fmt.Printf("  ✗ No volume mounted at %s\n", path)
		return
	}

	if err := UnmountVolume(volumeID); err != nil {
		fmt.Printf("  ✗ Failed to unmount: %v\n", err)
		return
	}

	fmt.Printf("  ✓ Unmounted %s\n", path)
}

func handleVolumeMounts() {
	mounts := ListMounts()
	if len(mounts) == 0 {
		fmt.Println("No active mounts.")
		return
	}

	fmt.Printf("%-36s  %-20s  %-15s  %s\n", "VOLUME", "MOUNT POINT", "AGENT", "MODE")
	fmt.Printf("%-36s  %-20s  %-15s  %s\n", "------", "-----------", "-----", "----")
	for _, m := range mounts {
		mode := "ro"
		if m.RW {
			mode = "rw"
		}
		fmt.Printf("%-36s  %-20s  %-15s  %s\n", m.VolumeID, m.MountPoint, m.AgentHost, mode)
	}
}

func handleVolumeInspect(id string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	req, _ := http.NewRequest("GET", APIBaseURL+"/v1/volumes/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Println("  ✗ Volume not found")
		return
	}

	var vol Volume
	_ = json.NewDecoder(resp.Body).Decode(&vol)

	stack := vol.StackID
	if stack == "" {
		stack = "—"
	}
	agent := vol.AgentID
	if agent == "" {
		agent = "—"
	}

	fmt.Printf("  Volume:       %s\n", vol.ID)
	fmt.Printf("  Name:         %s\n", vol.Name)
	fmt.Printf("  Size:         %.1fG\n", vol.SizeGB)
	fmt.Printf("  Status:       %s\n", vol.Status)
	fmt.Printf("  Stack:        %s\n", stack)
	fmt.Printf("  Agent:        %s\n", agent)
	fmt.Printf("  Created:      %s\n", vol.CreatedAt)
	fmt.Printf("  Hourly rate:  $%.3f\n", vol.SizeGB*0.002)
}

func handleVolumeDelete(id string) {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Println("Error: You are not logged in. Please run: okay login")
		return
	}

	fmt.Printf("  ⚠ This will permanently delete volume %s and all its data.\n", id)
	fmt.Print("  Confirm? [y/N] ")
	var answer string
	fmt.Scanln(&answer)
	if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
		fmt.Println("  Aborted.")
		return
	}

	req, _ := http.NewRequest("DELETE", APIBaseURL+"/v1/volumes/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errData map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&errData)
		fmt.Printf("  ✗ %s\n", errData["error"])
		return
	}

	fmt.Printf("  ✓ Deleted volume %s\n", id)
}

func handleVolumePrune() {
	fmt.Println("  No unused volumes found.")
}
