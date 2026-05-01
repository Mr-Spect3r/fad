# 🚀 FAD - Fast Advanced Downloader

[![Go Version](https://img.shields.io/badge/Go-1.20+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux%20%7C%20macOS-lightgrey)]()

A powerful, multi-threaded downloader with support for **HTTP/HTTPS**, **FTP/FTPS**, **proxy connections**, and **smart resume capabilities**. Built for speed and reliability.

## ✨ Features

- 🧵 **Multi-threaded downloads** - Maximize bandwidth utilization
- 📡 **Multiple protocols** - HTTP, HTTPS, FTP, FTPS
- 🔄 **Resume support** - Interrupt and resume downloads seamlessly
- 🕸️ **Proxy support** - SOCKS4, SOCKS5, and HTTP proxies
- 📁 **Batch downloading** - Download from file lists
- 🕷️ **Web scraping** - Extract and download links from web pages
- 🔍 **Extension filtering** - Filter downloads by file extensions
- ⚡ **Adaptive buffering** - Automatically optimizes buffer sizes
- 💾 **Session saving** - Save progress and resume later
- 🎨 **Beautiful progress bars** - Real-time visual feedback
- 🌍 **Cross-platform** - Windows, Linux, macOS

## 📸 Screenshots

| Feature | Preview |
|---------|---------|
| **Normal Download** | ![Normal](https://github.com/user-attachments/assets/e1cd29f0-2d04-4af2-9e71-0d1492fc3aba) |
| **Web Scraping** | ![Scraping](https://github.com/user-attachments/assets/48a0409b-8bea-48bf-93ee-fa4e2f66f32d) |
| **Capabilities** | ![Capabilities](https://github.com/user-attachments/assets/826d5701-1f31-4992-97c0-89545386b083) |
| **Progress Bars** | ![Progress](https://github.com/user-attachments/assets/3c39c0be-3e9e-4004-8d75-58962bd90158) |

## 📦 Installation

### Direct installation

```
go install github.com/Mr-Spect3r/fad@latest
```

### From Source

```bash
git clone https://github.com/Mr-Spect3r/fad.git
cd fad
go build -o fad main.go
```

## 🚀 Quick Start

### Basic Usage

```bash
# Download a single file
./fad https://example.com/file.zip

# Download with 8 threads
./fad -t 8 https://example.com/large-file.zip

# Download multiple files
./fad https://example.com/file1.zip https://example.com/file2.zip

# Download from file list
./fad -f urls.txt
```

### Advanced Examples

#### 🔐 Proxy Downloads

```bash
# SOCKS5 proxy
./fad -proxy socks5://127.0.0.1:1080 https://example.com/file.zip

# SOCKS4 proxy with custom threads
./fad -proxy socks4://192.168.1.1:9050 -t 16 https://example.com/file.zip

# HTTP proxy
./fad -proxy http://proxy.company.com:8080 https://example.com/file.zip
```

### 📡 FTP Downloads

```bash
# Standard FTP
./fad -protocol ftp ftp://example.com/file.zip

# FTP with multi-part (faster)
./fad -protocol ftp -ftp-multipart -ftp-parts 8 ftp://example.com/large-file.zip

# FTPS (FTP over TLS)
./fad -protocol ftps ftps://example.com/secure-file.zip

# Custom FTP credentials
./fad -protocol ftp -ftp-user myuser -ftp-pass mypass ftp://example.com/file.zip
```

### 🕷️ Web Scraping

```bash
# Extract and download all files from a page
./fad -scrape https://example.com/downloads/

# Filter by extensions
./fad -scrape https://example.com/downloads/ -ex .mp4,.mp3,.zip

# Scrape with custom threads
./fad -scrape https://example.com/files/ -t 16 -ex .pdf,.doc
```

### 🔄 Resume Downloads

```bash
# Resume from saved session
./fad session_20231215_143022.json

# Session auto-saves on interrupt (Ctrl+C)
```

## ⚙️ Command Line Options

### Core Options

| Option | Default | Description |
|--------|---------|-------------|
| `-t` | CPU cores | Number of parallel download threads per file |
| `-o` | `.` | Destination directory for downloads |
| `-u` | `2` | Maximum simultaneous file downloads |
| `-r` | `5` | Retries per segment |
| `-timeout` | `30` | Network timeout in seconds |

### Network Options

| Option | Description |
|--------|-------------|
| `-proxy` | Proxy address (socks4://, socks5://, http://) |
| `-protocol` | Force protocol: auto, http, https, ftp, ftps |
| `-H` | Custom HTTP header (can be repeated) |
| `-c` | Cookie header value |

### FTP Options

| Option | Default | Description |
|--------|---------|-------------|
| `-ftp-user` | `anonymous` | FTP username |
| `-ftp-pass` | `anonymous@example.com` | FTP password |
| `-ftp-multipart` | `true` | Enable multi-part FTP download |
| `-ftp-parts` | `0` | Number of FTP parts (0 = auto) |

### Scraping Options

| Option | Description |
|--------|-------------|
| `-scrape` | URL to scrape for downloadable links |
| `-ex` | Filter extensions (e.g., `.mp4,.mp3,.zip`) |

### Other Options

| Option | Description |
|--------|-------------|
| `-v` | Verbose mode with per-thread progress |
| `-save-session` | Save session to JSON if interrupted |
| `-f` | File containing download URLs (one per line) |

## 🛠️ Building from Source

### Prerequisites

- Go 1.20 or higher
- GCC (for Windows builds)

### Build Commands

```bash
# Linux/macOS
go build -o fad main.go

# Windows
GOOS=windows GOARCH=amd64 go build -o fad.exe main.go

# With optimizations
go build -ldflags="-s -w" -o fad main.go
```

## 📝 File Format Examples

### URLs File (urls.txt)

```text
# This is a comment
https://example.com/file1.zip
https://example.com/file2.zip
ftp://ftp.example.com/large-file.iso
https://example.com/document.pdf
```

### Session Files

#### Session files are auto-saved as {filename}.json and {filename}.progress. To resume:

```bash
./fad file.zip.json
```

## 🎨 Output Preview

```text
══════════════════════════════════════════════════════════════════
                      DOWNLOAD STATUS
══════════════════════════════════════════════════════════════════
⬇️ 1. large-file.zip ████████████████████░░░░░░░░░░░░ 65.2%  1.2GB/1.8GB  ⚡ 12.5 MB/s  ETA: 45s
⬇️ 2. document.pdf   ████████████████████████████░░░░ 82.1%  8.2MB/10.0MB  ⚡ 2.3 MB/s  ETA: 2s
⏳ 3. image.jpg      ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░ 0.0%  0B/5.2MB       ⏳ Waiting
──────────────────────────────────────────────────────────────────
Avg Speed: 14.8 MB/s  Instant: 12.5 MB/s  Active: 2
Files: 1/3  Downloaded: 1.2GB / 1.8GB (65.2%)  Elapsed: 45.2s
Remaining: 45s  Left: 0.6GB
```

## ⚠️ Error Handling

FAD handles various error scenarios gracefully:

| Scenario | Behavior |
|----------|----------|
| ✅ Network interruptions | Auto-resume |
| ✅ Server timeouts | Retry with backoff |
| ✅ Partial downloads | Resume from last byte |
| ✅ Invalid URLs | Skip and continue |
| ✅ Proxy failures | Fallback to direct (configurable) |

---

## 🔧 Troubleshooting

### Common Issues

| Question | Solution |
|----------|----------|
| **Q: Slow download speeds?** | • Increase thread count: `-t 16`<br>• Check if server supports range requests<br>• Try FTP multi-part for FTP files |
| **Q: Proxy not working?** | • Verify proxy format: `socks5://host:port`<br>• Ensure proxy is reachable<br>• Try without authentication first |
| **Q: Resume not working?** | • Ensure server supports `Accept-Ranges: bytes`<br>• Check if session files exist in download directory<br>• Manual resume: `./fad session_file.json` |
