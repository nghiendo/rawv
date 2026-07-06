package main

import (
	"context"
	"crypto/sha256"
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	urlFlag := flag.String("url", "", "The URL of the file to download (required)")
	outFlag := flag.String("out", "", "Output file path (optional)")
	concurrencyFlag := flag.Int("c", 4, "Number of concurrent download connections")
	sha256Flag := flag.String("sha256", "", "Expected SHA-256 hash for integrity check (optional)")

	flag.Parse()

	if *urlFlag == "" {
		flag.Usage()
		return errors.New("URL is required (use -url <url>)")
	}

	urlStr := *urlFlag
	if _, err := url.ParseRequestURI(urlStr); err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	concurrency := *concurrencyFlag
	if concurrency <= 0 {
		return errors.New("concurrency must be greater than 0")
	}

	// Handle system signals for clean cancellation
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Clone default transport to customize timeout behaviors
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	client := &http.Client{
		Transport: transport,
	}

	fmt.Println("Checking server range support...")
	supportsRange, contentLength, filename, err := checkRangeSupport(ctx, client, urlStr)
	if err != nil {
		return fmt.Errorf("failed to check range support: %w", err)
	}

	outPath := *outFlag
	if outPath == "" {
		outPath = filename
	}

	if contentLength > 0 {
		fmt.Printf("File size: %s\n", formatBytes(contentLength))
	} else {
		fmt.Println("File size: Unknown")
	}

	if supportsRange {
		fmt.Printf("Range requests: Supported. Concurrency: %d\n", concurrency)
	} else {
		fmt.Println("Range requests: Not supported. Falling back to single-threaded download.")
		concurrency = 1
	}

	// Create directories if necessary
	dir := filepath.Dir(outPath)
	if dir != "." && dir != "/" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %w", err)
		}
	}

	tmpPath := outPath + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer tmpFile.Close()

	if supportsRange && contentLength > 0 {
		if err := tmpFile.Truncate(contentLength); err != nil {
			return fmt.Errorf("failed to pre-allocate temp file size: %w", err)
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

				drawProgressBar(current, contentLength, speed)

			case <-progressCtx.Done():
				current := atomic.LoadInt64(&totalDownloaded)
				overallElapsed := time.Since(startTime).Seconds()
				var avgSpeed float64
				if overallElapsed > 0 {
					avgSpeed = float64(current) / overallElapsed
				}
				drawProgressBar(current, contentLength, avgSpeed)
				fmt.Println()
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
		// Single thread sequential fallback
		downloadSequential(workerCtx, client, urlStr, tmpFile, &totalDownloaded, setError)
	}

	cancelProgress()
	<-progressDone

	if firstErr != nil {
		return fmt.Errorf("download failed: %w", firstErr)
	}

	// Success: close and rename temp file to final output
	tmpFile.Close()
	if err := os.Rename(tmpPath, outPath); err != nil {
		return fmt.Errorf("failed to rename temp file to destination: %w", err)
	}
	downloadSuccess = true

	fmt.Printf("Download complete. File saved to: %s\n", outPath)

	// Checksum calculation
	fmt.Println("Calculating SHA-256 checksum...")
	checksum, err := calculateSHA256(outPath)
	if err != nil {
		return fmt.Errorf("failed to calculate checksum: %w", err)
	}
	fmt.Printf("Calculated SHA-256: %s\n", checksum)

	if *sha256Flag != "" {
		expected := strings.ToLower(strings.TrimSpace(*sha256Flag))
		if checksum == expected {
			fmt.Println("Checksum verification: SUCCESS (Matches expected value)")
		} else {
			return fmt.Errorf("checksum verification: FAILED (Expected: %s, Got: %s)", expected, checksum)
		}
	}

	return nil
}

func checkRangeSupport(ctx context.Context, client *http.Client, urlStr string) (supportsRange bool, contentLength int64, filename string, err error) {
	// 1. Try HEAD request first
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

	// 2. Fallback: try GET request with Range: bytes=0-0
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
