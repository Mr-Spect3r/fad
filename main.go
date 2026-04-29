package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"regexp"
	"bufio"
	"golang.org/x/sys/windows"
	"golang.org/x/net/proxy"
	"github.com/jlaffaye/ftp"
)

func init() {
	if runtime.GOOS == "windows" {
		stdout := windows.Handle(os.Stdout.Fd())
		var mode uint32
		if err := windows.GetConsoleMode(stdout, &mode); err == nil {
			mode |= 0x0004
			windows.SetConsoleMode(stdout, mode)
		}
	}
}

var (
	numThreads   int
	headers      headerSlice
	cookie       string
	outDir       string
	retries      int
	timeoutSec   int
	maxParallel  int
	saveSession  bool
	sessionFile  string
	fileList     string
	verbose      bool
	proxyAddr    string
	protocol     string
	ftpUser      string
	ftpPass      string
	ftpMultiPart bool
	ftpParts     int
	scrapeURL       string
	extensionsFilter string 
)

type Logger struct {
	verbose bool
	mu      sync.Mutex
}

var logger = &Logger{verbose: false}

func (l *Logger) SetVerbose(v bool) {
	l.verbose = v
}

func (l *Logger) Info(format string, args ...interface{}) {
	if l.verbose {
		l.mu.Lock()
		defer l.mu.Unlock()
		fmt.Printf(colors["cyan"]+"[INFO] "+colors["reset"]+format+"\n", args...)
	}
}

func (l *Logger) Error(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Printf(colors["red"]+"[ERROR] "+colors["reset"]+format+"\n", args...)
}

func (l *Logger) Warning(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Printf(colors["yellow"]+"[WARN] "+colors["reset"]+format+"\n", args...)
}

func (l *Logger) Debug(format string, args ...interface{}) {
	if l.verbose {
		l.mu.Lock()
		defer l.mu.Unlock()
		fmt.Printf(colors["gray"]+"[DEBUG] "+colors["reset"]+format+"\n", args...)
	}
}

func (l *Logger) Success(format string, args ...interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Printf(colors["green"]+"[✓] "+colors["reset"]+format+"\n", args...)
}

func logInfo(format string, args ...interface{}) {
	logger.Info(format, args...)
}

func logError(format string, args ...interface{}) {
	logger.Error(format, args...)
}

func logWarning(format string, args ...interface{}) {
	logger.Warning(format, args...)
}

func logDebug(format string, args ...interface{}) {
	logger.Debug(format, args...)
}

func logSuccess(format string, args ...interface{}) {
	logger.Success(format, args...)
}

type FileStatus struct {
	Name           string
	Size           int64
	SizeFormatted  string
	Done           int64
	Total          int64
	Status         string
	StartTime      time.Time
	EndTime        time.Time
	TotalThreads   int
	ActiveThreads  int
	DoneThreads    int
	ThreadProgress []int64
	BufferSize     int
	completedFlag  bool
}

type Session struct {
	URL      string
	Path     string
	Size     int64
	Ranges   [][2]int64
	FileName string
	Progress []int64
}

type GlobalStatus struct {
	mu              sync.RWMutex
	files           []*FileStatus
	downloadedCount int64
	totalCount      int64
	startTime       time.Time
	doneCh          chan struct{}
	totalDone       *int64
	lastTotalDone   *int64
}

type AdaptiveBuffer struct {
	currentSize  int
	minSize      int
	maxSize      int
	speedHistory []float64
	lastAdjust   time.Time
	mu           sync.RWMutex
}

type Downloader struct {
	url            string
	file           *os.File
	headers        http.Header
	progress       []int64
	doneCh         chan struct{}
	client         *http.Client
	size           int64
	ranges         [][2]int64
	path           string
	totalDone      *int64
	global         *GlobalStatus
	retries        int
	cancelCtx      context.CancelFunc
	adaptiveBuffer *AdaptiveBuffer
	lastBytes      int64
	lastTime       time.Time
	bufferMu       sync.Mutex
	fileName       string
	protocol       string
}

func init() {
	flag.IntVar(&numThreads, "t", runtime.NumCPU(), "Number of parallel download threads per file")
	flag.Var(&headers, "H", "Custom HTTP header (can be repeated). Format: Key: Value")
	flag.StringVar(&cookie, "c", "", "Cookie header value")
	flag.StringVar(&outDir, "o", ".", "Destination directory for downloaded files")
	flag.IntVar(&retries, "r", 5, "Retries per segment")
	flag.IntVar(&timeoutSec, "timeout", 30, "Network timeout for connection in seconds")
	flag.IntVar(&maxParallel, "u", 2, "Maximum number of simultaneous file downloads")
	flag.BoolVar(&saveSession, "save-session", true, "Save session to JSON if interrupted")
	flag.StringVar(&fileList, "f", "", "Path to file containing download URLs (one per line)")
	flag.BoolVar(&verbose, "v", false, "Verbose mode: show per-thread progress bars")
	flag.StringVar(&proxyAddr, "proxy", "", "Proxy address (socks4://host:port, socks5://host:port, http://host:port)")
	flag.StringVar(&protocol, "protocol", "auto", "Protocol to use: auto, http, https, ftp, ftps")
	flag.StringVar(&ftpUser, "ftp-user", "anonymous", "FTP username")
	flag.StringVar(&ftpPass, "ftp-pass", "anonymous@example.com", "FTP password")
	flag.BoolVar(&ftpMultiPart, "ftp-multipart", true, "Enable FTP multi-part download (faster)")
	flag.IntVar(&ftpParts, "ftp-parts", 0, "Number of FTP parts (0 = auto based on threads)")
	flag.StringVar(&scrapeURL, "scrape", "", "URL to scrape for downloadable links")
	flag.StringVar(&extensionsFilter, "ex", "", "Filter extensions to show (comma-separated, e.g., .mp4,.mp3,.zip)")
}

var colors = map[string]string{
	"reset":  "\033[0m",
	"red":    "\033[31m",
	"green":  "\033[32m",
	"yellow": "\033[33m",
	"blue":   "\033[34m",
	"cyan":   "\033[36m",
	"bold":   "\033[1m",
	"gray":   "\033[90m",
}

type headerSlice []string

func (hs *headerSlice) String() string { return strings.Join(*hs, ", ") }
func (hs *headerSlice) Set(value string) error {
	if !strings.Contains(value, ":") {
		return fmt.Errorf("invalid header: %s", value)
	}
	*hs = append(*hs, value)
	return nil
}

func NewAdaptiveBuffer() *AdaptiveBuffer {
	return &AdaptiveBuffer{
		currentSize:  64 * 1024,
		minSize:      16 * 1024,
		maxSize:      1024 * 1024,
		speedHistory: make([]float64, 0, 10),
		lastAdjust:   time.Now(),
	}
}

func (ab *AdaptiveBuffer) Update(speedMBps float64) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	if time.Since(ab.lastAdjust) < 2*time.Second {
		return
	}

	ab.speedHistory = append(ab.speedHistory, speedMBps)
	if len(ab.speedHistory) > 10 {
		ab.speedHistory = ab.speedHistory[1:]
	}

	var avgSpeed float64
	for _, s := range ab.speedHistory {
		avgSpeed += s
	}
	if len(ab.speedHistory) > 0 {
		avgSpeed /= float64(len(ab.speedHistory))
	}

	oldSize := ab.currentSize

	switch {
	case avgSpeed > 100:
		ab.currentSize = min(ab.maxSize, ab.currentSize*2)
	case avgSpeed > 50:
		ab.currentSize = min(ab.maxSize, ab.currentSize+256*1024)
	case avgSpeed > 20:
		ab.currentSize = min(ab.maxSize, ab.currentSize+128*1024)
	case avgSpeed > 10:
		ab.currentSize = min(ab.maxSize, ab.currentSize+64*1024)
	case avgSpeed > 5:
	case avgSpeed > 1:
		ab.currentSize = max(ab.minSize, ab.currentSize/2)
	case avgSpeed > 0.1:
		ab.currentSize = max(ab.minSize, ab.currentSize/4)
	default:
		ab.currentSize = ab.minSize
	}

	if ab.currentSize < ab.minSize {
		ab.currentSize = ab.minSize
	}
	if ab.currentSize > ab.maxSize {
		ab.currentSize = ab.maxSize
	}

	if oldSize != ab.currentSize && verbose {
		logDebug("Thread buffer adjusted: %s → %s (speed: %.2f MB/s)",
			formatBytes(oldSize), formatBytes(ab.currentSize), avgSpeed)
	}

	ab.lastAdjust = time.Now()
}

func (ab *AdaptiveBuffer) GetSize() int {
	ab.mu.RLock()
	defer ab.mu.RUnlock()
	return ab.currentSize
}

