package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCleanBootOutput(t *testing.T) {
	marker := "===OKAYRUN_READY==="

	t.Run("buffers until marker is received", func(t *testing.T) {
		display, ready, buf := CleanBootOutput("", "some boot logs\n", marker)
		if ready {
			t.Errorf("expected not ready, got ready")
		}
		if display != "" {
			t.Errorf("expected empty display, got %q", display)
		}
		if buf != "some boot logs\n" {
			t.Errorf("expected buffer to match, got %q", buf)
		}

		display, ready, buf = CleanBootOutput(buf, "more logs\n", marker)
		if ready {
			t.Errorf("expected not ready, got ready")
		}
		if display != "" {
			t.Errorf("expected empty display, got %q", display)
		}
	})

	t.Run("detects marker and flushes remaining content", func(t *testing.T) {
		buffer := "boot logs\n"
		display, ready, buf := CleanBootOutput(buffer, "===OKAYRUN_READY===\r\nWelcome to Alpine!\n", marker)
		if !ready {
			t.Errorf("expected ready, got not ready")
		}
		if display != "Welcome to Alpine!\n" {
			t.Errorf("expected 'Welcome to Alpine!\\n', got %q", display)
		}
		if buf != "" {
			t.Errorf("expected empty buffer, got %q", buf)
		}
	})
}

func TestShouldExitBootMode_SizeThreshold(t *testing.T) {
	recentStart := time.Now()

	// Just under the limit — should NOT exit.
	if shouldExitBootMode(bootSizeLimit, recentStart) {
		t.Errorf("expected false when bufSize == bootSizeLimit and time is recent")
	}

	// One byte over — should exit.
	if !shouldExitBootMode(bootSizeLimit+1, recentStart) {
		t.Errorf("expected true when bufSize > bootSizeLimit")
	}
}

func TestShouldExitBootMode_TimeTimeout(t *testing.T) {
	// Simulate a boot that started well beyond the timeout.
	expiredStart := time.Now().Add(-(bootTimeout + time.Second))

	if !shouldExitBootMode(0, expiredStart) {
		t.Errorf("expected true when boot start has exceeded bootTimeout")
	}
}

func TestShouldExitBootMode_NotYet(t *testing.T) {
	recentStart := time.Now()
	smallBuf := 1024 // 1 KB

	if shouldExitBootMode(smallBuf, recentStart) {
		t.Errorf("expected false when buf is small and boot just started")
	}
}

func TestParseRunArgs_NoFlag(t *testing.T) {
	verbose, ports, distro, cmdArgs := parseRunArgs([]string{"fedora"})
	if verbose {
		t.Errorf("expected verbose=false, got true")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if distro != "fedora" {
		t.Errorf("expected distro=%q, got %q", "fedora", distro)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseFlagFirst(t *testing.T) {
	verbose, ports, distro, cmdArgs := parseRunArgs([]string{"--verbose", "fedora"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if distro != "fedora" {
		t.Errorf("expected distro=%q, got %q", "fedora", distro)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseFlagLast(t *testing.T) {
	verbose, ports, distro, cmdArgs := parseRunArgs([]string{"fedora", "--verbose"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if distro != "fedora" {
		t.Errorf("expected distro=%q, got %q", "fedora", distro)
	}
	if len(cmdArgs) != 0 {
		t.Errorf("expected empty cmdArgs, got %v", cmdArgs)
	}
}

func TestParseRunArgs_VerboseWithCommand(t *testing.T) {
	verbose, ports, distro, cmdArgs := parseRunArgs([]string{"--verbose", "fedora", "echo hi"})
	if !verbose {
		t.Errorf("expected verbose=true, got false")
	}
	if len(ports) != 0 {
		t.Errorf("expected empty ports, got %v", ports)
	}
	if distro != "fedora" {
		t.Errorf("expected distro=%q, got %q", "fedora", distro)
	}
	if len(cmdArgs) != 1 || cmdArgs[0] != "echo hi" {
		t.Errorf("expected cmdArgs=[\"echo hi\"], got %v", cmdArgs)
	}
}

func TestParseRunArgs_PublishFlags(t *testing.T) {
	tests := []struct {
		args          []string
		expectedPorts []string
		expectedDist  string
	}{
		{[]string{"-p", "3000:3000", "fedora"}, []string{"3000:3000"}, "fedora"},
		{[]string{"-p3000:3000", "fedora"}, []string{"3000:3000"}, "fedora"},
		{[]string{"--publish", "80:80", "fedora"}, []string{"80:80"}, "fedora"},
		{[]string{"--publish=8080:8080", "fedora"}, []string{"8080:8080"}, "fedora"},
		{[]string{"-p", "80:80", "-p", "443:443", "nginx"}, []string{"80:80", "443:443"}, "nginx"},
	}

	for _, tc := range tests {
		_, ports, distro, _ := parseRunArgs(tc.args)
		if distro != tc.expectedDist {
			t.Errorf("for args %v: expected distro %q, got %q", tc.args, tc.expectedDist, distro)
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

func TestEnterKeyTranslation(t *testing.T) {
	buf := []byte("hello\rworld\r\n")
	expected := "hello\nworld\n\n"

	// Mimic translation loop in main.go
	for i := 0; i < len(buf); i++ {
		if buf[i] == '\r' {
			buf[i] = '\n'
		}
	}

	if string(buf) != expected {
		t.Errorf("expected translated string to be %q, got %q", expected, string(buf))
	}
}
