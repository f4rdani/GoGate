package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"
)

// TunnelManager handles the lifecycle of the cloudflared process.
type TunnelManager struct {
	mu         sync.Mutex
	cmd        *exec.Cmd
	tunnelURL  string
	isRunning  bool
	cancelFunc context.CancelFunc
}

// NewTunnelManager creates a new TunnelManager.
func NewTunnelManager() *TunnelManager {
	return &TunnelManager{}
}

// GetURL returns the active tunnel URL.
func (m *TunnelManager) GetURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tunnelURL
}

// IsRunning returns true if the tunnel is running.
func (m *TunnelManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRunning
}

// DownloadCloudflared downloads the appropriate cloudflared binary if it doesn't exist.
func (m *TunnelManager) DownloadCloudflared(ctx context.Context) (string, error) {
	binaryName := "cloudflared"
	downloadURL := "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64"

	if runtime.GOOS == "windows" {
		binaryName = "cloudflared.exe"
		downloadURL = "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-windows-amd64.exe"
	}

	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("get executable path: %w", err)
	}
	binDir := filepath.Dir(execPath)
	localPath := filepath.Join(binDir, binaryName)

	// Check if already exists
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	slog.Info("cloudflared not found locally, downloading...", "url", downloadURL, "path", localPath)

	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return "", fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed: status %d", resp.StatusCode)
	}

	// Write to temp file first to prevent partial downloads
	tmpPath := localPath + ".tmp"
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		out.Close()
		os.Remove(tmpPath)
	}()

	if _, err = io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("write download data: %w", err)
	}
	out.Close()

	if err = os.Rename(tmpPath, localPath); err != nil {
		return "", fmt.Errorf("finalize download: %w", err)
	}

	slog.Info("cloudflared download complete", "path", localPath)
	return localPath, nil
}

// Start spawns the cloudflared process and returns the generated trycloudflare.com URL.
func (m *TunnelManager) Start(ctx context.Context, port int) (string, error) {
	m.mu.Lock()
	if m.isRunning {
		url := m.tunnelURL
		m.mu.Unlock()
		return url, nil
	}
	m.mu.Unlock()

	binaryPath, err := m.DownloadCloudflared(ctx)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Ensure clean slate
	m.tunnelURL = ""
	tunnelCtx, cancel := context.WithCancel(context.Background())
	m.cancelFunc = cancel

	cmd := exec.CommandContext(tunnelCtx, binaryPath, "tunnel", "--protocol", "http2", "--url", fmt.Sprintf("http://127.0.0.1:%d", port))
	prepareCmd(cmd)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return "", fmt.Errorf("start process: %w", err)
	}

	m.cmd = cmd
	m.isRunning = true

	// Parse logs asynchronously to find the URL
	urlChan := make(chan string, 1)
	errChan := make(chan error, 1)
	urlRegex := regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

	go func() {
		reader := bufio.NewReader(stderr)
		urlFound := false

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if !urlFound {
					errChan <- fmt.Errorf("process exited before generating URL: %w", err)
				}
				return
			}

			// Parse URL
			if !urlFound {
				if match := urlRegex.FindString(line); match != "" {
					urlFound = true
					urlChan <- match
				}
			}
		}
	}()

	// Wait for URL or error/timeout
	select {
	case url := <-urlChan:
		m.tunnelURL = url
		slog.Info("Cloudflare Quick Tunnel established successfully", "url", url)
		SetGlobalStatus(true, url)
		
		// Monitor process exit in background
		go func() {
			err := cmd.Wait()
			m.mu.Lock()
			m.isRunning = false
			m.tunnelURL = ""
			m.mu.Unlock()
			SetGlobalStatus(false, "")
			cancel()
			if err != nil {
				slog.Info("cloudflared tunnel process stopped", "error", err.Error())
			} else {
				slog.Info("cloudflared tunnel process stopped cleanly")
			}
		}()
		
		return url, nil

	case err := <-errChan:
		m.isRunning = false
		SetGlobalStatus(false, "")
		cancel()
		return "", err

	case <-time.After(30 * time.Second):
		m.isRunning = false
		SetGlobalStatus(false, "")
		cancel()
		return "", fmt.Errorf("timeout waiting for tunnel URL generation")
	}
}

// Stop terminates the running cloudflared process.
func (m *TunnelManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.isRunning {
		return
	}

	slog.Info("stopping cloudflared quick tunnel...")
	if m.cancelFunc != nil {
		m.cancelFunc()
	}

	if m.cmd != nil {
		killProcessGroup(m.cmd)
	}

	m.isRunning = false
	m.tunnelURL = ""
	SetGlobalStatus(false, "")
}

// Global tunnel status tracking
var (
	globalURL       string
	globalIsRunning bool
	globalMu        sync.RWMutex
)

// SetGlobalStatus updates the package-level global tunnel status.
func SetGlobalStatus(running bool, url string) {
	globalMu.Lock()
	defer globalMu.Unlock()
	globalIsRunning = running
	globalURL = url
}

// GetGlobalStatus retrieves the package-level global tunnel status.
func GetGlobalStatus() (bool, string) {
	globalMu.RLock()
	defer globalMu.RUnlock()
	return globalIsRunning, globalURL
}