func formatBytes(bytes int) string {
	if bytes < 1024 {
		return fmt.Sprintf("%dB", bytes)
	} else if bytes < 1024*1024 {
		return fmt.Sprintf("%.0fKB", float64(bytes)/1024)
	}
	return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func NewGlobalStatus() *GlobalStatus {
	totalDone := int64(0)
	lastTotalDone := int64(0)
	return &GlobalStatus{
		files:         make([]*FileStatus, 0),
		doneCh:        make(chan struct{}),
		startTime:     time.Now(),
		totalDone:     &totalDone,
		lastTotalDone: &lastTotalDone,
	}
}

func (gs *GlobalStatus) addFile(name string, size int64) {
	gs.mu.Lock()
	defer gs.mu.Unlock()

	if size < 0 {
		size = 0
	}

	fileStatus := &FileStatus{
		Name:           name,
		Size:           size,
		SizeFormatted:  Size4Human(size),
		Done:           0,
		Total:          size,
		Status:         "pending",
		StartTime:      time.Now(),
		BufferSize:     64 * 1024,
		TotalThreads:   0,
		ActiveThreads:  0,
		DoneThreads:    0,
		ThreadProgress: make([]int64, 0),
		completedFlag:  false,
	}

	gs.files = append(gs.files, fileStatus)
	atomic.AddInt64(&gs.totalCount, 1)
}

func (gs *GlobalStatus) updateProgress(name string, done int64) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	for _, f := range gs.files {
		if f.Name == name {
			if done > f.Total && f.Total > 0 {
				done = f.Total
			}
			f.Done = done
			if f.Status == "pending" {
				f.Status = "downloading"
			}
			if done >= f.Total && f.Total > 0 && !f.completedFlag {
				f.Status = "downloaded"
				f.EndTime = time.Now()
				f.completedFlag = true
				atomic.AddInt64(&gs.downloadedCount, 1)
			}
			return
		}
	}
}

func (g *GlobalStatus) updateThreadProgress(name string, idx int, progress int64, segmentTotal int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, f := range g.files {
		if f.Name == name {
			if idx < len(f.ThreadProgress) {
				if progress > f.ThreadProgress[idx] {
					f.ThreadProgress[idx] = progress
				}

				doneCount := 0
				activeCount := 0
				for i, p := range f.ThreadProgress {
					var segTotal int64
					if f.Total > 0 && f.TotalThreads > 0 {
						baseSize := f.Total / int64(f.TotalThreads)
						if i == f.TotalThreads-1 {
							segTotal = f.Total - baseSize*int64(i)
						} else {
							segTotal = baseSize
						}
					} else {
						segTotal = segmentTotal
					}

					if segTotal > 0 && p >= segTotal {
						doneCount++
					} else if p > 0 {
						activeCount++
					}
				}
				f.DoneThreads = doneCount
				f.ActiveThreads = activeCount
			}
			return
		}
	}
}

func (gs *GlobalStatus) updateBufferSize(name string, size int) {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	for _, f := range gs.files {
		if f.Name == name {
			f.BufferSize = size
			return
		}
	}
}

func (gs *GlobalStatus) reportAllFiles() {
	clearScreen := "\033[2J\033[H"
	prevTotalDone := int64(0)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			fmt.Print(clearScreen)

			fmt.Printf("%s════════════════════════════════════════════════════════════════════════════════════════════════════════════════%s\n", colors["cyan"], colors["reset"])
			fmt.Printf("%s                                      DOWNLOAD STATUS%s\n", colors["bold"], colors["reset"])
			fmt.Printf("%s════════════════════════════════════════════════════════════════════════════════════════════════════════════════%s\n", colors["cyan"], colors["reset"])

			gs.mu.RLock()

			var totalDownloadedBytes int64 = 0
			var totalSizeBytes int64 = 0
			var completedFiles int64 = 0
			var activeDownloads int = 0

			for i, f := range gs.files {
				if f == nil {
					continue
				}

				var filePct float64 = 0
				if f.Total > 0 {
					filePct = float64(f.Done) * 100 / float64(f.Total)
					if filePct > 100 {
						filePct = 100
					}
				}

				var statusColor string
				var statusIcon string
				switch f.Status {
				case "pending":
					statusColor = colors["yellow"]
					statusIcon = "⏳"
				case "downloading":
					statusColor = colors["cyan"]
					statusIcon = "⬇️"
					activeDownloads++
				case "downloaded":
					statusColor = colors["green"]
					statusIcon = "✅"
					completedFiles++
				default:
					statusColor = colors["reset"]
					statusIcon = "❓"
				}

				barLen := 40
				filled := int(filePct / 100 * float64(barLen))
				if filled > barLen {
					filled = barLen
				}
				if filled < 0 {
					filled = 0
				}

				bar := fmt.Sprintf("%s%s%s%s",
					colors["green"], strings.Repeat("█", filled),
					colors["reset"], strings.Repeat("░", barLen-filled))

				displayName := f.Name
				if len(displayName) > 40 {
					displayName = displayName[:37] + "..."
				}

				fmt.Printf("%s%2d.%s %s %s%s%s %s %5.1f%%  %s/%s",
					colors["bold"], i+1, colors["reset"],
					statusIcon,
					statusColor, displayName, colors["reset"],
					bar, filePct,
					Size4Human(f.Done), Size4Human(f.Total))

				if f.Status == "downloading" && f.Done > 0 {
					elapsed := time.Since(f.StartTime).Seconds()
					if elapsed > 0 {
						speed := float64(f.Done) / 1024 / 1024 / elapsed
						fmt.Printf("  %s%.2f MB/s%s", colors["yellow"], speed, colors["reset"])

						if speed > 0 && f.Total > f.Done {
							remaining := float64(f.Total-f.Done) / 1024 / 1024 / speed
							if remaining < 3600 {
								mins := int(remaining) / 60
								secs := int(remaining) % 60
								fmt.Printf("  %sETA: %dm%ds%s", colors["cyan"], mins, secs, colors["reset"])
							}
						}
					}
				} else if f.Status == "downloaded" {
					fmt.Printf("  %s✓ Completed%s", colors["green"], colors["reset"])
				} else if f.Status == "pending" {
					fmt.Printf("  %s⏳ Waiting%s", colors["yellow"], colors["reset"])
				}

				fmt.Println()

				if verbose && f.TotalThreads > 0 {
					segSize := f.Total / int64(f.TotalThreads)

					fmt.Printf("     %s└─ Threads Progress [%d/%d completed]:%s\n", colors["gray"], f.DoneThreads, f.TotalThreads, colors["reset"])

					threadBarLen := 10

					for tIdx, tProgress := range f.ThreadProgress {
						var segTotal int64 = segSize
						if tIdx == f.TotalThreads-1 {
							segTotal = f.Total - segSize*int64(tIdx)
						}

						var tPct float64 = 0
						if segTotal > 0 {
							tPct = float64(tProgress) * 100 / float64(segTotal)
							if tPct > 100 {
								tPct = 100
							}
						}

						var tColor string
						var threadStatusIcon string
						var statusText string

						if tPct >= 99.99 || (segTotal > 0 && tProgress >= segTotal) {
							tColor = colors["green"]
							threadStatusIcon = "✅"
							statusText = "Complete"
						} else if tPct > 0 {
							tColor = colors["cyan"]
							threadStatusIcon = "⬇️"
							statusText = fmt.Sprintf("Downloading (%.1f%%)", tPct)
						} else {
							tColor = colors["yellow"]
							threadStatusIcon = "⏳"
							statusText = "Waiting"
						}

						filledCh := int(tPct / 100 * float64(threadBarLen))
						if filledCh > threadBarLen {
							filledCh = threadBarLen
						}
						if filledCh < 0 {
							filledCh = 0
						}
						
						threadBar := fmt.Sprintf("%s%s%s%s",
							colors["green"], strings.Repeat("█", filledCh),
							colors["reset"], strings.Repeat("░", threadBarLen-filledCh))

						fmt.Printf("     %s   T%d: %s %s [%s] %s %s\n",
							colors["gray"],
							tIdx+1,
							threadStatusIcon,
							tColor, threadBar, colors["reset"],
							statusText)
					}
				}

				if f.Status == "downloaded" || f.Done >= f.Total {
					totalDownloadedBytes += f.Size
				} else {
					totalDownloadedBytes += f.Done
				}
				totalSizeBytes += f.Size
			}
			gs.mu.RUnlock()

			elapsed := time.Since(gs.startTime).Seconds()
			currentTotalDone := atomic.LoadInt64(gs.totalDone)
			diff := currentTotalDone - prevTotalDone
			prevTotalDone = currentTotalDone

			var avgSpeed, instSpeed float64
			if elapsed > 0 {
				avgSpeed = float64(currentTotalDone) / 1024 / 1024 / elapsed
			}
			if diff > 0 {
				instSpeed = float64(diff) / 1024 / 1024 / 0.5
			}

			var totalPercent float64 = 0
			if totalSizeBytes > 0 {
				totalPercent = (float64(totalDownloadedBytes) * 100.0) / float64(totalSizeBytes)
				if totalPercent > 100 {
					totalPercent = 100
				}
			}

			fmt.Printf("%s────────────────────────────────────────────────────────────────────────────────────────────────────────────%s\n", colors["cyan"], colors["reset"])

			downloadedCount := atomic.LoadInt64(&gs.downloadedCount)
			totalFiles := len(gs.files)

			fmt.Printf("%s Avg Speed:%s %s%.2f MB/s%s  %s Instant:%s %s%.2f MB/s%s  %s Active:%s %s%d%s\n",
				colors["bold"], colors["reset"],
				colors["green"], avgSpeed, colors["reset"],
				colors["bold"], colors["reset"],
				colors["yellow"], instSpeed, colors["reset"],
				colors["bold"], colors["reset"],
				colors["cyan"], activeDownloads, colors["reset"])

			statsLine := fmt.Sprintf(" Files: %d/%d  Downloaded: %s / %s (%.2f%%)  Elapsed: %.1fs",
				downloadedCount, totalFiles,
				Size4Human(totalDownloadedBytes),
				Size4Human(totalSizeBytes),
				totalPercent, elapsed)
			fmt.Println(statsLine)

			if totalPercent > 0 && totalPercent < 100 && avgSpeed > 0 {
				remainingBytes := float64(totalSizeBytes - totalDownloadedBytes)
				remainingTime := remainingBytes / 1024 / 1024 / avgSpeed
				if remainingTime > 0 && remainingTime < 3600 {
					mins := int(remainingTime) / 60
					secs := int(remainingTime) % 60
					remainingMB := remainingBytes / 1024 / 1024
					fmt.Printf("%s Remaining:%s %s%dm%ds%s  %s Left:%s %s%.1fMB%s\n",
						colors["bold"], colors["reset"],
						colors["yellow"], mins, secs, colors["reset"],
						colors["bold"], colors["reset"],
						colors["yellow"], remainingMB, colors["reset"])
				}
			}

			if completedFiles == int64(totalFiles) && totalFiles > 0 {
				logSuccess("All downloads finished!")
				time.Sleep(2 * time.Second)
				close(gs.doneCh)
				return
			}

		case <-gs.doneCh:
			fmt.Print(clearScreen)
			fmt.Printf("%s════════════════════════════════════════════════════════════════════════════════════════════════════════════════%s\n", colors["cyan"], colors["reset"])
			fmt.Printf("%s                                      FINAL STATUS%s\n", colors["bold"], colors["reset"])
			fmt.Printf("%s════════════════════════════════════════════════════════════════════════════════════════════════════════════════%s\n", colors["cyan"], colors["reset"])

			gs.mu.RLock()
			var totalDownloadedBytes int64 = 0
			var totalSizeBytes int64 = 0

			for i, f := range gs.files {
				if f == nil {
					continue
				}

				displayName := f.Name
				if len(displayName) > 60 {
					displayName = displayName[:57] + "..."
				}

				statusIcon := "✅"
				if f.Status != "downloaded" {
					statusIcon = "⚠️"
				}

				fmt.Printf("%s%2d.%s %s %s - %s/%s\n",
					colors["bold"], i+1, colors["reset"],
					statusIcon, displayName,
					Size4Human(f.Done), Size4Human(f.Total))

				totalDownloadedBytes += f.Done
				totalSizeBytes += f.Total
			}
			gs.mu.RUnlock()

			totalTime := time.Since(gs.startTime).Seconds()
			fmt.Printf("%s────────────────────────────────────────────────────────────────────────────────────────────────────────────%s\n", colors["cyan"], colors["reset"])
			fmt.Printf("%s⏱️ Total time:%s %s%.1fs%s\n", colors["bold"], colors["reset"], colors["yellow"], totalTime, colors["reset"])
			if totalTime > 0 && totalSizeBytes > 0 {
				fmt.Printf("%s⚡ Average speed:%s %s%.2f MB/s%s\n", colors["bold"], colors["reset"], colors["green"], float64(totalSizeBytes)/1024/1024/totalTime, colors["reset"])
			}
			fmt.Printf("%s💾 Total downloaded:%s %s%s%s\n", colors["bold"], colors["reset"], colors["green"], Size4Human(totalDownloadedBytes), colors["reset"])
			fmt.Printf("%s📊 Completion:%s %s%.1f%%%s\n", colors["bold"], colors["reset"], colors["green"], float64(totalDownloadedBytes)*100/float64(totalSizeBytes), colors["reset"])
			fmt.Printf("\n%s✅ All downloads completed!%s\n", colors["green"], colors["reset"])
			return
		}
	}
}

