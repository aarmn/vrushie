package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	qrcode "github.com/skip2/go-qrcode"
)

const version = "1.1.0"

// --- Configuration ---
var (
	port            = flag.Int("port", 0, "Port to listen on (0 for random available port)")
	limitN          = flag.Int("n", 1, "Number of downloads allowed (1 = serve once, >1 = serve to N unique IPs)")
	allowedIPs      = flag.String("ips", "", "Comma-separated list of specific IPs allowed to connect")
	showHelp        = flag.Bool("h", false, "Show help message")
	showHelpLong    = flag.Bool("help", false, "Show help message")
	showVersion     = flag.Bool("v", false, "Show version information")
	showVersionLong = flag.Bool("version", false, "Show version information")
	zipMode         = flag.Bool("z", false, "Serve multiple files or a directory as a single archive")
	archiveFormat   = flag.String("format", "zip", "Archive format to use (zip or tar.gz)")
)

// --- TUI Styles ---
var (
	// Using more pastel/calm colors for "cuteness"
	stylePrimary      = lipgloss.NewStyle().Foreground(lipgloss.Color("69"))                                              // Mauve
	styleSecondary    = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))                                             // Gold
	styleSuccess      = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))                                              // Light Green
	styleError        = lipgloss.NewStyle().Foreground(lipgloss.Color("204"))                                             // Salmon
	styleSubtle       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))                                             // Gray
	styleBorder       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder(), true).BorderForeground(lipgloss.Color("63")) // Pink Border
	styleSpinner      = stylePrimary
	styleURL          = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Underline(true) // Blue Underline
	styleIPAllowed    = styleSuccess
	styleIPRejected   = styleError
	styleInstructions = styleSubtle.PaddingTop(1)
)

// --- TUI Model ---

type model struct {
	spinner        spinner.Model
	server         *http.Server
	listener       net.Listener
	shutdownChan   chan struct{}    // Channel to signal graceful shutdown
	errChan        chan error       // Channel for server errors
	activityChan   chan activityLog // Channel for logging access attempts/downloads
	serverReady    bool
	servingURL     string
	publicIP       string
	qrCode         string
	paths          []string
	fileName       string
	fileSize       int64
	accessMode     string
	limitN         int                 // 0 means serve-once logic, >0 means N downloads/IPs
	specificIPs    map[string]struct{} // Set of specifically allowed IPs
	allowedFirstN  map[string]struct{} // Set of the first N IPs that connected (if limitN > 0 and specificIPs is empty)
	ipLock         sync.Mutex          // Protects access maps and download count
	downloadCount  int
	activity       []activityLog // Log of recent activities
	maxActivityLog int           // Max number of log entries to keep
	lastError      error
	quitting       bool
	width          int
	height         int
}

type activityLog struct {
	Timestamp time.Time
	IP        string
	Action    string // e.g., "Connected", "Rejected", "Downloaded", "Error"
	Style     lipgloss.Style
}

// --- TUI Messages ---

type serverReadyMsg struct {
	url      string
	publicIP string
}
type serverErrMsg struct{ err error }
type activityMsg struct{ log activityLog }
type shutdownMsg struct{} // Message to initiate shutdown

// --- Bubbletea Implementation ---

