package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestParseRunArgs_NoFlag(t *testing.T) {
	verbose, ports, memory, cpus, disk, envVars, name, detach, image, cmdArgs := parseRunArgs([]string{"fedora"})
	if verbose {
		t.Errorf("expected verbose=false, got true")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if memory != "" {
		t.Errorf("expected empty memory, got %v", memory)
	}
	if cpus != 0 {
		t.Errorf("expected cpus=0, got %v", cpus)
	}
	if disk != "" {
		t.Errorf("expected empty disk, got %v", disk)
	}
	if len(envVars) != 0 {
		t.Errorf("expected empty envVars, got %v", envVars)
	}
	if name != "" {
		t.Errorf("expected empty name, got %q", name)
	}
	if detach {
		t.Errorf("expected detach=false, got true")
	}
	if image != "fedora" {
		t.Errorf("expected image=%q, got %q", "fedora", image)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseFlagFirst(t *testing.T) {
	verbose, ports, _, _, _, _, _, _, image, cmdArgs := parseRunArgs([]string{"--verbose", "fedora"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if image != "fedora" {
		t.Errorf("expected image=%q, got %q", "fedora", image)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseFlagLast(t *testing.T) {
	verbose, ports, _, _, _, _, _, _, image, cmdArgs := parseRunArgs([]string{"fedora", "--verbose"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if image != "fedora" {
		t.Errorf("expected image=%q, got %q", "fedora", image)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseWithCommand(t *testing.T) {
	verbose, ports, _, _, _, _, _, _, image, cmdArgs := parseRunArgs([]string{"--verbose", "fedora", "echo hi"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if image != "fedora" {
		t.Errorf("expected image=%q, got %q", "fedora", image)
	}
	if len(cmdArgs) != 1 || cmdArgs[0] != "echo hi" {
		t.Errorf("expected cmdArgs=[\"echo hi\"], got %v", cmdArgs)
	}
}

func TestParseRunArgs_PublishFlags(t *testing.T) {
	tests := []struct {
		args          []string
		expectedPorts []string
		expectedImage string
	}{
		{[]string{"-p", "3000:3000", "fedora"}, []string{"3000:3000"}, "fedora"},
		{[]string{"-p3000:3000", "fedora"}, []string{"3000:3000"}, "fedora"},
		{[]string{"--publish", "80:80", "fedora"}, []string{"80:80"}, "fedora"},
		{[]string{"--publish=8080:8080", "fedora"}, []string{"8080:8080"}, "fedora"},
		{[]string{"-p", "80:80", "-p", "443:443", "nginx"}, []string{"80:80", "443:443"}, "nginx"},
	}

	for _, tc := range tests {
		_, ports, _, _, _, _, _, _, image, _ := parseRunArgs(tc.args)
		if image != tc.expectedImage {
			t.Errorf("for args %v: expected image %q, got %q", tc.args, tc.expectedImage, image)
		}
		if len(ports) != len(tc.expectedPorts) {
			t.Errorf("for args %v: expected ports %v, got %v", tc.args, tc.expectedPorts, ports)
		} else {
			for i, p := range ports {
				if p != tc.expectedPorts[i] {
					t.Errorf("for args %v: expected port[%d]=%q, got %q", tc.args, i, tc.expectedPorts[i], p)
				}
			}
		}
	}
}

func TestParseRunArgs_EnvFlags(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		expectedEnv   []string
		expectedImage string
	}{
		{
			name:          "single -e flag",
			args:          []string{"-e", "NODE_ENV=production", "fedora"},
			expectedEnv:   []string{"NODE_ENV=production"},
			expectedImage: "fedora",
		},
		{
			name:          "single --env flag",
			args:          []string{"--env", "PORT=3000", "fedora"},
			expectedEnv:   []string{"PORT=3000"},
			expectedImage: "fedora",
		},
		{
			name:          "env with equals syntax",
			args:          []string{"--env=DEBUG=1", "fedora"},
			expectedEnv:   []string{"DEBUG=1"},
			expectedImage: "fedora",
		},
		{
			name:          "multiple env flags",
			args:          []string{"-e", "NODE_ENV=prod", "-e", "PORT=8080", "nginx"},
			expectedEnv:   []string{"NODE_ENV=prod", "PORT=8080"},
			expectedImage: "nginx",
		},
		{
			name:          "env with other flags",
			args:          []string{"-e", "A=1", "--verbose", "-p", "80:80", "-e", "B=2", "ubuntu"},
			expectedEnv:   []string{"A=1", "B=2"},
			expectedImage: "ubuntu",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, envVars, _, _, image, _ := parseRunArgs(tc.args)
			if image != tc.expectedImage {
				t.Errorf("expected image %q, got %q", tc.expectedImage, image)
			}
			if len(envVars) != len(tc.expectedEnv) {
				t.Fatalf("expected %d env vars, got %d: %v", len(tc.expectedEnv), len(envVars), envVars)
			}
			for i, e := range envVars {
				if e != tc.expectedEnv[i] {
					t.Errorf("expected env[%d]=%q, got %q", i, tc.expectedEnv[i], e)
				}
			}
		})
	}
}

func TestParseRunArgs_NameFlag(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		expectedName string
	}{
		{
			name:         "name with space",
			args:         []string{"--name", "my-app", "fedora"},
			expectedName: "my-app",
		},
		{
			name:         "name with equals",
			args:         []string{"--name=my-container", "fedora"},
			expectedName: "my-container",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, name, _, _, _ := parseRunArgs(tc.args)
			if name != tc.expectedName {
				t.Errorf("expected name %q, got %q", tc.expectedName, name)
			}
		})
	}
}

func TestParseRunArgs_DetachFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected bool
	}{
		{
			name:     "short -d flag",
			args:     []string{"-d", "fedora"},
			expected: true,
		},
		{
			name:     "long --detach flag",
			args:     []string{"--detach", "fedora"},
			expected: true,
		},
		{
			name:     "no detach flag",
			args:     []string{"fedora"},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, _, detach, _, _ := parseRunArgs(tc.args)
			if detach != tc.expected {
				t.Errorf("expected detach=%v, got %v", tc.expected, detach)
			}
		})
	}
}

func TestParseRunArgs_AllFlagsCombined(t *testing.T) {
	args := []string{
		"-e", "NODE_ENV=prod",
		"--name", "web-server",
		"-d",
		"-p", "80:80",
		"--memory", "1g",
		"--cpus", "2",
		"nginx",
	}
	verbose, ports, memory, cpus, disk, envVars, name, detach, image, cmdArgs := parseRunArgs(args)

	if verbose {
		t.Errorf("expected verbose=false")
	}
	if image != "nginx" {
		t.Errorf("expected image=%q, got %q", "nginx", image)
	}
	if !detach {
		t.Errorf("expected detach=true")
	}
	if name != "web-server" {
		t.Errorf("expected name=%q, got %q", "web-server", name)
	}
	if len(envVars) != 1 || envVars[0] != "NODE_ENV=prod" {
		t.Errorf("expected envVars=[NODE_ENV=prod], got %v", envVars)
	}
	if len(ports) != 1 || ports[0] != "80:80" {
		t.Errorf("expected ports=[80:80], got %v", ports)
	}
	if memory != "1g" {
		t.Errorf("expected memory=%q, got %q", "1g", memory)
	}
	if cpus != 2 {
		t.Errorf("expected cpus=2, got %d", cpus)
	}
	if disk != "" {
		t.Errorf("expected empty disk, got %q", disk)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	os.Setenv("OKAY_TOKEN", "test-env-token")
	defer os.Unsetenv("OKAY_TOKEN")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("unexpected error loading config with env override: %v", err)
	}
	if cfg.Token != "test-env-token" {
		t.Errorf("expected Token = 'test-env-token', got %q", cfg.Token)
	}
}

func TestGetConfigPath_Fallback(t *testing.T) {
	path := getConfigPath()
	if !strings.HasSuffix(path, ".okay.json") {
		t.Errorf("expected config path to end with .okay.json, got %q", path)
	}
}

func TestSaveAndLoadConfig(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "okayrun-cli-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Override HOME env variable so getConfigPath uses our temp directory
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempDir)
	defer os.Setenv("HOME", origHome)

	err = saveConfig("secret-token-123", "test@user.io")
	if err != nil {
		t.Fatalf("saveConfig failed: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if cfg.Token != "secret-token-123" {
		t.Errorf("expected Token = 'secret-token-123', got %q", cfg.Token)
	}
	if cfg.Email != "test@user.io" {
		t.Errorf("expected Email = 'test@user.io', got %q", cfg.Email)
	}
}

func TestParsePSArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected bool
	}{
		{
			name:     "No flags",
			args:     []string{},
			expected: false,
		},
		{
			name:     "Some other positional args",
			args:     []string{"foo", "bar"},
			expected: false,
		},
		{
			name:     "With -a flag",
			args:     []string{"-a"},
			expected: true,
		},
		{
			name:     "With --all flag",
			args:     []string{"--all"},
			expected: true,
		},
		{
			name:     "With mixed arguments and -a flag",
			args:     []string{"foo", "-a", "bar"},
			expected: true,
		},
		{
			name:     "With mixed arguments and --all flag",
			args:     []string{"foo", "bar", "--all"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := parsePSArgs(tt.args)
			if res != tt.expected {
				t.Errorf("expected %v, got %v for args %v", tt.expected, res, tt.args)
			}
		})
	}
}

func TestTerminateSession(t *testing.T) {
	var receivedMethod string
	var receivedURL string
	var receivedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		receivedURL = r.URL.String()
		receivedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	originalAPIBaseURL := APIBaseURL
	APIBaseURL = srv.URL
	defer func() {
		APIBaseURL = originalAPIBaseURL
	}()

	terminateSession("test-session-id", "test-token")

	if receivedMethod != "DELETE" {
		t.Errorf("expected Method DELETE, got %q", receivedMethod)
	}
	if receivedURL != "/v1/sessions/test-session-id" {
		t.Errorf("expected URL /v1/sessions/test-session-id, got %q", receivedURL)
	}
	if receivedAuth != "Bearer test-token" {
		t.Errorf("expected Authorization header 'Bearer test-token', got %q", receivedAuth)
	}
}