func (gs *GlobalStatus) totalSize() int64 {
	gs.mu.RLock()
	defer gs.mu.RUnlock()
	var total int64
	for _, f := range gs.files {
		total += f.Size
	}
	return total
}

func showUsage() {
	fmt.Println("FAD - Fast Advanced Downloader")
	fmt.Println("\nUSAGE:")
	fmt.Println("  fad [OPTIONS] <url1> <url2> ...")
	fmt.Println("  fad -f <file-list> [OPTIONS]")
	fmt.Println("  fad <session.json> (resume download)")
	fmt.Println("\nOPTIONS:")
	flag.PrintDefaults()
	fmt.Println("\nPROXY SUPPORT (SOCKS4/SOCKS5/HTTP):")
	fmt.Println("  -proxy socks4://host:port     Use SOCKS4 proxy")
	fmt.Println("  -proxy socks5://host:port     Use SOCKS5 proxy")
	fmt.Println("  -proxy http://host:port       Use HTTP proxy")
	fmt.Println("  Example: -proxy socks5://127.0.0.1:1080")
	fmt.Println("\nPROTOCOL SUPPORT:")
	fmt.Println("  -protocol auto    Auto-detect protocol (default)")
	fmt.Println("  -protocol http     Force HTTP")
	fmt.Println("  -protocol https    Force HTTPS")
	fmt.Println("  -protocol ftp      FTP protocol (with resume support)")
	fmt.Println("  -protocol ftps     FTPS protocol (FTP over TLS)")
	fmt.Println("\nFTP OPTIONS:")
	fmt.Println("  -ftp-user USER     FTP username (default: anonymous)")
	fmt.Println("  -ftp-pass PASS     FTP password (default: anonymous@example.com)")
	fmt.Println("  -ftp-multipart     Enable multi-part FTP download (faster, default: true)")
	fmt.Println("  -ftp-parts NUM     Number of FTP parts (0 = auto)")
	fmt.Println("\nEXAMPLES:")
	fmt.Println("  # Download via SOCKS5 proxy")
	fmt.Println("  ./fad -proxy socks5://127.0.0.1:1080 https://example.com/file.zip")
	fmt.Println("")
	fmt.Println("  # Download via SOCKS4 proxy with 8 threads")
	fmt.Println("  ./fad -proxy socks4://192.168.1.1:9050 -t 8 https://example.com/file.zip")
	fmt.Println("")
	fmt.Println("  # Download FTP file with multi-part (faster)")
	fmt.Println("  ./fad -protocol ftp -ftp-multipart -ftp-parts 8 ftp://example.com/file.zip")
	fmt.Println("")
	fmt.Println("  # Download FTPS (FTP over TLS)")
	fmt.Println("  ./fad -protocol ftps ftps://example.com/secure-file.zip")
	fmt.Println("")
	fmt.Println("  # Download multiple files with HTTP proxy")
	fmt.Println("  ./fad -proxy http://proxy.company.com:8080 url1 url2 url3")
}

func createHTTPClient() *http.Client {
	transport := &http.Transport{
		MaxIdleConns:          2000,
		MaxIdleConnsPerHost:   numThreads * 2,
		TLSHandshakeTimeout:   30 * time.Second,
		DisableCompression:    false,
		IdleConnTimeout:       120 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		WriteBufferSize:       32 * 1024,
		ReadBufferSize:        32 * 1024,
	}

	dialer := &net.Dialer{
		Timeout:   time.Duration(timeoutSec) * time.Second,
		KeepAlive: 90 * time.Second,
	}

	if proxyAddr != "" {
		switch {
		case strings.HasPrefix(proxyAddr, "socks5://"):
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				var auth *proxy.Auth
				if proxyURL.User != nil {
					password, _ := proxyURL.User.Password()
					auth = &proxy.Auth{
						User:     proxyURL.User.Username(),
						Password: password,
					}
				}

				socksDialer, err := proxy.SOCKS5("tcp", proxyURL.Host, auth, dialer)
				if err == nil {
					transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
						return socksDialer.Dial(network, addr)
					}
					logInfo("Using SOCKS5 proxy: %s", proxyAddr)
				} else {
					logError("Failed to setup SOCKS5 proxy: %v", err)
				}
			}

		case strings.HasPrefix(proxyAddr, "socks4://"):
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				dialerSocks4 := &net.Dialer{
					Timeout:   time.Duration(timeoutSec) * time.Second,
					KeepAlive: 90 * time.Second,
				}
				transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					proxyConn, err := dialerSocks4.DialContext(ctx, "tcp", proxyURL.Host)
					if err != nil {
						return nil, err
					}

					host, port, _ := net.SplitHostPort(addr)
					portInt, _ := strconv.Atoi(port)

					packet := []byte{4, 1}
					packet = append(packet, byte(portInt>>8), byte(portInt&0xFF))

					ip := net.ParseIP(host)
					if ip == nil {
						ip = net.IPv4(0, 0, 0, 1)
					}
					ip4 := ip.To4()
					packet = append(packet, ip4...)

					packet = append(packet, []byte("downloader")...)
					packet = append(packet, 0)

					_, err = proxyConn.Write(packet)
					if err != nil {
						proxyConn.Close()
						return nil, err
					}

					response := make([]byte, 8)
					_, err = proxyConn.Read(response)
					if err != nil || response[1] != 90 {
						proxyConn.Close()
						return nil, fmt.Errorf("SOCKS4 handshake failed")
					}

					return proxyConn, nil
				}
				logInfo("Using SOCKS4 proxy: %s", proxyAddr)
			}

		case strings.HasPrefix(proxyAddr, "http://"), strings.HasPrefix(proxyAddr, "https://"):
			proxyURL, err := url.Parse(proxyAddr)
			if err == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
				logInfo("Using HTTP proxy: %s", proxyAddr)
			}

		default:
			logWarning("Unsupported proxy format. Use socks4://, socks5://, or http://")
		}
	}

	if transport.DialContext == nil {
		transport.DialContext = dialer.DialContext
	}

	return &http.Client{
		Transport: transport,
		Timeout:   0,
	}
}