func initialModel(paths []string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = styleSpinner

	m := model{
		spinner:        s,
		shutdownChan:   make(chan struct{}),
		errChan:        make(chan error, 1),
		activityChan:   make(chan activityLog, 10),
		serverReady:    false,
		paths:          paths,
		limitN:         *limitN,
		specificIPs:    make(map[string]struct{}),
		allowedFirstN:  make(map[string]struct{}),
		maxActivityLog: 10,
		activity:       make([]activityLog, 0, 10),
	}

	if *zipMode {
		m.fileName = "archive." + strings.ToLower(*archiveFormat)
		m.fileSize = -1 // Use -1 to indicate size is unknown/dynamic
	} else {
		m.fileName = filepath.Base(paths[0])
		info, err := os.Stat(paths[0])
		if err == nil {
			m.fileSize = info.Size()
		}
	}

	// Parse specific IPs if provided
	if *allowedIPs != "" {
		ips := strings.Split(*allowedIPs, ",")
		for _, ip := range ips {
			trimmedIP := strings.TrimSpace(ip)
			if trimmedIP != "" {
				m.specificIPs[trimmedIP] = struct{}{}
			}
		}
		m.accessMode = fmt.Sprintf("Locked to %d specific IP(s)", len(m.specificIPs))
	} else if m.limitN == 1 {
		m.accessMode = "Serve once to first successful download"
	} else if m.limitN > 1 {
		m.accessMode = fmt.Sprintf("Serve to first %d unique IPs", m.limitN)
	} else { // Should not happen with flag default, but handle defensively
		m.accessMode = "Serve once (fallback)"
		m.limitN = 1
	}

	return m
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.startServer())
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			// Send shutdown signal if server is running
			if m.server != nil {
				close(m.shutdownChan) // Signal server goroutine
			}
			return m, tea.Quit // Signal Bubble Tea to quit
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Recalculate border width maybe?
		styleBorder.Width(m.width - 4) // Adjust for padding

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case serverReadyMsg:
		m.serverReady = true
		m.servingURL = msg.url
		m.publicIP = msg.publicIP

		// Generate QR code if we have a public IP
		if m.publicIP != "" {
			port := m.listener.Addr().(*net.TCPAddr).Port
			publicURL := fmt.Sprintf("http://%s:%d/", m.publicIP, port)
			qr, err := generateQRCode(publicURL)
			if err != nil {
				log.Printf("QR code generation failed: %v", err)
				// Don't crash, just won't show QR
			} else {
				m.qrCode = qr
			}
		}
		return m, nil

	case serverErrMsg:
		m.lastError = msg.err
		m.quitting = true // Assume fatal error
		return m, tea.Quit

	case activityMsg:
		m.activity = append(m.activity, msg.log)
		// Keep the log trimmed
		if len(m.activity) > m.maxActivityLog {
			m.activity = m.activity[len(m.activity)-m.maxActivityLog:]
		}

		// Check if a successful download triggers shutdown
		if msg.log.Action == "Download Complete" {
			m.ipLock.Lock()
			m.downloadCount++
			shouldShutdown := (m.limitN == 1 || (m.limitN > 1 && m.downloadCount >= m.limitN))
			m.ipLock.Unlock()

			if shouldShutdown {
				// Send internal message to trigger graceful shutdown sequence
				return m, func() tea.Msg { return shutdownMsg{} }
			}
		}
		return m, nil

	case shutdownMsg: // Received internally when download limit reached
		m.quitting = true
		m.activity = append(m.activity, activityLog{
			Timestamp: time.Now(),
			IP:        "Server",
			Action:    "Download limit reached. Shutting down...",
			Style:     styleSubtle,
		})
		// Signal the server goroutine via the channel
		close(m.shutdownChan)
		// Don't quit Bubble Tea immediately, let the server goroutine finish shutdown
		return m, m.waitForShutdown() // Command to wait for error channel

	case error: // This likely comes from waitForShutdown
		m.lastError = msg
		return m, tea.Quit // Now quit Bubble Tea
	}

	return m, nil
}

