package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// DownloadJob represents the state of a single download task
type DownloadJob struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	OutPath    string `json:"out_path"`
	Size       int64  `json:"size"`
	Downloaded int64  `json:"downloaded"`
	Status     string `json:"status"` // "downloading", "completed", "failed", "cancelled"
	Error      string `json:"error,omitempty"`
	Speed      int64  `json:"speed"` // bytes per second
	SHA256     string `json:"sha256,omitempty"`
	cancelFunc context.CancelFunc
}

var (
	jobs   = make(map[string]*DownloadJob)
	jobsMu sync.RWMutex
)

func main() {
	serverFlag := flag.Bool("server", false, "Run in local HTTP API server mode")
	portFlag := flag.Int("port", 8321, "Port for the HTTP API server (default 8321)")

	// CLI-specific flags
	urlFlag := flag.String("url", "", "The URL of the file to download (required in CLI mode)")
	outFlag := flag.String("out", "", "Output file path (optional)")
	concurrencyFlag := flag.Int("c", 4, "Number of concurrent download connections")
	sha256Flag := flag.String("sha256", "", "Expected SHA-256 hash for integrity check (optional)")

	flag.Parse()

	if *serverFlag || *urlFlag == "" {
		if err := runServer(*portFlag); err != nil {
			fmt.Fprintf(os.Stderr, "Server Error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := runCLI(*urlFlag, *outFlag, *concurrencyFlag, *sha256Flag); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}

func runCLI(urlStr, outPath string, concurrency int, expectedSHA string) error {
	if urlStr == "" {
		flag.Usage()
		return errors.New("URL is required (use -url <url>)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Println("Checking server range support...")
	onProgress := func(downloaded, total, speed float64) {
		drawProgressBar(int64(downloaded), int64(total), speed)
	}

	actualSHA, err := downloadFile(ctx, urlStr, outPath, concurrency, expectedSHA, onProgress)
	fmt.Println() // Print newline after progress bar completes

	if err != nil {
		return err
	}

	if expectedSHA != "" {
		expected := strings.ToLower(strings.TrimSpace(expectedSHA))
		if actualSHA == expected {
			fmt.Println("Checksum verification: SUCCESS (Matches expected value)")
		} else {
			return fmt.Errorf("checksum verification: FAILED (Expected: %s, Got: %s)", expected, actualSHA)
		}
	}

	return nil
}

func runServer(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	mux := http.NewServeMux()

	mux.HandleFunc("/api/status", enableCORS(handleStatus))
	mux.HandleFunc("/api/download", enableCORS(handleDownload))
	mux.HandleFunc("/api/jobs", enableCORS(handleJobs))
	mux.HandleFunc("/api/jobs/cancel", enableCORS(handleCancelJob))
	mux.HandleFunc("/api/select-folder", enableCORS(handleSelectFolder))

	srv := &http.Server{Addr: addr, Handler: mux}

	// Start server in background
	go func() {
		fmt.Printf("Go Downloader Server starting on http://%s\n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "Server Error: %v\n", err)
		}
	}()

	// Start tray icon loop (blocks until exit)
	startSystray(func() {
		// Shutdown the HTTP server gracefully
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})

	return nil
}

func enableCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	}
}

func handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func handleSelectFolder(w http.ResponseWriter, r *http.Request) {
	path, err := selectFolderDialog()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"path": path})
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.RLock()
	defer jobsMu.RUnlock()

	list := make([]*DownloadJob, 0, len(jobs))
	for _, job := range jobs {
		list = append(list, job)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(list)
}

type DownloadRequest struct {
	URL         string `json:"url"`
	Out         string `json:"out"`
	Concurrency int    `json:"concurrency"`
	SHA256      string `json:"sha256"`
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req DownloadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		http.Error(w, "URL is required", http.StatusBadRequest)
		return
	}

	if req.Concurrency <= 0 {
		req.Concurrency = 4
	}

	jobID := strconv.FormatInt(time.Now().UnixNano(), 36)
	ctx, cancel := context.WithCancel(context.Background())

	job := &DownloadJob{
		ID:         jobID,
		URL:        req.URL,
		OutPath:    req.Out,
		Status:     "downloading",
		SHA256:     req.SHA256,
		cancelFunc: cancel,
	}

	jobsMu.Lock()
	jobs[jobID] = job
	jobsMu.Unlock()

	// Start downloader in background
	go func() {
		onProgress := func(downloaded, total, speed float64) {
			jobsMu.Lock()
			job.Downloaded = int64(downloaded)
			job.Size = int64(total)
			job.Speed = int64(speed)
			jobsMu.Unlock()
		}

		actualSHA, err := downloadFile(ctx, req.URL, req.Out, req.Concurrency, req.SHA256, onProgress)

		jobsMu.Lock()
		defer jobsMu.Unlock()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				job.Status = "cancelled"
				job.Error = "Download cancelled by user"
			} else {
				job.Status = "failed"
				job.Error = err.Error()
			}
		} else {
			job.Status = "completed"
			job.Speed = 0
			// Validate checksum if provided
			if req.SHA256 != "" {
				expected := strings.ToLower(strings.TrimSpace(req.SHA256))
				if actualSHA == expected {
					job.Status = "completed"
				} else {
					job.Status = "failed"
					job.Error = fmt.Sprintf("checksum mismatch: expected %s, got %s", expected, actualSHA)
				}
			}
		}
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(job)
}

func handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "Missing job id parameter", http.StatusBadRequest)
		return
	}

	jobsMu.Lock()
	job, exists := jobs[id]
	jobsMu.Unlock()

	if !exists {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}

	if job.cancelFunc != nil {
		job.cancelFunc()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "cancelled", "id": id})
}