func downloadFTPMultiPart(fileURL string, global *GlobalStatus, numParts int) {
	parsedURL, err := url.Parse(fileURL)
	if err != nil {
		logError("Invalid FTP URL: %v", err)
		return
	}

	host := parsedURL.Host
	path := parsedURL.Path
	if path == "" {
		path = "/"
	}

	fileName := filepath.Base(path)
	if fileName == "" || fileName == "." || fileName == "/" {
		fileName = fmt.Sprintf("ftp_download_%d", time.Now().Unix())
	}

	outPath := filepath.Join(outDir, fileName)

	ftpClient, err := connectFTP(host, protocol == "ftps")
	if err != nil {
		logError("FTP connection failed: %v", err)
		return
	}
	defer ftpClient.Quit()

	if err := ftpClient.Login(ftpUser, ftpPass); err != nil {
		logError("FTP login failed: %v", err)
		return
	}

	size, err := ftpClient.FileSize(path)
	if err != nil {
		logWarning("Cannot get file size: %v, using single thread", err)
		downloadFTPSingle(fileURL, global)
		return
	}

	if size < 10*1024*1024 { 
		logInfo("File too small for multi-part (%s), using single thread", Size4Human(size))
		downloadFTPSingle(fileURL, global)
		return
	}

	logInfo("FTP Multi-part download: %s (%s) with %d parts", fileName, Size4Human(size), numParts)

	var existingSize int64 = 0
	if info, err := os.Stat(outPath); err == nil {
		existingSize = info.Size()
		if existingSize >= size && size > 0 {
			logSuccess("File already exists: %s", fileName)
			if global != nil {
				global.addFile(fileName, size)
				global.updateProgress(fileName, size)
			}
			return
		}
	}

	file, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		logError("Cannot create file: %v", err)
		return
	}
	defer file.Close()

	if size > 0 {
		file.Truncate(size)
	}

	partSize := size / int64(numParts)
	ranges := make([][2]int64, numParts)
	for i := 0; i < numParts; i++ {
		start := int64(i) * partSize
		end := start + partSize - 1
		if i == numParts-1 {
			end = size - 1
		}
		ranges[i] = [2]int64{start, end}
	}

	progress := make([]int64, numParts)
	if existingSize > 0 {
		for i := range progress {
			if existingSize > ranges[i][0] {
				progress[i] = min64(existingSize-ranges[i][0], ranges[i][1]-ranges[i][0]+1)
			}
		}
	}

	if global != nil {
		global.addFile(fileName, size)
		global.mu.Lock()
		for _, f := range global.files {
			if f.Name == fileName {
				f.TotalThreads = numParts
				f.ThreadProgress = progress
				break
			}
		}
		global.mu.Unlock()
	}

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, numParts)

	for i, r := range ranges {
		if progress[i] >= (r[1]-r[0]+1) {
			continue
		}

		wg.Add(1)
		go func(partIdx int, start, end int64) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if err := downloadFTPPart(host, path, file, partIdx, start, end, progress[partIdx], global, fileName); err != nil {
				logError("FTP part %d failed: %v", partIdx, err)
			}
		}(i, r[0], r[1])
	}

	wg.Wait()
	logSuccess("FTP multi-part download completed: %s", fileName)

	if global != nil {
		global.updateProgress(fileName, size)
	}
}

func downloadFTPPart(host, path string, file *os.File, partIdx int, start, end int64, existing int64, global *GlobalStatus, fileName string) error {
	ftpClient, err := connectFTP(host, protocol == "ftps")
	if err != nil {
		return fmt.Errorf("connection failed: %v", err)
	}
	defer ftpClient.Quit()

	if err := ftpClient.Login(ftpUser, ftpPass); err != nil {
		return fmt.Errorf("login failed: %v", err)
	}

	startPos := start + existing

	reader, err := ftpClient.RetrFrom(path, uint64(startPos))
	if err != nil {
		return fmt.Errorf("retr failed: %v", err)
	}
	defer reader.Close()

	bufferSize := 64 * 1024
	buffer := make([]byte, bufferSize)
	downloaded := existing
	totalSize := end - start + 1

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			if startPos+int64(n) > end+1 {
				n = int(end + 1 - startPos)
			}

			_, writeErr := file.WriteAt(buffer[:n], startPos)
			if writeErr != nil {
				return fmt.Errorf("write error: %v", writeErr)
			}

			startPos += int64(n)
			downloaded += int64(n)

			if global != nil {
				global.updateThreadProgress(fileName, partIdx, downloaded, totalSize)

				var total int64
				global.mu.RLock()
				for _, f := range global.files {
					if f.Name == fileName {
						for _, p := range f.ThreadProgress {
							total += p
						}
						break
					}
				}
				global.mu.RUnlock()
				global.updateProgress(fileName, total)
				atomic.AddInt64(global.totalDone, int64(n))
			}
		}

		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read error: %v", err)
		}

		if startPos > end {
			break
		}
	}

	return nil
}

func connectFTP(host string, useTLS bool) (*ftp.ServerConn, error) {
	if useTLS {
		tlsConfig := &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         strings.Split(host, ":")[0],
		}
		return ftp.Dial(host+":21", ftp.DialWithTLS(tlsConfig))
	}
	return ftp.Dial(host + ":21")
}


func downloadFTPSingle(fileURL string, global *GlobalStatus) {
	parsedURL, err := url.Parse(fileURL)
	if err != nil {
		logError("Invalid FTP URL: %v", err)
		return
	}

	host := parsedURL.Host
	path := parsedURL.Path
	if path == "" {
		path = "/"
	}

	fileName := filepath.Base(path)
	if fileName == "" || fileName == "." || fileName == "/" {
		fileName = fmt.Sprintf("ftp_download_%d", time.Now().Unix())
	}

	outPath := filepath.Join(outDir, fileName)

	ftpClient, err := connectFTP(host, protocol == "ftps")
	if err != nil {
		logError("FTP connection failed: %v", err)
		return
	}
	defer ftpClient.Quit()

	if err := ftpClient.Login(ftpUser, ftpPass); err != nil {
		logError("FTP login failed: %v", err)
		return
	}

	size, err := ftpClient.FileSize(path)
	if err != nil {
		logWarning("Cannot get file size: %v", err)
		size = -1
	}

	logInfo("FTP Single-thread download: %s (%s)", fileName, Size4Human(size))

	var existingSize int64 = 0
	if info, err := os.Stat(outPath); err == nil {
		existingSize = info.Size()
		if existingSize > 0 && existingSize < size {
			logInfo("Resuming from %s", Size4Human(existingSize))
		} else if existingSize >= size && size > 0 {
			logSuccess("File already exists: %s", fileName)
			if global != nil {
				global.addFile(fileName, size)
				global.updateProgress(fileName, size)
			}
			return
		}
	}

	file, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logError("Cannot create file: %v", err)
		return
	}
	defer file.Close()

	if existingSize > 0 {
		file.Seek(existingSize, io.SeekStart)
	}

	var reader io.ReadCloser
	if existingSize > 0 {
		reader, err = ftpClient.RetrFrom(path, uint64(existingSize))
	} else {
		reader, err = ftpClient.Retr(path)
	}

	if err != nil {
		logError("FTP download failed: %v", err)
		return
	}
	defer reader.Close()

	if global != nil {
		global.addFile(fileName, size)
	}

	buffer := make([]byte, 32*1024)
	downloaded := existingSize
	startTime := time.Now()
	lastUpdate := time.Now()

	for {
		n, err := reader.Read(buffer)
		if n > 0 {
			_, writeErr := file.Write(buffer[:n])
			if writeErr != nil {
				logError("Write error: %v", writeErr)
				return
			}
			downloaded += int64(n)

			if global != nil {
				global.updateProgress(fileName, downloaded)
				atomic.AddInt64(global.totalDone, int64(n))
			}

			if time.Since(lastUpdate) >= time.Second {
				elapsed := time.Since(startTime).Seconds()
				var speed float64
				if elapsed > 0 {
					speed = float64(downloaded-existingSize) / 1024 / 1024 / elapsed
				}
				var pct float64
				if size > 0 {
					pct = float64(downloaded) * 100 / float64(size)
				} else {
					pct = 0
				}
				fmt.Printf("\r%s↻ Progress: %.1f%% (%.2f MB/s) %s/%s%s",
					colors["cyan"], pct, speed,
					Size4Human(downloaded), Size4Human(size), colors["reset"])
				lastUpdate = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Printf("\n")
			logError("Read error: %v", err)
			return
		}
	}

	fmt.Printf("\n")
	logSuccess("FTP download completed: %s", fileName)

	if global != nil && size > 0 {
		global.updateProgress(fileName, size)
	}
}

