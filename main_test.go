package main

import (
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
