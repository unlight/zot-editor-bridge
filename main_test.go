package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func decodeFrames(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var frames []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("invalid frame %q: %v", line, err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestFormatPromptFull(t *testing.T) {
	got := formatPrompt(sendRequest{
		File:        "main.go",
		Instruction: "explain this",
		Language:    "go",
		Code:        "fmt.Println(\"hi\")",
	})
	want := "File: main.go\nInstruction: explain this\n\n```go\nfmt.Println(\"hi\")\n```"
	if got != want {
		t.Fatalf("prompt mismatch:\nwant: %q\ngot:  %q", want, got)
	}
}

func TestFormatPromptInstructionOnly(t *testing.T) {
	got := formatPrompt(sendRequest{Instruction: "  summarize the project  "})
	if want := "Instruction: summarize the project"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormatPromptCodeOnly(t *testing.T) {
	got := formatPrompt(sendRequest{Code: "print(1)"})
	if want := "```\nprint(1)\n```"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormatPromptEscapesCodeFence(t *testing.T) {
	got := formatPrompt(sendRequest{Instruction: "review", Language: "markdown", Code: "```js\nx\n```"})
	if !strings.Contains(got, "````markdown") || !strings.HasSuffix(got, "````") {
		t.Fatalf("expected a four-backtick fence, got %q", got)
	}
}

func TestHandleSendPromptMode(t *testing.T) {
	var out bytes.Buffer
	b := newBridge(newOutbound(&out))

	body := `{"code":"package main","instruction":"what is this?","file":"main.go","language":"go"}`
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	frames := decodeFrames(t, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d: %s", len(frames), out.String())
	}
	if frames[0]["type"] != "notify" || frames[0]["level"] != "info" {
		t.Fatalf("unexpected notify frame: %v", frames[0])
	}
	if frames[1]["type"] != "submit" {
		t.Fatalf("unexpected prompt frame: %v", frames[1])
	}
	text, _ := frames[1]["text"].(string)
	if !strings.Contains(text, "File: main.go") || !strings.Contains(text, "```go") {
		t.Fatalf("prompt lost context: %q", text)
	}
}

func TestHandleSendNotifyMode(t *testing.T) {
	var out bytes.Buffer
	b := newBridge(newOutbound(&out))

	body := `{"instruction":"just show this","mode":"notify"}`
	req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(body))
	rec := httptest.NewRecorder()
	b.handleSend(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	frames := decodeFrames(t, out.Bytes())
	if len(frames) != 1 || frames[0]["type"] != "notify" {
		t.Fatalf("expected a single notify frame, got %v", frames)
	}
}

func TestHandleSendValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty fields", `{}`, http.StatusBadRequest},
		{"blank fields", `{"code":"  ","instruction":" "}`, http.StatusBadRequest},
		{"invalid json", `{"instruction":`, http.StatusBadRequest},
		{"empty body", ``, http.StatusBadRequest},
		{"bad mode", `{"instruction":"x","mode":"chat"}`, http.StatusBadRequest},
		{"oversized", strings.Repeat("a", maxRequestBody+64), http.StatusRequestEntityTooLarge},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			b := newBridge(newOutbound(&out))
			req := httptest.NewRequest(http.MethodPost, "/send", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			b.handleSend(rec, req)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestHandleHealth(t *testing.T) {
	var out bytes.Buffer
	b := newBridge(newOutbound(&out))
	b.mu.Lock()
	b.cwd = "/work/project"
	b.session = "session-1"
	b.port = 47831
	b.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	b.handleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if payload["cwd"] != "/work/project" || payload["session_id"] != "session-1" || payload["ok"] != true {
		t.Fatalf("unexpected payload: %v", payload)
	}
}

func TestPortFileName(t *testing.T) {
	got := portFileName("/home/roman/Dev/zot-editor-bridge")
	if want := "editor-bridge-7db538f0.port"; got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if portFileName("/home/roman/Dev/zot-editor-bridge/") != got {
		t.Fatalf("trailing slash must not change the name")
	}
	if portFileName("/work/project") == got {
		t.Fatalf("different cwd must produce a different name")
	}
}

func TestPortFilesRespectOtherSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, extensionName+".port")

	written := writePortFiles([]string{path}, 47831)
	if len(written) != 1 {
		t.Fatalf("expected one port file, got %v", written)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read port file: %v", err)
	}
	if strings.TrimSpace(string(data)) != "47831" {
		t.Fatalf("unexpected port file content: %q", data)
	}

	// A different port means another session owns the file: keep it.
	if err := os.WriteFile(path, []byte("50000\n"), 0o600); err != nil {
		t.Fatalf("rewrite port file: %v", err)
	}
	removePortFiles([]string{path}, 47831)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("port file of another session was removed: %v", err)
	}

	if err := os.WriteFile(path, []byte(strconv.Itoa(47831)+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite port file: %v", err)
	}
	removePortFiles([]string{path}, 47831)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("own port file was not removed: %v", err)
	}
}

func TestResolvePort(t *testing.T) {
	t.Setenv("ZOT_EDITOR_BRIDGE_PORT", "51234")
	if got := resolvePort(); got != 51234 {
		t.Fatalf("want 51234, got %d", got)
	}
	t.Setenv("ZOT_EDITOR_BRIDGE_PORT", "not-a-port")
	if got := resolvePort(); got != defaultPort {
		t.Fatalf("want default %d, got %d", defaultPort, got)
	}
	t.Setenv("ZOT_EDITOR_BRIDGE_PORT", "70000")
	if got := resolvePort(); got != defaultPort {
		t.Fatalf("want default %d, got %d", defaultPort, got)
	}
}