func downloadFTP(fileURL string, global *GlobalStatus) {
	if !ftpMultiPart {
		downloadFTPSingle(fileURL, global)
		return
	}

	parts := ftpParts
	if parts <= 0 {
		parts = numThreads
		if parts > 16 {
			parts = 16 
		}
		if parts < 2 {
			parts = 2
		}
	}

	downloadFTPMultiPart(fileURL, global, parts)
}

func downloadSingle(url string, client *http.Client, global *GlobalStatus) {
	protocolDetected := protocol
	if protocolDetected == "auto" {
		if strings.HasPrefix(url, "ftp://") {
			protocolDetected = "ftp"
		} else if strings.HasPrefix(url, "ftps://") {
			protocolDetected = "ftps"
		} else if strings.HasPrefix(url, "https://") {
			protocolDetected = "https"
		} else {
			protocolDetected = "http"
		}
	}

	if protocolDetected == "ftp" || protocolDetected == "ftps" {
		downloadFTP(url, global)
		return
	}

	fileName, size, err := fetchFileInfo(url, client)
	if err != nil {
		logError("Error fetching file info: %v", err)
		return
	}
	
	if size <= 0 {
		logError("Invalid file size (%d bytes) for %s, cannot download", size, fileName)
		return
	}

	outPath := filepath.Join(outDir, fileName)

	var existingProgress []int64
	if _, err := os.Stat(outPath + ".progress"); err == nil {
		if data, err := os.ReadFile(outPath + ".progress"); err == nil {
			var progressData struct {
				Progress []int64
				Ranges   [][2]int64
			}
			if json.Unmarshal(data, &progressData) == nil {
				existingProgress = progressData.Progress
				logInfo("Found partial download, resuming...")
			}
		}
	}

	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		die("Cannot create file:", err)
	}
	defer f.Close()

	if size > 0 {
		f.Truncate(size)
	}

	numThreadsEffective := numThreads
	req, _ := http.NewRequest("HEAD", url, nil)
	resp, err := client.Do(req)
	if err == nil {
		if !strings.Contains(resp.Header.Get("Accept-Ranges"), "bytes") {
			numThreadsEffective = 1
			logDebug("Server doesn't support range requests, using single thread")
		}
		resp.Body.Close()
	} else {
		numThreadsEffective = 1
	}

	var ranges [][2]int64
	part := int64(0)
	if size > 0 {
		part = size / int64(numThreadsEffective)
		for i := 0; i < numThreadsEffective; i++ {
			start := int64(i) * part
			end := start + part - 1
			if i == numThreadsEffective-1 {
				end = size - 1
			}
			ranges = append(ranges, [2]int64{start, end})
		}
	} else {
		ranges = append(ranges, [2]int64{0, -1})
	}

	progress := make([]int64, len(ranges))
	if len(existingProgress) == len(ranges) {
		copy(progress, existingProgress)
	}

	ctx, cancel := context.WithCancel(context.Background())

	dl := &Downloader{
		url:            url,
		file:           f,
		headers:        make(http.Header),
		progress:       progress,
		doneCh:         make(chan struct{}),
		client:         client,
		size:           size,
		ranges:         ranges,
		path:           outPath,
		totalDone:      global.totalDone,
		global:         global,
		retries:        retries,
		cancelCtx:      cancel,
		adaptiveBuffer: NewAdaptiveBuffer(),
		fileName:       fileName,
	}
	dl.headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	if cookie != "" {
		dl.headers.Set("Cookie", cookie)
	}
	for _, h := range headers {
		parts := strings.SplitN(h, ":", 2)
		if len(parts) == 2 {
			dl.headers.Set(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
		}
	}

	global.mu.Lock()
	for _, fi := range global.files {
		if fi.Name == fileName {
			fi.TotalThreads = len(ranges)
			fi.ActiveThreads = len(ranges)
			fi.DoneThreads = 0
			fi.ThreadProgress = make([]int64, len(ranges))
			copy(fi.ThreadProgress, progress)
			break
		}
	}
	global.mu.Unlock()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		logInfo("Interrupt detected, saving session...")
		dl.saveSession()
		os.Exit(0)
	}()

	saveTicker := time.NewTicker(10 * time.Second)
	defer saveTicker.Stop()

	go func() {
		for {
			select {
			case <-saveTicker.C:
				dl.saveProgress()
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i, seg := range ranges {
		if progress[i] >= (seg[1]-seg[0]+1) && seg[1] >= 0 {
			atomic.AddInt64(&dl.progress[i], progress[i])
			global.mu.Lock()
			for _, fi := range global.files {
				if fi.Name == fileName {
					fi.DoneThreads++
					fi.ThreadProgress[i] = progress[i]
					break
				}
			}
			global.mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(i int, s, e int64) {
			defer wg.Done()
			if err := dl.downloadPart(i, s, e); err != nil {
				logError("Thread %d error: %v", i, err)
			} else {
				logDebug("Thread %d completed successfully", i)
			}
		}(i, seg[0], seg[1])
	}

	wg.Wait()
	cancel()

	time.Sleep(1 * time.Second)

	os.Remove(outPath + ".progress")

global.mu.Lock()
for _, fi := range global.files {
    if fi.Name == fileName {
        fi.DoneThreads = fi.TotalThreads
        fi.ActiveThreads = 0
        if !fi.completedFlag {  
            fi.Status = "downloaded"
            fi.EndTime = time.Now()
            fi.completedFlag = true
            atomic.AddInt64(&global.downloadedCount, 1)
        }
        if fi.Done < fi.Total {
            fi.Done = fi.Total
        }
        break
    }
}
global.mu.Unlock()

	close(dl.doneCh)
}

func (dl *Downloader) saveProgress() {
	progressData := struct {
		Progress []int64
		Ranges   [][2]int64
	}{
		Progress: make([]int64, len(dl.progress)),
		Ranges:   dl.ranges,
	}
	for i, p := range dl.progress {
		progressData.Progress[i] = atomic.LoadInt64(&p)
	}

	data, _ := json.Marshal(progressData)
	os.WriteFile(dl.path+".progress", data, 0644)
}

func fetchFileInfo(url string, client *http.Client) (name string, size int64, err error) {

	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return "", 0, fmt.Errorf("HTTP error: %d", resp.StatusCode)
	}
	
	name = getFileName(url, resp)
	
	if resp.ContentLength > 0 {
		size = resp.ContentLength
		logDebug("Got size from HEAD: %d bytes", size)
		return name, size, nil
	}
	
	logDebug("HEAD didn't return size, trying GET with Range")
	req2, _ := http.NewRequest("GET", url, nil)
	req2.Header.Set("User-Agent", req.Header.Get("User-Agent"))
	req2.Header.Set("Range", "bytes=0-0")
	
	resp2, err := client.Do(req2)
	if err != nil {
		return name, -1, err
	}
	defer resp2.Body.Close()
	
	contentRange := resp2.Header.Get("Content-Range")
	if contentRange != "" {
		parts := strings.Split(contentRange, "/")
		if len(parts) == 2 {
			if s, err := strconv.ParseInt(parts[1], 10, 64); err == nil && s > 0 {
				size = s
				logDebug("Got size from Content-Range: %d bytes", size)
				return name, size, nil
			}
		}
	}
	
	logWarning("Could not determine file size via HEAD or Range, downloading entire file to get size...")
	
	req3, _ := http.NewRequest("GET", url, nil)
	req3.Header.Set("User-Agent", req.Header.Get("User-Agent"))
	
	resp3, err := client.Do(req3)
	if err != nil {
		return name, -1, err
	}
	defer resp3.Body.Close()
	
	size, err = io.Copy(io.Discard, resp3.Body)
	if err != nil {
		return name, -1, err
	}
	
	logDebug("Got size by downloading full file: %d bytes", size)
	return name, size, nil
}
func (dl *Downloader) downloadPart(idx int, start, end int64) error {
	var segmentTotal int64
	if end >= 0 {
		segmentTotal = end - start + 1
	} else {
		segmentTotal = dl.size - start
	}

	success := false

	defer func() {
		if success && dl.global != nil {
			finalProgress := atomic.LoadInt64(&dl.progress[idx])
			dl.global.updateThreadProgress(dl.fileName, idx, finalProgress, segmentTotal)

			var total int64
			for i := range dl.progress {
				total += atomic.LoadInt64(&dl.progress[i])
			}
			dl.global.updateProgress(dl.fileName, total)
		}
	}()

	for attempt := 1; attempt <= dl.retries; attempt++ {
		downloaded := atomic.LoadInt64(&dl.progress[idx])
		currentStart := start + downloaded

		if end >= 0 && currentStart > end {
			success = true
			return nil
		}

		req, err := http.NewRequest("GET", dl.url, nil)
		if err != nil {
			if attempt < dl.retries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return err
		}

		req.Header = dl.headers.Clone()
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", currentStart, end))
		}

		resp, err := dl.client.Do(req)
		if err != nil {
			if attempt < dl.retries {
				time.Sleep(time.Duration(attempt) * time.Second)
				continue
			}
			return fmt.Errorf("request failed: %v", err)
		}

		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			resp.Body.Close()
			success = true
			return nil
		}

		if resp.StatusCode != http.StatusPartialContent && end != -1 {
			resp.Body.Close()
			if end != -1 && attempt == dl.retries {
				end = -1
				currentStart = start
				continue
			}
			if attempt < dl.retries {
				time.Sleep(time.Duration(attempt) * time.Second)
				resp.Body.Close()
				continue
			}
			resp.Body.Close()
			return fmt.Errorf("server returned %d", resp.StatusCode)
		}

		pos := currentStart
		buf := make([]byte, dl.adaptiveBuffer.GetSize())

		for {
			n, readErr := resp.Body.Read(buf)

			if n > 0 {
				writeN := n
				if end >= 0 && pos+int64(n) > end+1 {
					writeN = int(end + 1 - pos)
				}

				if writeN > 0 {
					_, writeErr := dl.file.WriteAt(buf[:writeN], pos)
					if writeErr != nil {
						resp.Body.Close()
						return writeErr
					}

					pos += int64(writeN)
					atomic.AddInt64(&dl.progress[idx], int64(writeN))
					atomic.AddInt64(dl.totalDone, int64(writeN))

					if dl.global != nil {
						currentProgress := atomic.LoadInt64(&dl.progress[idx])
						dl.global.updateThreadProgress(dl.fileName, idx, currentProgress, segmentTotal)

						var total int64
						for i := range dl.progress {
							total += atomic.LoadInt64(&dl.progress[i])
						}
						dl.global.updateProgress(dl.fileName, total)
					}
				}
			}

			if readErr == io.EOF {
				resp.Body.Close()
				currentProgress := atomic.LoadInt64(&dl.progress[idx])
				if currentProgress >= segmentTotal || (end < 0) {
					success = true
				}
				return nil
			}

			if readErr != nil {
				resp.Body.Close()
				if attempt < dl.retries {
					time.Sleep(time.Duration(attempt) * time.Second)
					break
				}
				return fmt.Errorf("read error: %v", readErr)
			}
		}
	}

	return fmt.Errorf("segment %d failed after %d retries", idx, dl.retries)
}

