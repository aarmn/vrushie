package main

import (
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
)

const version = "1.0.2"

// --- Configuration ---
var (
	port            = flag.Int("port", 0, "Port to listen on (0 for random available port)")
	limitN          = flag.Int("n", 1, "Number of downloads allowed (1 = serve once, >1 = serve to N unique IPs)")
	allowedIPs      = flag.String("ips", "", "Comma-separated list of specific IPs allowed to connect")
	showHelp        = flag.Bool("h", false, "Show help message")
	showHelpLong    = flag.Bool("help", false, "Show help message")
	showVersion     = flag.Bool("v", false, "Show version information")
	showVersionLong = flag.Bool("version", false, "Show version information")
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
	filePath       string
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

type serverReadyMsg struct{ url string }
type serverErrMsg struct{ err error }
type activityMsg struct{ log activityLog }
type shutdownMsg struct{} // Message to initiate shutdown

// --- Bubbletea Implementation ---

func initialModel(filePath string) model {
	s := spinner.New()
	s.Spinner = spinner.Dot // Or choose another cute one: Line, Jump, Pulse, Points, Globe, Moon, Monkey
	s.Style = styleSpinner

	m := model{
		spinner:        s,
		shutdownChan:   make(chan struct{}),
		errChan:        make(chan error, 1),        // Buffered to prevent blocking
		activityChan:   make(chan activityLog, 10), // Buffered channel for activities
		serverReady:    false,
		filePath:       filePath,
		fileName:       filepath.Base(filePath),
		limitN:         *limitN,
		specificIPs:    make(map[string]struct{}),
		allowedFirstN:  make(map[string]struct{}),
		maxActivityLog: 10, // Keep last 10 activities
		activity:       make([]activityLog, 0, 10),
	}

	// Determine File Size
	info, err := os.Stat(filePath)
	if err == nil {
		m.fileSize = info.Size()
	} // Error handled later in main

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

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.startServer())
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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

func (m model) View() string {
	if m.quitting && m.lastError == nil {
		return styleSuccess.Render("\nServer shut down gracefully. Bye! ♡\n\n")
	}
	if m.lastError != nil {
		return styleError.Render(fmt.Sprintf("\nServer Error: %v\n\n", m.lastError))
	}

	var s strings.Builder

	// Header
	s.WriteString(stylePrimary.Render("🌸 Vrushie Server 🌸"))
	s.WriteString("\n\n")

	// File Info
	s.WriteString(fmt.Sprintf("Serving File: %s\n", styleSecondary.Render(m.fileName)))
	s.WriteString(fmt.Sprintf("Size: %s\n", styleSubtle.Render(formatBytes(m.fileSize))))
	s.WriteString("\n")

	// Server Status
	if !m.serverReady {
		s.WriteString(fmt.Sprintf("%s Initializing server...", m.spinner.View()))
	} else {
		s.WriteString(styleSuccess.Render("Server Ready! ✨\n"))
		s.WriteString("Listening on:\n")
		urls := strings.Split(m.servingURL, "\n")
		for _, url := range urls {
			if url != "" {
				s.WriteString(fmt.Sprintf("  %s\n", styleURL.Render(url)))
			}
		}
	}
	s.WriteString("\n")

	// Access Mode
	s.WriteString(fmt.Sprintf("Access Mode: %s\n", stylePrimary.Render(m.accessMode)))
	if len(m.specificIPs) > 0 {
		var ips []string
		for ip := range m.specificIPs {
			ips = append(ips, ip)
		}
		s.WriteString(fmt.Sprintf("Allowed IPs: %s\n", styleSubtle.Render(strings.Join(ips, ", "))))
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
		s.WriteString(fmt.Sprintf("First %d IPs: %s\n", m.limitN, styleSubtle.Render(status)))
	}
	s.WriteString("\n")

	// Activity Log
	s.WriteString("Activity Log:\n")
	if len(m.activity) == 0 {
		s.WriteString(styleSubtle.Render("  No activity yet...\n"))
	} else {
		// Display in reverse chronological order (newest first)
		for i := len(m.activity) - 1; i >= 0; i-- {
			logEntry := m.activity[i]
			ts := logEntry.Timestamp.Format("15:04:05")
			s.WriteString(fmt.Sprintf("  %s [%s] %s\n",
				styleSubtle.Render(ts),
				logEntry.Style.Render(logEntry.IP),
				logEntry.Action,
			))
		}
	}

	// Footer/Instructions
	if !m.quitting {
		s.WriteString(styleInstructions.Render("\nPress 'q' or Ctrl+C to shut down manually."))
	} else {
		s.WriteString(styleInstructions.Render("\nShutting down..."))
	}

	// Apply border and padding
	return styleBorder.Render(s.String())
}

// --- Helper Functions ---

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
		ips := getOutboundIPs()
		var urlBuilder strings.Builder
		for _, ip := range ips {
			urlBuilder.WriteString(fmt.Sprintf("http://%s:%d/\n", ip, actualPort))
		}
		// Always include localhost
		if !contains(ips, "127.0.0.1") {
			urlBuilder.WriteString(fmt.Sprintf("http://127.0.0.1:%d/\n", actualPort))
		}
		serverURL := strings.TrimSpace(urlBuilder.String())

		// Create server
		mux := http.NewServeMux()
		mux.HandleFunc("/", m.fileHandler) // Pass model method
		m.server = &http.Server{
			Handler: mux,
			// Add timeouts for robustness? e.g., ReadTimeout, WriteTimeout
		}

		// Start server in a goroutine
		go func() {
			<-m.shutdownChan // Wait for shutdown signal
			log.Println("Shutdown signal received, stopping server...")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // Graceful shutdown timeout
			defer cancel()
			if err := m.server.Shutdown(ctx); err != nil {
				// Send error back to main loop if shutdown fails
				m.errChan <- fmt.Errorf("server shutdown failed: %w", err)
			} else {
				log.Println("Server stopped gracefully.")
			}
			close(m.errChan) // Signal that shutdown goroutine is done
		}()

		// Start listening in another goroutine, send errors back via channel
		go func() {
			log.Printf("Server starting on port %d...", actualPort)
			if err := m.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				m.errChan <- fmt.Errorf("server failed: %w", err)
			}
			log.Println("Server Serve() function finished.")
		}()

		// Report server ready via message
		return serverReadyMsg{url: serverURL}
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
	// Get client IP (handle potential proxies later if needed)
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr // Fallback if split fails
	}

	m.ipLock.Lock()
	isAllowed := false
	reason := "Access denied"

	if len(m.specificIPs) > 0 {
		// Mode 1: Specific IPs
		if _, ok := m.specificIPs[ip]; ok {
			isAllowed = true
		} else {
			reason = "IP not in allowed list"
		}
	} else if m.limitN > 1 {
		// Mode 2: First N unique IPs
		if _, ok := m.allowedFirstN[ip]; ok {
			// Already seen and allowed
			isAllowed = true
		} else if len(m.allowedFirstN) < m.limitN {
			// New IP within limit, allow and add
			m.allowedFirstN[ip] = struct{}{}
			isAllowed = true
		} else {
			reason = fmt.Sprintf("Limit of %d unique IPs reached", m.limitN)
		}
	} else {
		// Mode 3: Serve once (limitN == 1) - Allow first connection attempt
		isAllowed = true
	}

	// Check if download limit is already reached (even if IP is allowed)
	// This handles the N > 1 case where an allowed IP tries after N downloads finished
	if isAllowed && m.limitN > 1 && m.downloadCount >= m.limitN {
		isAllowed = false
		reason = fmt.Sprintf("Download limit of %d already reached", m.limitN)
	}
	// This handles the serve-once case after the first download finished
	if isAllowed && m.limitN == 1 && m.downloadCount > 0 {
		isAllowed = false
		reason = "File has already been downloaded"
	}

	m.ipLock.Unlock() // Release lock before logging and serving

	// Log activity and potentially reject
	if !isAllowed {
		logMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Rejected: %s", reason), Style: styleIPRejected}
		m.activityChan <- logMsg // Send to TUI via channel
		http.Error(w, reason, http.StatusForbidden)
		return
	}

	// Log allowed connection attempt
	logMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: "Connected & Allowed", Style: styleIPAllowed}
	m.activityChan <- logMsg

	// --- Serve the file ---
	file, err := os.Open(m.filePath)
	if err != nil {
		errMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Error opening file: %s", err), Style: styleError}
		m.activityChan <- errMsg
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	// Set headers for download
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(m.fileName))
	w.Header().Set("Content-Type", "application/octet-stream") // Generic byte stream
	w.Header().Set("Content-Length", strconv.FormatInt(m.fileSize, 10))

	// Use ServeContent for efficiency (handles Range requests etc.)
	// http.ServeContent(w, r, m.fileName, time.Time{}, file) // Simpler version

	// Or io.Copy for explicit control/error checking (though ServeContent is usually better)
	_, copyErr := io.Copy(w, file)

	// Check if the copy was successful *from the server's perspective*
	// This doesn't perfectly guarantee the client got everything, but it's the best we can easily do.
	if copyErr == nil {
		// Log successful download completion
		successMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: "Download Complete", Style: styleSuccess}
		// Send via channel - this will trigger the Update logic to check shutdown condition
		m.activityChan <- successMsg
	} else {
		// Log potential error during transfer
		errMsg := activityLog{Timestamp: time.Now(), IP: ip, Action: fmt.Sprintf("Error during transfer: %s", copyErr), Style: styleError}
		m.activityChan <- errMsg
		// Don't explicitly trigger shutdown on transfer error
	}
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
  vrushie document.pdf                    # Serve once to first downloader
  vrushie -n 3 photo.jpg                  # Serve to first 3 unique IPs
  vrushie -port 8080 video.mp4           # Serve on specific port
  vrushie -ips "192.168.1.10,192.168.1.20" file.zip  # Only allow specific IPs

