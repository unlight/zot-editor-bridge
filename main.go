// editor-bridge: HTTP bridge between an editor and the active zot session.
//
// The extension runs a local HTTP server and turns POST /send into a user
// prompt (submit frame) or a status notification (notify frame).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	extensionName    = "editor-bridge"
	extensionVersion = "0.1.0"

	// defaultPort is five digits to avoid common development ports.
	defaultPort = 47831

	maxRequestBody = 1 << 20 // 1 MiB
	readTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	shutdownGrace  = 2 * time.Second
)

var logger = log.New(os.Stderr, "["+extensionName+"] ", log.LstdFlags)

// outbound serializes NDJSON frame writes to stdout: HTTP handlers and the
// main loop write concurrently, and json.Encoder is not thread-safe.
type outbound struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newOutbound(w io.Writer) *outbound {
	return &outbound{enc: json.NewEncoder(w)}
}

func (o *outbound) emit(frames ...map[string]any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, frame := range frames {
		if err := o.enc.Encode(frame); err != nil {
			logger.Printf("stdout write failed: %v", err)
		}
	}
}

func notifyFrame(level, message string, startup bool) map[string]any {
	frame := map[string]any{"type": "notify", "level": level, "message": message}
	if startup {
		frame["startup"] = true
	}
	return frame
}

// sendRequest is the POST /send body.
type sendRequest struct {
	Code        string `json:"code"`
	Instruction string `json:"instruction"`
	File        string `json:"file"`
	Language    string `json:"language"`
	Mode        string `json:"mode"`
}

// bridge holds session metadata and the HTTP server state.
type bridge struct {
	out *outbound

	mu       sync.RWMutex
	cwd      string
	session  string
	port     int
	portFile []string

	httpServer *http.Server
	stopOnce   sync.Once
}

func newBridge(out *outbound) *bridge {
	return &bridge{out: out}
}

// observeFrame picks up metadata the host attaches to various frames.
func (b *bridge) observeFrame(frame map[string]any) {
	session, _ := frame["session_id"].(string)
	cwd, _ := frame["cwd"].(string)

	b.mu.Lock()
	defer b.mu.Unlock()
	if session != "" {
		b.session = session
	}
	if cwd != "" {
		b.cwd = cwd
	}
}

func (b *bridge) sessionState() (cwd, session string, port int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cwd, b.session, b.port
}

// start listens on 127.0.0.1 and publishes the port in discovery files.
func (b *bridge) start() {
	cwd, session, _ := b.sessionState()
	desiredPort := resolvePort()

	ln, port, err := listenLocal(desiredPort)
	if err != nil {
		logger.Printf("cannot start HTTP server: %v", err)
		b.out.emit(notifyFrame("error", fmt.Sprintf("HTTP bridge failed to start: %v", err), true))
		return
	}

	portFiles := writePortFiles(portFilePaths(cwd), port)
	if len(portFiles) == 0 {
		logger.Printf("warning: port file was not written; editors cannot discover the port")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", b.handleHealth)
	mux.HandleFunc("/send", b.handleSend)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: readTimeout,
		WriteTimeout:      writeTimeout,
	}

	b.mu.Lock()
	b.port = port
	b.portFile = portFiles
	b.httpServer = srv
	b.mu.Unlock()

	go func() {
		if serveErr := srv.Serve(ln); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Printf("HTTP server stopped: %v", serveErr)
		}
	}()

	logger.Printf("listening on http://127.0.0.1:%d (cwd=%s session=%s)", port, cwd, session)

	message := fmt.Sprintf("HTTP bridge listening on port %d", port)
	b.out.emit(notifyFrame("info", message, true))
}

// stop shuts the server down and removes port files owned by this process.
func (b *bridge) stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		srv := b.httpServer
		port := b.port
		files := b.portFile
		b.mu.Unlock()

		if srv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
			defer cancel()
			if err := srv.Shutdown(ctx); err != nil {
				logger.Printf("HTTP shutdown: %v", err)
			}
		}
		removePortFiles(files, port)
	})
}