func progressBarBeautiful(pct int, length int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}

	filled := int(float64(length) * float64(pct) / 100)
	empty := length - filled

	bar := fmt.Sprintf("%s%s%s%s%s",
		colors["green"], strings.Repeat("█", filled),
		colors["blue"], strings.Repeat("░", empty),
		colors["reset"],
	)

	return fmt.Sprintf("[%s] %6.2f%%", bar, float64(pct))
}

func displayFileProgress(f *FileStatus) string {
	var pct float64
	if f.Total > 0 {
		pct = float64(f.Done) * 100 / float64(f.Total)
	}

	barLen := 30
	filled := int(pct / 100 * float64(barLen))
	if filled > barLen {
		filled = barLen
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", barLen-filled)

	return fmt.Sprintf("%-30s [%s] %5.1f%%  %s/%s",
		truncateString(f.Name, 30),
		bar,
		pct,
		Size4Human(f.Done),
		Size4Human(f.Total))
}

func formatDuration(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	if seconds < 3600 {
		mins := int(seconds) / 60
		secs := int(seconds) % 60
		return fmt.Sprintf("%dm%ds", mins, secs)
	}
	hours := int(seconds) / 3600
	mins := int(seconds) % 3600 / 60
	return fmt.Sprintf("%dh%dm", hours, mins)
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

func (dl *Downloader) saveSession() {
	fileName := filepath.Base(dl.path)
	if fileName == "" || fileName == "/" {
		fileName = fmt.Sprintf("file_%d%s", time.Now().Unix(), filepath.Ext(dl.url))
	}

	progressCopy := make([]int64, len(dl.progress))
	for i, v := range dl.progress {
		progressCopy[i] = atomic.LoadInt64(&v)
	}

	s := Session{
		URL:      dl.url,
		Path:     dl.path,
		Size:     dl.size,
		Ranges:   dl.ranges,
		FileName: fileName,
		Progress: progressCopy,
	}
	fname := dl.path + ".json"
	f, err := os.Create(fname)
	if err != nil {
		logError("Error saving session: %v", err)
		return
	}
	json.NewEncoder(f).Encode(s)
	f.Close()
	logInfo("Session saved → %s", fname)
}

func resumeFromSession(file string, global *GlobalStatus) {
	f, err := os.Open(file)
	if err != nil {
		die("Cannot open session:", err)
	}
	defer f.Close()

	var s Session
	if err := json.NewDecoder(f).Decode(&s); err != nil {
		die("Invalid session JSON:", err)
	}

	client := createHTTPClient()
	if client == nil {
		die("Failed to create HTTP client")
	}

	fileName := s.FileName
	if fileName == "" || fileName == "/" {
		fileName = filepath.Base(s.Path)
		if fileName == "" || fileName == "/" {
			fileName = fmt.Sprintf("file_%d%s", time.Now().Unix(), filepath.Ext(s.URL))
		}
	}
	outPath := filepath.Join(outDir, fileName)

	fout, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		die("Cannot open/create file:", err)
	}
	defer fout.Close()

	if len(s.Ranges) == 0 {
		s.Ranges = [][2]int64{{0, s.Size - 1}}
	}

	if len(s.Progress) == 0 || len(s.Progress) != len(s.Ranges) {
		s.Progress = make([]int64, len(s.Ranges))
	}

	fmt.Printf("%s╔════════════════════════════════════════════════════════════════════════════╗%s\n", colors["cyan"], colors["reset"])
	fmt.Printf("%s║                           RESUMING DOWNLOAD                                ║%s\n", colors["bold"], colors["reset"])
	fmt.Printf("%s╚════════════════════════════════════════════════════════════════════════════╝%s\n", colors["cyan"], colors["reset"])
	fmt.Printf("%s File:%s %s\n", colors["blue"], colors["reset"], s.URL)
	fmt.Printf("%s Size:%s %s\n", colors["blue"], colors["reset"], Size4Human(s.Size))

	totalDone := int64(0)
	for _, v := range s.Progress {
		totalDone += v
	}

	dl := &Downloader{
		url:            s.URL,
		file:           fout,
		headers:        make(http.Header),
		progress:       s.Progress,
		doneCh:         make(chan struct{}),
		client:         client,
		size:           s.Size,
		path:           outPath,
		ranges:         s.Ranges,
		totalDone:      &totalDone,
		retries:        retries,
		global:         global,
		adaptiveBuffer: NewAdaptiveBuffer(),
		fileName:       fileName,
	}
	dl.headers.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	var wg sync.WaitGroup
	for i, seg := range s.Ranges {
		wg.Add(1)
		go func(i int, st, en int64) {
			defer wg.Done()
			if err := dl.downloadPart(i, st, en); err != nil {
				logError("Thread %d error: %v", i, err)
			}
		}(i, seg[0], seg[1])
	}

	go func() {
		barLen := 50
		clearScreen := "\033[2J\033[H"
		for {
			select {
			case <-time.After(500 * time.Millisecond):
				done := int64(0)
				for _, v := range dl.progress {
					done += atomic.LoadInt64(&v)
				}
				pct := float64(done) * 100 / float64(dl.size)
				if dl.size <= 0 {
					pct = 100
				}
				fmt.Print(clearScreen)
				fmt.Printf("%s╔════════════════════════════════════════════════════════════════════════════╗%s\n", colors["cyan"], colors["reset"])
				fmt.Printf("%s║                           RESUMING DOWNLOAD                                ║%s\n", colors["bold"], colors["reset"])
				fmt.Printf("%s╚════════════════════════════════════════════════════════════════════════════╝%s\n", colors["cyan"], colors["reset"])
				fmt.Printf("%s File:%s %s\n", colors["blue"], colors["reset"], fileName)
				fmt.Println(strings.Repeat("─", 70))
				fmt.Printf(" %s\n", progressBarBeautiful(int(pct), barLen))
				fmt.Printf(" %6.2f%% │ %s/%s\n",
					pct,
					Size4Human(done),
					Size4Human(dl.size),
				)
				fmt.Println(strings.Repeat("─", 70))

				if done >= dl.size && dl.size > 0 {
					logSuccess("Resumed download completed (%s)", Size4Human(dl.size))
					return
				}
			case <-dl.doneCh:
				done := int64(0)
				for _, v := range dl.progress {
					done += atomic.LoadInt64(&v)
				}
				if done > dl.size {
					done = dl.size
				}
				pct := float64(done) * 100 / float64(dl.size)
				if dl.size <= 0 {
					pct = 100
				}
				fmt.Print(clearScreen)
				fmt.Printf("%s╔════════════════════════════════════════════════════════════════════════════╗%s\n", colors["cyan"], colors["reset"])
				fmt.Printf("%s║                           RESUMING DOWNLOAD                                ║%s\n", colors["bold"], colors["reset"])
				fmt.Printf("%s╚════════════════════════════════════════════════════════════════════════════╝%s\n", colors["cyan"], colors["reset"])
				fmt.Printf("%s File:%s %s\n", colors["blue"], colors["reset"], fileName)
				fmt.Println(strings.Repeat("─", 70))
				fmt.Printf(" %s\n", progressBarBeautiful(100, barLen))
				fmt.Printf(" %6.2f%% │ %s/%s\n",
					pct,
					Size4Human(dl.size),
					Size4Human(dl.size),
				)
				fmt.Println(strings.Repeat("─", 70))
				logSuccess("Resumed download completed (%s)", Size4Human(dl.size))
				return
			}
		}
	}()

	wg.Wait()
	close(dl.doneCh)
	os.Remove(file)
	time.Sleep(1 * time.Second)
}

func Size4Human(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%dB", b)
	}
	exp := int(math.Log(float64(b)) / math.Log(1024))
	val := float64(b) / math.Pow(1024, float64(exp))
	units := []string{"B", "KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.2f%s", val, units[exp])
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func getFileName(url string, resp *http.Response) string {
	disp := resp.Header.Get("Content-Disposition")
	if strings.Contains(disp, "filename=") {
		start := strings.Index(disp, "filename=") + 9
		end := start
		for end < len(disp) && disp[end] != '"' && disp[end] != ';' {
			end++
		}
		name := strings.TrimSpace(disp[start:end])
		if name != "" {
			return name
		}
	}
	name := filepath.Base(strings.SplitN(url, "?", 2)[0])
	if name == "" || name == "/" || name == "." {
		return fmt.Sprintf("file_%d", time.Now().Unix())
	}
	return name
}

func SetColor(c, t string) string {
	code := colors[c]
	return fmt.Sprintf("%s%s%s", code, t, colors["reset"])
}

func die(a ...interface{}) {
	fmt.Fprintln(os.Stderr, "ERROR:", fmt.Sprint(a...))
	os.Exit(1)
}

func main() {
	flag.Usage = showUsage
	flag.Parse()

	logger.SetVerbose(verbose)
	if scrapeURL != "" {
		global := NewGlobalStatus()
		scrapeAndDownload(scrapeURL, global)
		return
	}
	var args []string
	if fileList != "" {
		data, err := os.ReadFile(fileList)
		if err != nil {
			die("Cannot read file list:", err)
		}
		lines := strings.Split(string(data), "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				args = append(args, line)
			}
		}
		if len(args) == 0 {
			logError("No valid URLs found in file list")
			return
		}
	} else {
		args = flag.Args()
	}

	if len(args) == 1 && strings.HasSuffix(args[0], ".json") {
		sessionFile = args[0]
		resumeFromSession(sessionFile, nil)
		return
	}

	if len(args) == 0 {
		showUsage()
		return
	}

	client := createHTTPClient()
	global := NewGlobalStatus()

	logInfo("Starting download manager with %d threads", numThreads)
	logInfo("Max parallel downloads: %d", maxParallel)

	fmt.Printf("%s╔════════════════════════════════════════════════════════════════════════════╗%s\n", colors["cyan"], colors["reset"])
	fmt.Printf("%s║                      FETCHING FILE METADATA                                ║%s\n", colors["bold"], colors["reset"])
	fmt.Printf("%s╚════════════════════════════════════════════════════════════════════════════╝%s\n", colors["cyan"], colors["reset"])

	for _, u := range args {
		if strings.HasPrefix(u, "ftp://") || strings.HasPrefix(u, "ftps://") || protocol == "ftp" || protocol == "ftps" {
			parsedURL, _ := url.Parse(u)
			name := filepath.Base(parsedURL.Path)
			if name == "" || name == "/" {
				name = fmt.Sprintf("ftp_file_%d", time.Now().Unix())
			}
			global.addFile(name, -1)
			fmt.Printf("  %s•%s %s %s(FTP)%s\n", colors["green"], colors["reset"], name, colors["yellow"], colors["reset"])
		} else {
			name, size, err := fetchFileInfo(u, client)
			if err != nil {
				logWarning("Skipping %s: %v", u, err)
				continue
			}
			global.addFile(name, size)
			fmt.Printf("  %s•%s %s (%s)\n", colors["green"], colors["reset"], name, Size4Human(size))
		}
	}

	if global.totalCount == 0 {
		logError("No valid files to download")
		return
	}

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	for _, u := range args {
		wg.Add(1)
		sem <- struct{}{}

		go func(url string) {
			defer wg.Done()
			defer func() { <-sem }()

			var httpClient *http.Client
			if strings.HasPrefix(url, "ftp://") || strings.HasPrefix(url, "ftps://") || protocol == "ftp" || protocol == "ftps" {
				httpClient = nil
			} else {
				httpClient = createHTTPClient()
			}
			downloadSingle(url, httpClient, global)
		}(u)
	}

	go global.reportAllFiles()
	wg.Wait()
	close(global.doneCh)

	time.Sleep(1 * time.Second)
}