Options:
`, version)
	flag.PrintDefaults()
	fmt.Println()
}

func printVersion() {
	fmt.Printf("🌸 Vrushie Server v%s 🌸\n", version)
}

func getFilePath() (string, error) {
	args := flag.Args()

	// Check if file provided as positional argument
	if len(args) > 0 {
		return args[0], nil
	}

	// If no positional argument, show error with helpful message
	return "", fmt.Errorf("no file specified")
}

// --- Main Function ---

func main() {
	// Custom usage function
	flag.Usage = printUsage
	flag.Parse()

	// Handle help and version flags
	if *showHelp || *showHelpLong {
		printUsage()
		os.Exit(0)
	}

	if *showVersion || *showVersionLong {
		printVersion()
		os.Exit(0)
	}

	// --- Get file path ---
	filePath, err := getFilePath()
	if err != nil {
		fmt.Println(styleError.Render("❌ Error: No file specified"))
		fmt.Println(styleSubtle.Render("\nUsage: vrushie [options] <file>"))
		fmt.Println(styleSubtle.Render("Try 'vrushie --help' for more information."))
		os.Exit(1)
	}

	// --- Input Validation ---
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		fmt.Println(styleError.Render(fmt.Sprintf("❌ Error: File not found: %s", filePath)))
		os.Exit(1)
	}
	if *limitN < 1 && *allowedIPs == "" {
		// If -n is 0 or less, and no specific IPs are given, default to serve-once.
		fmt.Println(styleSubtle.Render("⚠️  Warning: -n must be 1 or greater. Defaulting to serve-once (n=1)."))
		*limitN = 1
	}

	// Setup logging (optional, for debugging server internals)
	// You can pipe this to a file if needed: go run main.go ... >> server.log 2>&1
	log.SetOutput(io.Discard) // Disable standard logger by default, TUI shows info
	// log.SetOutput(os.Stderr) // Enable if debugging needed

	// Create and run the Bubble Tea program
	model := initialModel(filePath)
	p := tea.NewProgram(model, tea.WithAltScreen()) // Use AltScreen for clean exit

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