func resolvePort() int {
	raw := strings.TrimSpace(os.Getenv("ZOT_EDITOR_BRIDGE_PORT"))
	if raw == "" {
		return defaultPort
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 0 || port > 65535 {
		logger.Printf("invalid ZOT_EDITOR_BRIDGE_PORT=%q, using %d", raw, defaultPort)
		return defaultPort
	}
	return port
}

// listenLocal tries the desired port first, then an ephemeral one so that
// multiple zot windows can run at the same time.
func listenLocal(desired int) (net.Listener, int, error) {
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(desired))
	ln, err := net.Listen("tcp", address)
	if err == nil {
		port := desired
		if port == 0 {
			port = ln.Addr().(*net.TCPAddr).Port
		}
		return ln, port, nil
	}
	if desired == 0 {
		return nil, 0, err
	}

	logger.Printf("port %d is unavailable (%v), falling back to an ephemeral port", desired, err)
	ln, fallbackErr := net.Listen("tcp", "127.0.0.1:0")
	if fallbackErr != nil {
		return nil, 0, fmt.Errorf("port %d: %v; ephemeral port: %w", desired, err, fallbackErr)
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// stateDir mirrors zot: ZOT_HOME, then XDG_STATE_HOME, then the OS path.
func stateDir() string {
	if home := strings.TrimSpace(os.Getenv("ZOT_HOME")); home != "" {
		return home
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "zot")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "zot")
	case "windows":
		if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
			return filepath.Join(local, "zot")
		}
		return filepath.Join(home, "AppData", "Local", "zot")
	default:
		return filepath.Join(home, ".local", "state", "zot")
	}
}

// portFilePaths returns the primary zot path and the compatible ~/.zot path.
func portFilePaths(cwd string) []string {
	name := portFileName(cwd)
	primary := filepath.Join(stateDir(), name)
	paths := []string{primary}

	if strings.TrimSpace(os.Getenv("ZOT_HOME")) != "" {
		return paths
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return paths
	}
	legacy := filepath.Join(home, ".zot", name)
	if legacy != primary {
		paths = append(paths, legacy)
	}
	return paths
}

// portFileName scopes discovery to a project: sha256(cwd)[:8].
func portFileName(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return extensionName + "-" + hex.EncodeToString(sum[:4]) + ".port"
}

func writePortFiles(paths []string, port int) []string {
	written := make([]string, 0, len(paths))
	content := []byte(strconv.Itoa(port) + "\n")

	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			logger.Printf("create port dir %s: %v", filepath.Dir(path), err)
			continue
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			logger.Printf("write port file %s: %v", path, err)
			continue
		}
		written = append(written, path)
	}
	return written
}

// removePortFiles keeps a file that another session has already overwritten.
func removePortFiles(paths []string, port int) {
	if port == 0 {
		return
	}
	expected := strconv.Itoa(port)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(data)) != expected {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			logger.Printf("remove port file %s: %v", path, err)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		logger.Printf("HTTP response write failed: %v", err)
	}
}

func (b *bridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	cwd, session, port := b.sessionState()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"cwd":        cwd,
		"session_id": session,
		"port":       port,
	})
}

func (b *bridge) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
		return
	}

	req, err := decodeSendRequest(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "error": "request body exceeds 1 MiB",
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "prompt"
	}
	if mode != "prompt" && mode != "notify" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": `mode must be "prompt" or "notify"`,
		})
		return
	}

	text := formatPrompt(req)
	label := strings.TrimSpace(req.File)
	if label == "" {
		label = "no file"
	}

	frames := []map[string]any{
		notifyFrame("info", "Editor request: "+label, false),
	}
	if mode == "prompt" {
		// The real zot protocol accepts user text through the submit frame.
		frames = append(frames, map[string]any{"type": "submit", "text": text})
	}
	b.out.emit(frames...)

	_, session, port := b.sessionState()
	logger.Printf("accepted %s request (mode=%s file=%s bytes=%d)", extensionName, mode, label, len(text))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"mode":       mode,
		"port":       port,
		"session_id": session,
	})
}

func decodeSendRequest(w http.ResponseWriter, r *http.Request) (sendRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return sendRequest{}, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return sendRequest{}, errors.New("request body is empty")
	}

	var req sendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return sendRequest{}, fmt.Errorf("invalid JSON: %v", err)
	}
	if strings.TrimSpace(req.Instruction) == "" && strings.TrimSpace(req.Code) == "" {
		return sendRequest{}, errors.New("instruction or code is required")
	}
	return req, nil
}