func scrapeAndDownload(targetURL string, global *GlobalStatus) {
	logInfo("Starting scrape of: %s", targetURL)
	
	if extensionsFilter != "" {
		logInfo("Filtering extensions: %s", extensionsFilter)
	}
	
	client := createHTTPClient()
	
	req, err := http.NewRequest("GET", targetURL, nil)
	if err != nil {
		logError("Failed to create request: %v", err)
		return
	}
	
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	
	resp, err := client.Do(req)
	if err != nil {
		logError("Failed to fetch page: %v", err)
		return
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		logError("HTTP error: %d", resp.StatusCode)
		return
	}
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logError("Failed to read response: %v", err)
		return
	}
	
	links := extractLinks(string(body), targetURL)
	
	if len(links) == 0 {
		logError("No links found on page")
		return
	}
	
	filteredLinks := filterLinksByContent(links, client)
	
	fmt.Printf("\n%s╔════════════════════════════════════════════════════════════════════════════╗%s\n", colors["cyan"], colors["reset"])
	fmt.Printf("%s║                           EXTRACTED LINKS                                  ║%s\n", colors["bold"], colors["reset"])
	fmt.Printf("%s╚════════════════════════════════════════════════════════════════════════════╝%s\n", colors["cyan"], colors["reset"])
	
	if extensionsFilter != "" {
		fmt.Printf("%s🔍 Filter: %s%s%s\n", colors["yellow"], colors["bold"], extensionsFilter, colors["reset"])
	}
	
	for i, link := range filteredLinks {
		displayURL := link
		if len(displayURL) > 60 {
			displayURL = displayURL[:57] + "..."
		}
		
		ext := filepath.Ext(link)
		extDisplay := ""
		if ext != "" {
			extDisplay = fmt.Sprintf("%s[%s]%s ", colors["green"], ext, colors["reset"])
		}
		
		var sizeStr string
		if !strings.HasPrefix(link, "ftp://") && !strings.HasPrefix(link, "ftps://") {
			if _, size, err := fetchFileInfo(link, client); err == nil && size > 0 {
				sizeStr = fmt.Sprintf(" %s(%s)%s", colors["yellow"], Size4Human(size), colors["reset"])
			}
		}
		
		fmt.Printf("%s%4d.%s %s%s%s%s%s\n", 
			colors["bold"], i+1, colors["reset"],
			extDisplay,
			colors["cyan"], displayURL, colors["reset"],
			sizeStr)
	}
	
	fmt.Printf("\n%sTotal links found: %d%s\n", colors["green"], len(filteredLinks), colors["reset"])
	
	if len(filteredLinks) == 0 {
		logWarning("No matching links found with filter: %s", extensionsFilter)
		return
	}
	
	selectedIndices := getUserSelection(len(filteredLinks))
	
	if len(selectedIndices) == 0 {
		logWarning("No links selected for download")
		return
	}
	
	selectedLinks := make([]string, 0)
	for _, idx := range selectedIndices {
		if idx >= 1 && idx <= len(filteredLinks) {
			selectedLinks = append(selectedLinks, filteredLinks[idx-1])
		}
	}
	
	logSuccess("Selected %d links for download", len(selectedLinks))
	
	if verbose {
		fmt.Printf("\n%sSelected files:%s\n", colors["bold"], colors["reset"])
		for i, link := range selectedLinks {
			fileName := filepath.Base(link)
			fmt.Printf("  %d. %s\n", i+1, fileName)
		}
	}
	
	startDownloads(selectedLinks)
}