func (m *model) View() string {
	if m.quitting && m.lastError == nil {
		return styleSuccess.Render("\nServer shut down gracefully. Bye! ♡\n\n")
	}
	if m.lastError != nil {
		return styleError.Render(fmt.Sprintf("\nServer Error: %v\n\n", m.lastError))
	}

	// --- Left Panel (Main Info) ---
	var leftPanel strings.Builder
	leftPanel.WriteString(stylePrimary.Render("🌸 Vrushie Server 🌸"))
	leftPanel.WriteString("\n\n")
	if *zipMode {
		leftPanel.WriteString(fmt.Sprintf("Serving Archive: %s\n", styleSecondary.Render(m.fileName)))
		leftPanel.WriteString(fmt.Sprintf("Contents: %s\n", styleSubtle.Render(strings.Join(m.paths, ", "))))
	} else {
		leftPanel.WriteString(fmt.Sprintf("Serving File: %s\n", styleSecondary.Render(m.fileName)))
		if m.fileSize >= 0 {
			leftPanel.WriteString(fmt.Sprintf("Size: %s\n", styleSubtle.Render(formatBytes(m.fileSize))))
		}
	}
	leftPanel.WriteString("\n")

	if !m.serverReady {
		leftPanel.WriteString(fmt.Sprintf("%s Initializing server...", m.spinner.View()))
	} else {
		leftPanel.WriteString(styleSuccess.Render("Server Ready! ✨\n"))
		leftPanel.WriteString("Listening on:\n")
		urls := strings.Split(m.servingURL, "\n")
		for _, url := range urls {
			if url != "" {
				if m.publicIP != "" && strings.Contains(url, m.publicIP) {
					leftPanel.WriteString(fmt.Sprintf("🌐 %s\n", styleURL.Render(url)))
				} else {
					leftPanel.WriteString(fmt.Sprintf("   %s\n", styleURL.Render(url)))
				}
			}
		}
	}
	leftPanel.WriteString("\n")
	leftPanel.WriteString(fmt.Sprintf("Access Mode: %s\n", stylePrimary.Render(m.accessMode)))
	if len(m.specificIPs) > 0 {
		var ips []string
		for ip := range m.specificIPs {
			ips = append(ips, ip)
		}
		leftPanel.WriteString(fmt.Sprintf("Allowed IPs: %s\n", styleSubtle.Render(strings.Join(ips, ", "))))
	} else if m.limitN > 1 {
		m.ipLock.Lock()
		var ips []string
		for ip := range m.allowedFirstN {
			ips = append(ips, ip)
		}
		status := fmt.Sprintf("%d / %d slots filled", len(ips), m.limitN)
		if len(ips) > 0 {
			status += ": " + strings.Join(ips, ", ")
		}
		m.ipLock.Unlock()
		leftPanel.WriteString(fmt.Sprintf("First %d IPs: %s\n", m.limitN, styleSubtle.Render(status)))
	}
	leftPanel.WriteString("\n")
	leftPanel.WriteString("Activity Log:\n")
	if len(m.activity) == 0 {
		leftPanel.WriteString(styleSubtle.Render("  No activity yet...\n"))
	} else {
		for i := len(m.activity) - 1; i >= 0; i-- {
			logEntry := m.activity[i]
			ts := logEntry.Timestamp.Format("15:04:05")
			leftPanel.WriteString(fmt.Sprintf("  %s [%s] %s\n",
				styleSubtle.Render(ts),
				logEntry.Style.Render(logEntry.IP),
				logEntry.Action,
			))
		}
	}
	leftPanelStr := leftPanel.String()

	// --- Right Panel (QR Code) ---
	rightPanelStr := ""
	if m.qrCode != "" {
		qrStyle := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder(), true).
			BorderForeground(stylePrimary.GetForeground()).
			Padding(1)
		rightPanelStr = qrStyle.Render(m.qrCode)
	}

	// --- Combine Panels ---
	mainContent := leftPanelStr
	// Check if there's enough space for a side-by-side layout
	if rightPanelStr != "" && lipgloss.Width(leftPanelStr)+lipgloss.Width(rightPanelStr) < m.width {
		mainContent = lipgloss.JoinHorizontal(lipgloss.Top, leftPanelStr, rightPanelStr)
	}

	// --- Footer ---
	var footer strings.Builder
	if !m.quitting {
		footer.WriteString(styleInstructions.Render("\nPress 'q' or Ctrl+C to shut down manually."))
	} else {
		footer.WriteString(styleInstructions.Render("\nShutting down..."))
	}

	// Apply final border
	return styleBorder.Render(lipgloss.JoinVertical(lipgloss.Left, mainContent, footer.String()))
}

// --- Helper Functions ---