// formatPrompt assembles the Markdown context for the agent.
func formatPrompt(req sendRequest) string {
	file := oneLine(req.File)
	instruction := strings.TrimSpace(req.Instruction)
	language := sanitizeLanguage(req.Language)
	code := req.Code

	var b strings.Builder
	if file != "" {
		fmt.Fprintf(&b, "File: %s\n", file)
	}
	if instruction != "" {
		fmt.Fprintf(&b, "Instruction: %s\n", instruction)
	}

	if strings.TrimSpace(code) == "" {
		return strings.TrimRight(b.String(), "\n")
	}

	if b.Len() > 0 {
		b.WriteString("\n")
	}
	fence := codeFence(code)
	b.WriteString(fence)
	b.WriteString(language)
	b.WriteString("\n")
	b.WriteString(code)
	if !strings.HasSuffix(code, "\n") {
		b.WriteString("\n")
	}
	b.WriteString(fence)
	b.WriteString("\n")

	return strings.TrimRight(b.String(), "\n")
}

func oneLine(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}

func sanitizeLanguage(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`")
	value = strings.Join(strings.Fields(value), "")
	if len(value) > 32 {
		value = value[:32]
	}
	return value
}

// codeFence returns a fence longer than any backtick run in the code.
func codeFence(code string) string {
	longest, current := 0, 0
	for _, r := range code {
		if r == '`' {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	if longest < 2 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

func (b *bridge) handleCommand(id, name string) {
	if name != extensionName {
		return
	}

	cwd, session, port := b.sessionState()
	if port == 0 {
		b.out.emit(map[string]any{
			"type":    "command_response",
			"id":      id,
			"action":  "display",
			"display": "editor-bridge: HTTP bridge is not running; check the extension log",
		})
		return
	}

	address := fmt.Sprintf("http://127.0.0.1:%d", port)
	lines := []string{
		"editor-bridge",
		"  send:   " + address + "/send",
		"  health: " + address + "/health",
		"  cwd:    " + cwd,
	}
	if session != "" {
		lines = append(lines, "  session: "+session)
	}
	if paths := portFilePaths(cwd); len(paths) > 0 {
		lines = append(lines, "  port file: "+paths[0])
	}

	b.out.emit(map[string]any{
		"type":    "command_response",
		"id":      id,
		"action":  "display",
		"display": strings.Join(lines, "\n"),
	})
}

// register sends command registrations and readiness to the host.
func (b *bridge) register() {
	description := "Show HTTP bridge status and port"
	b.out.emit(map[string]any{
		"type":        "register_command",
		"name":        extensionName,
		"description": description,
	})

	b.out.emit(map[string]any{
		"type":   "subscribe",
		"events": []string{"session_start", "session_end"},
	})

	b.out.emit(map[string]any{
		"type":    "ready",
		"name":    extensionName,
		"version": extensionVersion,
		"commands": []map[string]any{
			{"name": extensionName, "description": description},
		},
	})
}

func main() {
	out := newOutbound(os.Stdout)
	b := newBridge(out)

	out.emit(map[string]any{
		"type":         "hello",
		"name":         extensionName,
		"version":      extensionVersion,
		"capabilities": []string{"commands"},
	})

	frames := make(chan map[string]any)
	readErr := make(chan error, 1)

	go func() {
		decoder := json.NewDecoder(os.Stdin)
		for {
			var frame map[string]any
			if err := decoder.Decode(&frame); err != nil {
				readErr <- err
				return
			}
			frames <- frame
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	started := false

	for {
		select {
		case <-signals:
			logger.Printf("signal received, shutting down")
			b.stop()
			return

		case err := <-readErr:
			if !errors.Is(err, io.EOF) {
				logger.Printf("stdin read failed: %v", err)
			}
			b.stop()
			return

		case frame := <-frames:
			b.observeFrame(frame)

			switch typeName, _ := frame["type"].(string); typeName {
			case "hello", "hello_ack":
				if started {
					continue
				}
				started = true
				b.register()
				b.start()

			case "shutdown":
				out.emit(map[string]any{"type": "shutdown_ack"})
				b.stop()
				return

			case "command_invoked":
				id, _ := frame["id"].(string)
				name, _ := frame["name"].(string)
				b.handleCommand(id, name)

			case "session_end":
				b.mu.Lock()
				b.session = ""
				b.mu.Unlock()
			}
		}
	}
}