func startDownloads(links []string) {
	if len(links) == 0 {
		return
	}

	logInfo("Starting download of %d selected links", len(links))

	global := NewGlobalStatus()
	
	for _, link := range links {
		fileName := filepath.Base(strings.SplitN(link, "?", 2)[0])
		if fileName == "" || fileName == "/" || fileName == "." {
			fileName = fmt.Sprintf("file_%d", time.Now().Unix())
		}
		
		var size int64 = -1
		if !strings.HasPrefix(link, "ftp://") && !strings.HasPrefix(link, "ftps://") {
			client := createHTTPClient()
			if _, s, err := fetchFileInfo(link, client); err == nil && s > 0 {
				size = s
			}
		}
		
		global.addFile(fileName, size)
	}

	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup

	go global.reportAllFiles()

	for idx, link := range links {
		wg.Add(1)
		sem <- struct{}{}

		go func(downloadURL string, fileIdx int) {
			defer wg.Done()
			defer func() { <-sem }()

			if strings.HasPrefix(downloadURL, "ftp://") || strings.HasPrefix(downloadURL, "ftps://") || protocol == "ftp" || protocol == "ftps" {
				downloadFTP(downloadURL, global)
			} else {
				httpClient := createHTTPClient()
				downloadSingle(downloadURL, httpClient, global)
			}
		}(link, idx)
	}

	wg.Wait()
	
	time.Sleep(2 * time.Second)
	close(global.doneCh)
	logSuccess("All selected downloads completed")
}

func filterLinksByContent(links []string, client *http.Client) []string {
	if extensionsFilter == "" {
		return links
	}
	
	filtered := make([]string, 0)
	extensions := strings.Split(extensionsFilter, ",")
	
	for i, ext := range extensions {
		extensions[i] = strings.TrimSpace(ext)
		if !strings.HasPrefix(extensions[i], ".") {
			extensions[i] = "." + extensions[i]
		}
	}
	
	for _, link := range links {
		linkLower := strings.ToLower(link)
		for _, ext := range extensions {
			if strings.HasSuffix(linkLower, ext) {
				filtered = append(filtered, link)
				break
			}
		}
	}
	
	return filtered
}

func extractLinks(html, baseURL string) []string {
    links := make([]string, 0)
    seen := make(map[string]bool)
    
    patterns := []string{
        `href="([^"]+)"`,
        `href='([^']+)'`,
        `src="([^"]+)"`,
        `src='([^']+)'`,
        `data-url="([^"]+)"`,
        `data-url='([^']+)'`,
        `data-file="([^"]+)"`,
        `data-file='([^']+)'`,
    }
    
    for _, pattern := range patterns {
        re := regexp.MustCompile(pattern)
        matches := re.FindAllStringSubmatch(html, -1)
        
        for _, match := range matches {
            if len(match) > 1 {
                link := match[1]
                
                if strings.HasPrefix(link, "#") || 
                   strings.HasPrefix(link, "javascript:") ||
                   strings.HasPrefix(link, "mailto:") ||
                   link == "" {
                    continue
                }
                
                absoluteLink := toAbsoluteURL(link, baseURL)
                
                if isDownloadableFile(absoluteLink) && !seen[absoluteLink] {
                    seen[absoluteLink] = true
                    links = append(links, absoluteLink)
                }
            }
        }
    }
    
    return links
}

func toAbsoluteURL(href, baseURL string) string {
    if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") || 
       strings.HasPrefix(href, "ftp://") || strings.HasPrefix(href, "ftps://") {
        return href
    }
    
    if strings.HasPrefix(href, "//") {
        base, err := url.Parse(baseURL)
        if err == nil {
            return base.Scheme + ":" + href
        }
        return href
    }

    if strings.HasPrefix(href, "/") {
        base, err := url.Parse(baseURL)
        if err == nil {
            return base.Scheme + "://" + base.Host + href
        }
        return href
    }
    
    base, err := url.Parse(baseURL)
    if err != nil {
        return href
    }

    if !strings.HasSuffix(base.Path, "/") {
        base.Path = base.Path + "/"
    }
    
    relative, err := url.Parse(href)
    if err != nil {
        return href
    }
    
    return base.ResolveReference(relative).String()
}
func isDownloadableFile(url string) bool {

    if extensionsFilter != "" {
        return hasAllowedExtension(url)
    }
    
    downloadableExtensions := []string{
        ".zip", ".rar", ".7z", ".tar", ".gz", ".bz2",
        ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
        ".jpg", ".jpeg", ".png", ".gif", ".bmp", ".svg", ".webp",
        ".mp4", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".webm",
        ".mp3", ".wav", ".flac", ".aac", ".ogg", ".m4a",
        ".exe", ".msi", ".deb", ".rpm", ".apk",
        ".iso", ".img", ".bin",
        ".txt", ".csv", ".json", ".xml", ".log",
        ".psd", ".ai", ".eps", ".cdr",
        ".ttf", ".otf", ".woff", ".woff2",
    }
    
    urlLower := strings.ToLower(url)
    
    for _, ext := range downloadableExtensions {
        if strings.HasSuffix(urlLower, ext) {
            return true
        }
    }
    
    if strings.Contains(urlLower, "/download") || 
       strings.Contains(urlLower, "/file") ||
       strings.Contains(urlLower, "/get") {
        return true
    }
    
    return false
}

func hasAllowedExtension(url string) bool {
    if extensionsFilter == "" {
        return true
    }
    
    extensions := strings.Split(extensionsFilter, ",")
    urlLower := strings.ToLower(url)
    
    for _, ext := range extensions {
        ext = strings.TrimSpace(ext)
        if !strings.HasPrefix(ext, ".") {
            ext = "." + ext
        }
        
        if strings.HasSuffix(urlLower, ext) {
            return true
        }
    }
    
    return false
}

func getUserSelection(maxCount int) []int {
	reader := bufio.NewReader(os.Stdin)
	
	for {
		fmt.Printf("\n%s┌─────────────────────────────────────────────────────────────────┐%s\n", colors["cyan"], colors["reset"])
		fmt.Printf("%s│                    SELECT LINKS TO DOWNLOAD                     │%s\n", colors["bold"], colors["reset"])
		fmt.Printf("%s└─────────────────────────────────────────────────────────────────┘%s\n", colors["cyan"], colors["reset"])
		fmt.Printf("%s\nEnter selection %s(1-%d)%s: %s", 
			colors["yellow"], colors["reset"], maxCount, colors["reset"], colors["bold"])
		
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)
		
		if input == "" {
			fmt.Printf("%sNo selection made.%s\n", colors["yellow"], colors["reset"])
			return []int{}
		}
		
		selected := parseSelection(input, maxCount)
		
		if len(selected) > 0 {
			fmt.Printf("\n%sSelected indices: %v%s\n", colors["green"], selected, colors["reset"])
			return selected
		}
		
		fmt.Printf("%sInvalid selection format!%s\n", colors["red"], colors["reset"])
		fmt.Printf("Supported formats:\n")
		fmt.Printf("  • Range: 1-4,7,9\n")
		fmt.Printf("  • List:  1,2,3,4\n")
		fmt.Printf("  • Mixed: 1-4,7,9-11\n")
		fmt.Printf("  • Space: 1 2 3 4\n")
	}
}

func parseSelection(input string, maxCount int) []int {
	selected := make(map[int]bool)
	
	input = strings.ReplaceAll(input, " ", ",")
	
	parts := strings.Split(input, ",")
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		
		if strings.Contains(part, "-") {
			rangeParts := strings.Split(part, "-")
			if len(rangeParts) == 2 {
				start, err1 := strconv.Atoi(strings.TrimSpace(rangeParts[0]))
				end, err2 := strconv.Atoi(strings.TrimSpace(rangeParts[1]))
				
				if err1 == nil && err2 == nil && start <= end {
					for i := start; i <= end && i <= maxCount; i++ {
						if i >= 1 {
							selected[i] = true
						}
					}
				}
			}
		} else {

			if num, err := strconv.Atoi(part); err == nil {
				if num >= 1 && num <= maxCount {
					selected[num] = true
				}
			}
		}
	}

	result := make([]int, 0, len(selected))
	for i := 1; i <= maxCount; i++ {
		if selected[i] {
			result = append(result, i)
		}
	}
	
	return result
}