func generateQRCode(url string) (string, error) {
	// Generate a QR code, then convert it to a small ASCII string
	// The `false` parameter creates a denser QR code, better for terminals
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return "", fmt.Errorf("could not generate QR code: %w", err)
	}
	return qr.ToSmallString(false), nil
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func getOutboundIPs() []string {
	var ips []string
	interfaces, err := net.Interfaces()
	if err != nil {
		return []string{"127.0.0.1"} // Fallback
	}
	for _, i := range interfaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			// Process IP address
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			// Prefer IPv4 for easier sharing generally
			if ip.To4() != nil {
				ips = append([]string{ip.String()}, ips...) // Prepend IPv4
			} else {
				ips = append(ips, ip.String()) // Append IPv6
			}
		}
	}
	if len(ips) == 0 {
		return []string{"127.0.0.1"} // Fallback if no non-local found
	}
	return ips
}

// getPublicIP attempts to retrieve the public IP from a list of services.
func getPublicIP() (string, error) {
	services := []string{
		"https://api.ipify.org",
		"https://ifconfig.me/ip",
		"https://icanhazip.com",
		"https://ident.me",
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	for _, service := range services {
		resp, err := client.Get(service)
		if err != nil {
			log.Printf("Failed to get public IP from %s: %v", service, err)
			continue // Try next service
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("Service %s returned non-200 status: %d", service, resp.StatusCode)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body from %s: %v", service, err)
			continue
		}

		ipStr := strings.TrimSpace(string(body))
		if net.ParseIP(ipStr) != nil {
			return ipStr, nil // Success
		}
	}

	return "", fmt.Errorf("all public IP services failed")
}

// --- Server Logic ---

// startServer is a tea.Cmd that starts the HTTP server in a goroutine
func (m *model) startServer() tea.Cmd {
	return func() tea.Msg {
		// Create listener
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			return serverErrMsg{fmt.Errorf("failed to listen: %w", err)}
		}
		m.listener = listener // Store listener for shutdown

		// Get actual port if random was requested
		actualPort := listener.Addr().(*net.TCPAddr).Port

		// Determine server URLs
		publicIP, err := getPublicIP()
		if err != nil {
			log.Printf("Could not retrieve public IP: %v", err) // Log error for debugging
		}

		ips := getOutboundIPs()
		var urlBuilder strings.Builder

		// Prepend public IP if available
		if publicIP != "" {
			urlBuilder.WriteString(fmt.Sprintf("http://%s:%d/\n", publicIP, actualPort))
		}

		for _, ip := range ips {
			// Avoid duplicating the public IP if it's also found as a local IP
			if ip == publicIP {
				continue
			}
			urlBuilder.WriteString(fmt.Sprintf("http://%s:%d/\n", ip, actualPort))
		}
		// Always include localhost
		if !contains(ips, "127.0.0.1") && publicIP != "127.0.0.1" {
			urlBuilder.WriteString(fmt.Sprintf("http://127.0.0.1:%d/\n", actualPort))
		}
		serverURL := strings.TrimSpace(urlBuilder.String())

		// Create server
		mux := http.NewServeMux()
		mux.HandleFunc("/", m.fileHandler) // Pass model method
		m.server = &http.Server{
			Handler: mux,
		}

		// Start server in a goroutine
		go func() {
			<-m.shutdownChan // Wait for shutdown signal
			log.Println("Shutdown signal received, stopping server...")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // Graceful shutdown timeout
			defer cancel()
			if err := m.server.Shutdown(ctx); err != nil {
				m.errChan <- fmt.Errorf("server shutdown failed: %w", err)
			}
			close(m.errChan) // Signal that shutdown goroutine is done
		}()

		// Start listening in another goroutine
		go func() {
			log.Printf("Server starting on port %d...", actualPort)
			if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.errChan <- fmt.Errorf("server failed: %w", err)
			}
		}()

		// Report server ready via message
		return serverReadyMsg{url: serverURL, publicIP: publicIP}
	}
}

// waitForShutdown waits for the server goroutine to finish shutting down
func (m *model) waitForShutdown() tea.Cmd {
	return func() tea.Msg {
		// Wait for an error from the channel OR for it to be closed (success)
		err, ok := <-m.errChan
		if ok && err != nil {
			return err // Return the error to Bubble Tea
		}
		return nil // Return nil on successful shutdown (channel closed)
	}
}