// downloadFile contains the core concurrent chunk downloading logic
func downloadFile(ctx context.Context, urlStr, outPath string, concurrency int, expectedSHA string, onProgress func(downloaded, total, speed float64)) (string, error) {
	if _, err := url.ParseRequestURI(urlStr); err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	client := &http.Client{
		Transport: transport,
	}

	supportsRange, contentLength, filename, err := checkRangeSupport(ctx, client, urlStr)
	if err != nil {
		return "", fmt.Errorf("failed to check range support: %w", err)
	}

	if outPath != "" {
		if fi, err := os.Stat(outPath); err == nil && fi.IsDir() {
			outPath = filepath.Join(outPath, filename)
		} else if strings.HasSuffix(outPath, string(filepath.Separator)) || strings.HasSuffix(outPath, "/") {
			outPath = filepath.Join(outPath, filename)
		}
	} else {
		outPath = filename
	}

	// Create directories if necessary
	dir := filepath.Dir(outPath)
	if dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return "", fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	if !supportsRange {
		concurrency = 1
	}

	tmpPath := outPath + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer tmpFile.Close()

	if supportsRange && contentLength > 0 {
		if err := tmpFile.Truncate(contentLength); err != nil {
			return "", fmt.Errorf("failed to pre-allocate temp file size: %w", err)
		}
	}

	downloadSuccess := false
	defer func() {
		if !downloadSuccess {
			tmpFile.Close()
			os.Remove(tmpPath)
		}
	}()

	var totalDownloaded int64
	progressCtx, cancelProgress := context.WithCancel(context.Background())
	progressDone := make(chan struct{})

	// Start progress reporter
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()

		startTime := time.Now()
		lastTime := startTime
		var lastDownloaded int64

		for {
			select {
			case <-ticker.C:
				current := atomic.LoadInt64(&totalDownloaded)
				now := time.Now()
				elapsedSec := now.Sub(lastTime).Seconds()
				if elapsedSec <= 0 {
					elapsedSec = 0.2
				}

				speed := float64(current-lastDownloaded) / elapsedSec
				lastDownloaded = current
				lastTime = now

				if onProgress != nil {
					onProgress(float64(current), float64(contentLength), speed)
				}

			case <-progressCtx.Done():
				current := atomic.LoadInt64(&totalDownloaded)
				overallElapsed := time.Since(startTime).Seconds()
				var avgSpeed float64
				if overallElapsed > 0 {
					avgSpeed = float64(current) / overallElapsed
				}
				if onProgress != nil {
					onProgress(float64(current), float64(contentLength), avgSpeed)
				}
				return
			}
		}
	}()

	var firstErr error
	var errMu sync.Mutex
	setError := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
		}
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()

	if supportsRange && contentLength > 0 {
		if int64(concurrency) > contentLength {
			concurrency = int(contentLength)
		}

		chunkSize := contentLength / int64(concurrency)
		var wg sync.WaitGroup

		for i := 0; i < concurrency; i++ {
			start := int64(i) * chunkSize
			end := start + chunkSize - 1
			if i == concurrency-1 {
				end = contentLength - 1
			}

			wg.Add(1)
			go func(s, e int64) {
				defer wg.Done()
				worker(workerCtx, client, urlStr, s, e, tmpFile, &totalDownloaded, cancelWorkers, setError)
			}(start, end)
		}

		wg.Wait()
	} else {
		downloadSequential(workerCtx, client, urlStr, tmpFile, &totalDownloaded, setError)
	}

	cancelProgress()
	<-progressDone

	if firstErr != nil {
		return "", fmt.Errorf("download failed: %w", firstErr)
	}

	tmpFile.Close()
	if err := os.Rename(tmpPath, outPath); err != nil {
		return "", fmt.Errorf("failed to rename temp file to destination: %w", err)
	}
	downloadSuccess = true

	// Checksum calculation
	actualSHA, err := calculateSHA256(outPath)
	if err != nil {
		return "", fmt.Errorf("failed to calculate checksum: %w", err)
	}

	return actualSHA, nil
}

