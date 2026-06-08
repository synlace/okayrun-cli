package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseComposeFile_EnvironmentAndDepends(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "okayrun-compose-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	yamlContent := `
version: "3"
services:
  app:
    image: nginx:latest
    ports:
      - "8080:80"
    depends_on:
      - redis
    environment:
      - APP_ENV=production
      - DEBUG=true
    volumes:
      - ./public:/usr/share/nginx/html

  redis:
    image: redis:alpine
    ports:
      - "6379:6379"
    environment:
      REDIS_PASSWORD: secret
`

	yamlPath := filepath.Join(tempDir, "docker-compose.yaml")
	if err := os.WriteFile(yamlPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write temp compose file: %v", err)
	}

	comp, err := ParseComposeFile(yamlPath)
	if err != nil {
		t.Fatalf("ParseComposeFile failed: %v", err)
	}

	if len(comp.Services) != 2 {
		t.Errorf("expected 2 services, got %d", len(comp.Services))
	}

	appSvc, ok := comp.Services["app"]
	if !ok {
		t.Fatalf("app service not found")
	}

	if appSvc.Image != "nginx:latest" {
		t.Errorf("expected app image nginx:latest, got %q", appSvc.Image)
	}

	if len(appSvc.DependsOn) != 1 || appSvc.DependsOn[0] != "redis" {
		t.Errorf("expected depends_on [redis], got %v", appSvc.DependsOn)
	}

	if len(appSvc.Environment) != 2 {
		t.Errorf("expected 2 env vars for app, got %d", len(appSvc.Environment))
	}

	redisSvc, ok := comp.Services["redis"]
	if !ok {
		t.Fatalf("redis service not found")
	}

	if len(redisSvc.Environment) != 1 || redisSvc.Environment[0] != "REDIS_PASSWORD=secret" {
		t.Errorf("expected environment [REDIS_PASSWORD=secret], got %v", redisSvc.Environment)
	}
}

func TestTranslateImageToDistro(t *testing.T) {
	tests := []struct {
		image    string
		expected string
	}{
		{"alpine:3.20", "alpine"},
		{"ubuntu:latest", "ubuntu"},
		{"debian", "debian"},
		{"fedora:38", "fedora"},
		{"nginx:latest", "alpine"},
		{"redis", "alpine"},
		{"mysql:8", "alpine"},
		{"", "alpine"},
	}

	for _, tc := range tests {
		got := TranslateImageToDistro(tc.image)
		if got != tc.expected {
			t.Errorf("TranslateImageToDistro(%q) = %q; expected %q", tc.image, got, tc.expected)
		}
	}
}

func TestPackDirectoryToTarGz(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "okayrun-pack-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	subDir := filepath.Join(tempDir, "public")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatalf("failed to create sub dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(subDir, "index.html"), []byte("hello html"), 0644); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}

	data, err := PackDirectoryToTarGz(subDir)
	if err != nil {
		t.Fatalf("PackDirectoryToTarGz failed: %v", err)
	}

	if len(data) == 0 {
		t.Errorf("expected non-empty gzip data")
	}
}