// fileHandler is the HTTP handler function
func (m *model) fileHandler(w http.ResponseWriter, r *http.Request) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr // Fallback
	}

	if !m.isRequestAllowed(ip) {
		reason := "Access denied" // Generic reason, specific reasons are internal
		logMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Rejected: %s", reason), Style: styleIPRejected}
		m.activityChan <- logMsg
		http.Error(w, reason, http.StatusForbidden)
		return
	}

	logMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: "Connected & Allowed", Style: styleIPAllowed}
	m.activityChan <- logMsg

	var copyErr error
	if *zipMode {
		// --- Archive Mode ---
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(m.fileName))
		format := strings.ToLower(*archiveFormat)
		if format == "zip" {
			w.Header().Set("Content-Type", "application/zip")
			copyErr = m.streamZip(w)
		} else if format == "tar.gz" {
			w.Header().Set("Content-Type", "application/x-gzip")
			copyErr = m.streamTarGz(w)
		}
	} else {
		// --- Single File Mode ---
		file, err := os.Open(m.paths[0])
		if err != nil {
			errMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Error opening file: %s", err), Style: styleError}
			m.activityChan <- errMsg
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		defer file.Close()

		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(m.fileName))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(m.fileSize, 10))
		_, copyErr = io.Copy(w, file)
	}

	if copyErr == nil {
		successMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: "Download Complete", Style: styleSuccess}
		m.activityChan <- successMsg
	} else {
		errMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Error during transfer: %s", copyErr), Style: styleError}
		m.activityChan <- errMsg
	}
}

// isRequestAllowed checks if an incoming request from a given IP is allowed based on the access rules.
func (m *model) isRequestAllowed(ip string) bool {
	m.ipLock.Lock()
	defer m.ipLock.Unlock()

	isAllowed := false
	if len(m.specificIPs) > 0 {
		// Mode 1: Specific IPs
		if _, ok := m.specificIPs[ip]; ok {
			isAllowed = true
		}
	} else if m.limitN > 1 {
		// Mode 2: First N unique IPs
		if _, ok := m.allowedFirstN[ip]; ok {
			isAllowed = true
		} else if len(m.allowedFirstN) < m.limitN {
			m.allowedFirstN[ip] = struct{}{}
			isAllowed = true
		}
	} else {
		// Mode 3: Serve once (limitN == 1)
		isAllowed = true
	}

	// Final checks for download counts
	if isAllowed && m.limitN > 1 && m.downloadCount >= m.limitN {
		isAllowed = false
	}
	if isAllowed && m.limitN == 1 && m.downloadCount > 0 {
		isAllowed = false
	}

	return isAllowed
}

