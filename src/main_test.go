package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParseContentRangeTotal(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"bytes 0-10/100", 100},
		{"bytes 50-99/1000", 1000},
		{"bytes 0-0/*", -1},
		{"", -1},
		{"invalid", -1},
	}

	for _, tt := range tests {
		got := parseContentRangeTotal(tt.input)
		if got != tt.expected {
			t.Errorf("parseContentRangeTotal(%q) = %d; want %d", tt.input, got, tt.expected)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{500, "500 B"},
		{1024, "1.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
	}

	for _, tt := range tests {
		got := formatBytes(tt.input)
		if got != tt.expected {
			t.Errorf("formatBytes(%d) = %q; want %q", tt.input, got, tt.expected)
		}
	}
}

func TestCheckRangeSupport(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == "GET" && r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/100")
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("a"))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := server.Client()
	supports, length, filename, err := checkRangeSupport(context.Background(), client, server.URL)
	if err != nil {
		t.Fatalf("checkRangeSupport failed: %v", err)
	}

	if !supports {
		t.Error("Expected range support to be true")
	}
	if length != 100 {
		t.Errorf("Expected length 100, got %d", length)
	}
	if filename == "" {
		t.Error("Expected filename to be parsed")
	}
}

func TestWorker(t *testing.T) {
	content := "abcdefghijklmnopqrstuvwxyz"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var start, end int64
		_, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte(content[start : end+1]))
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	tmpDir, err := os.MkdirTemp("", "downloader_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, "test_out")
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer file.Close()

	err = file.Truncate(int64(len(content)))
	if err != nil {
		t.Fatalf("failed to truncate temp file: %v", err)
	}

	var downloaded int64
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var firstErr error
	setError := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}

	client := server.Client()
	worker(ctx, client, server.URL, 0, 12, file, &downloaded, cancel, setError)
	worker(ctx, client, server.URL, 13, 25, file, &downloaded, cancel, setError)

	if firstErr != nil {
		t.Fatalf("worker failed: %v", firstErr)
	}

	if downloaded != int64(len(content)) {
		t.Errorf("Expected downloaded bytes to be %d, got %d", len(content), downloaded)
	}

	file.Close()
	gotContent, err := os.ReadFile(tmpPath)
	if err != nil {
		t.Fatalf("failed to read downloaded file: %v", err)
	}

	if string(gotContent) != content {
		t.Errorf("Expected file content %q, got %q", content, string(gotContent))
	}
}

func TestSHA256Checksum(t *testing.T) {
	content := "test data for checksum"
	tmpDir, err := os.MkdirTemp("", "checksum_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpPath := filepath.Join(tmpDir, "test_file")
	err = os.WriteFile(tmpPath, []byte(content), 0666)
	if err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	h := sha256.Sum256([]byte(content))
	expectedHash := fmt.Sprintf("%x", h)

	calculatedHash, err := calculateSHA256(tmpPath)
	if err != nil {
		t.Fatalf("calculateSHA256 failed: %v", err)
	}

	if calculatedHash != expectedHash {
		t.Errorf("Expected hash %s, got %s", expectedHash, calculatedHash)
	}
}

func TestServerEndpoints(t *testing.T) {
	// 1. Test handleStatus
	req := httptest.NewRequest("GET", "/api/status", nil)
	rr := httptest.NewRecorder()

	handler := enableCORS(handleStatus)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handleStatus returned status %v, want %v", rr.Code, http.StatusOK)
	}

	if origin := rr.Header().Get("Access-Control-Allow-Origin"); origin != "*" {
		t.Errorf("Access-Control-Allow-Origin is %q, want *", origin)
	}

	var statusMap map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&statusMap); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if statusMap["status"] != "ok" {
		t.Errorf("status response is %q, want 'ok'", statusMap["status"])
	}

	// 2. Test handleJobs
	req = httptest.NewRequest("GET", "/api/jobs", nil)
	rr = httptest.NewRecorder()

	jobsHandler := enableCORS(handleJobs)
	jobsHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("handleJobs returned status %v, want %v", rr.Code, http.StatusOK)
	}

	var jobsList []DownloadJob
	if err := json.NewDecoder(rr.Body).Decode(&jobsList); err != nil {
		t.Fatalf("failed to decode jobs list response: %v", err)
	}

	// 3. Test handleDownload (with missing/invalid body)
	req = httptest.NewRequest("POST", "/api/download", bytes.NewBufferString("invalid json"))
	rr = httptest.NewRecorder()

	downloadHandler := enableCORS(handleDownload)
	downloadHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("handleDownload with invalid JSON returned status %v, want %v", rr.Code, http.StatusBadRequest)
	}
}