func checkRangeSupport(ctx context.Context, client *http.Client, urlStr string) (supportsRange bool, contentLength int64, filename string, err error) {
	req, err := http.NewRequestWithContext(ctx, "HEAD", urlStr, nil)
	if err == nil {
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				contentLength := resp.ContentLength
				acceptRanges := resp.Header.Get("Accept-Ranges")
				filename = getFilename(resp, urlStr)
				if acceptRanges == "bytes" && contentLength > 0 {
					return true, contentLength, filename, nil
				}
			}
		}
	}

	req, err = http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return false, -1, "", err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return false, -1, "", err
	}
	defer resp.Body.Close()

	filename = getFilename(resp, urlStr)

	if resp.StatusCode == http.StatusPartialContent {
		contentRange := resp.Header.Get("Content-Range")
		total := parseContentRangeTotal(contentRange)
		if total > 0 {
			return true, total, filename, nil
		}
	}

	return false, resp.ContentLength, filename, nil
}

func parseContentRangeTotal(contentRange string) int64 {
	if contentRange == "" {
		return -1
	}
	parts := strings.Split(contentRange, "/")
	if len(parts) < 2 {
		return -1
	}
	totalStr := strings.TrimSpace(parts[1])
	if totalStr == "*" {
		return -1
	}
	total, err := strconv.ParseInt(totalStr, 10, 64)
	if err != nil {
		return -1
	}
	return total
}

func getFilename(resp *http.Response, urlStr string) string {
	if resp != nil {
		cd := resp.Header.Get("Content-Disposition")
		if cd != "" {
			if _, params, err := mime.ParseMediaType(cd); err == nil {
				if filename, ok := params["filename"]; ok && filename != "" {
					return filepath.Base(filename)
				}
			}
		}
	}

	parsed, err := url.Parse(urlStr)
	if err == nil {
		path := parsed.Path
		if path != "" {
			base := filepath.Base(path)
			if base != "." && base != "/" {
				return base
			}
		}
	}

	return "downloaded_file"
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func drawProgressBar(downloaded, total int64, speed float64) {
	if total <= 0 {
		fmt.Printf("\rDownloading... %s (Speed: %s/s)      ", formatBytes(downloaded), formatBytes(int64(speed)))
		return
	}
	pct := float64(downloaded) / float64(total) * 100
	barWidth := 30
	filledWidth := int(float64(barWidth) * (float64(downloaded) / float64(total)))
	if filledWidth > barWidth {
		filledWidth = barWidth
	}
	bar := strings.Repeat("█", filledWidth) + strings.Repeat("░", barWidth-filledWidth)
	fmt.Printf("\r[%s] %.2f%% (%s / %s) %s/s      ", bar, pct, formatBytes(downloaded), formatBytes(total), formatBytes(int64(speed)))
}

func worker(ctx context.Context, client *http.Client, urlStr string, start, end int64, file *os.File, totalDownloaded *int64, cancel func(), setError func(error)) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		setError(err)
		cancel()
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		setError(err)
		cancel()
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		setError(fmt.Errorf("server returned unexpected status code for range %d-%d: %d", start, end, resp.StatusCode))
		cancel()
		return
	}

	buf := make([]byte, 64*1024)
	writeOffset := start
	for {
		select {
		case <-ctx.Done():
			setError(ctx.Err())
			return
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := file.WriteAt(buf[:n], writeOffset)
			if writeErr != nil {
				setError(writeErr)
				cancel()
				return
			}
			writeOffset += int64(n)
			atomic.AddInt64(totalDownloaded, int64(n))
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			setError(readErr)
			cancel()
			return
		}
	}
}

func downloadSequential(ctx context.Context, client *http.Client, urlStr string, file *os.File, totalDownloaded *int64, setError func(error)) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		setError(err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		setError(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		setError(fmt.Errorf("server returned unexpected status: %d", resp.StatusCode))
		return
	}

	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			setError(ctx.Err())
			return
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := file.Write(buf[:n])
			if writeErr != nil {
				setError(writeErr)
				return
			}
			atomic.AddInt64(totalDownloaded, int64(n))
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			setError(readErr)
			return
		}
	}
}

func calculateSHA256(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