// streamZip creates a zip archive on-the-fly and streams it to the ResponseWriter.
func (m *model) streamZip(w http.ResponseWriter) error {
	zipWriter := zip.NewWriter(w)
	defer zipWriter.Close()

	for _, path := range m.paths {
		err := filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil // Skip directories
			}

			// Create a proper zip header
			header, err := zip.FileInfoHeader(info)
			if err != nil {
				return err
			}
			header.Name, err = filepath.Rel(filepath.Dir(path), filePath)
			if err != nil {
				return err
			}
			header.Method = zip.Deflate

			// Create a writer for the file in the zip
			writer, err := zipWriter.CreateHeader(header)
			if err != nil {
				return err
			}

			// Open the original file
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer file.Close()

			// Copy the file content to the zip writer
			_, err = io.Copy(writer, file)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// streamTarGz creates a tar.gz archive on-the-fly and streams it.
func (m *model) streamTarGz(w http.ResponseWriter) error {
	gzipWriter := gzip.NewWriter(w)
	defer gzipWriter.Close()

	tarWriter := tar.NewWriter(gzipWriter)
	defer tarWriter.Close()

	for _, path := range m.paths {
		err := filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Create a proper tar header
			header, err := tar.FileInfoHeader(info, info.Name())
			if err != nil {
				return err
			}
			header.Name, err = filepath.Rel(filepath.Dir(path), filePath)
			if err != nil {
				return err
			}

			// Write the header
			if err := tarWriter.WriteHeader(header); err != nil {
				return err
			}

			// If it's a directory, we're done with this entry
			if info.IsDir() {
				return nil
			}

			// Open the original file
			file, err := os.Open(filePath)
			if err != nil {
				return err
			}
			defer file.Close()

			// Copy the file content to the tar writer
			_, err = io.Copy(tarWriter, file)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// contains checks if a string slice contains a specific string.
func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// --- Helper Functions for CLI ---

func printUsage() {
	fmt.Printf(`🌸 Vrushie Server %s 🌸

A cute and simple file server that serves files once or to a limited number of clients.

Usage:
  vrushie [options] <file>
  vrushie [options] --file <file>

Examples:
  vrushie document.pdf                           # Serve a single file once
  vrushie -n 3 photo.jpg                         # Serve to the first 3 unique IPs
  vrushie -z my_folder/                        # Serve a whole folder as a .zip archive
  vrushie -z --format tar.gz file1.txt file2.txt # Serve multiple files as a .tar.gz
  vrushie -ips "192.168.1.10" secret.zip         # Only allow a specific IP

Options:
`, version)
	flag.PrintDefaults()
	fmt.Println()
}

func printVersion() {
	fmt.Printf("🌸 Vrushie Server v%s 🌸\n", version)
}

func getTargetPaths() ([]string, error) {
	args := flag.Args()

	if len(args) == 0 {
		return nil, fmt.Errorf("no file or directory specified")
	}

	// In single-file mode, we only take one argument
	if !*zipMode && len(args) > 1 {
		return nil, fmt.Errorf("too many arguments for single file mode; use -z to serve multiple files as an archive")
	}

	return args, nil
}

// --- Main Function ---

func main() {
	// Custom usage function
	flag.Usage = printUsage
	flag.Parse()

	if *showHelp || *showHelpLong {
		printUsage()
		os.Exit(0)
	}
	if *showVersion || *showVersionLong {
		printVersion()
		os.Exit(0)
	}

	// --- Get file/dir paths ---
	paths, err := getTargetPaths()
	if err != nil {
		fmt.Println(styleError.Render(fmt.Sprintf("❌ Error: %s", err)))
		fmt.Println(styleSubtle.Render("\nUsage: vrushie [options] <file_or_dir...>"))
		fmt.Println(styleSubtle.Render("Try 'vrushie --help' for more information."))
		os.Exit(1)
	}

	// --- Input Validation ---
	for _, path := range paths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Println(styleError.Render(fmt.Sprintf("❌ Error: File or directory not found: %s", path)))
			os.Exit(1)
		}
	}
	if *zipMode {
		format := strings.ToLower(*archiveFormat)
		if format != "zip" && format != "tar.gz" {
			fmt.Println(styleError.Render(fmt.Sprintf("❌ Error: Invalid archive format '%s'. Must be 'zip' or 'tar.gz'.", *archiveFormat)))
			os.Exit(1)
		}
	}
	if *limitN < 1 && *allowedIPs == "" {
		fmt.Println(styleSubtle.Render("⚠️  Warning: -n must be 1 or greater. Defaulting to serve-once (n=1)."))
		*limitN = 1
	}

	// Setup logging
	log.SetOutput(io.Discard) // Disable standard logger by default

	// Create and run the Bubble Tea program
	model := initialModel(paths)
	p := tea.NewProgram(&model, tea.WithAltScreen())

	// Run Bubble Tea. This blocks until Quit is received.
	// Need to use p.Send for channel communication *after* Run starts
	go func() {
		for activity := range model.activityChan {
			p.Send(activityMsg{log: activity})
		}
	}()

	if _, err := p.Run(); err != nil {
		fmt.Printf("❌ Oh no! There was an error: %v\n", err)
		os.Exit(1)
	}

	// Exit message is handled in the model's View based on quitting state
}
