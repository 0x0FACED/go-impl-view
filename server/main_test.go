package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (output *capturedOutput) Write(data []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.buffer.Write(data)
}
func (output *capturedOutput) bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.buffer.Bytes()...)
}
func testServer(t *testing.T, root string) (*server, *capturedOutput) {
	t.Helper()
	output := &capturedOutput{}
	s := newServer(&connection{writer: output})
	s.initialized = true
	s.roots = []string{root}
	s.options = fixtureSnapshot(root).options
	t.Cleanup(func() { s.mu.Lock(); s.invalidate(); s.mu.Unlock(); s.workers.Wait() })
	return s, output
}
func notificationFor(t *testing.T, method string, params any) message {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return message{JSONRPC: "2.0", Method: method, Params: raw}
}
func requestFor(t *testing.T, id, method string, params any) message {
	t.Helper()
	m := notificationFor(t, method, params)
	m.ID = json.RawMessage(id)
	return m
}
func decodedFrames(t *testing.T, data []byte) []map[string]json.RawMessage {
	t.Helper()
	reader := bufio.NewReader(bytes.NewReader(data))
	var frames []map[string]json.RawMessage
	for {
		line, err := reader.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var length int
		if _, err := fmt.Sscanf(line, "Content-Length: %d\r\n", &length); err != nil {
			t.Fatal(err)
		}
		if line, err := reader.ReadString('\n'); err != nil || line != "\r\n" {
			t.Fatalf("bad header: %q, %v", line, err)
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Fatal(err)
		}
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(body, &frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestChangesCancelGenerationRefreshAndRejectOldRequest(t *testing.T) {
	s, output := testServer(t, t.TempDir())
	s.refreshSupport = true
	oldGeneration := s.generation
	ctx, cancel := context.WithCancel(s.generationCtx)
	s.requests["1"] = cancel
	s.cache = &flight{done: make(chan struct{})}
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(filepath.Join(s.roots[0], "other.go")), "version": 1, "text": "package p\n"}}))
	if ctx.Err() == nil || s.generation == oldGeneration || s.cache != nil {
		t.Fatal("edit failed to cancel and invalidate prior generation")
	}
	s.workers.Add(1)
	s.codeLenses(ctx, cancel, json.RawMessage("1"), filepath.Join(s.roots[0], "main.go"), oldGeneration)
	frames := decodedFrames(t, output.bytes())
	if len(frames) != 2 || string(frames[0]["method"]) != `"workspace/codeLens/refresh"` {
		t.Fatalf("missing refresh: %#v", frames)
	}
	var failure rpcError
	if err := json.Unmarshal(frames[1]["error"], &failure); err != nil || failure.Code != -32801 {
		t.Fatalf("old request was not rejected: %#v %v", frames[1], err)
	}
}

func TestDocumentOverlaysIncrementalEditsSaveCloseAndWatchedFiles(t *testing.T) {
	source := "package specimen\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source, "impl.go": "package specimen\ntype T struct{}\n"})
	s, _ := testServer(t, root)
	path := filepath.Join(root, "impl.go")
	openText := "package specimen\ntype T struct{}\nfunc (*T) M() {}\n"
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "version": 1, "text": openText}}))
	assertTitle := func(want string) {
		t.Helper()
		result, err := s.getAnalysis(context.Background(), s.generation)
		if err != nil {
			t.Fatal(err)
		}
		if got := typeLens(t, result, filepath.Join(root, "main.go"), source, "I").Command.Title; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	assertTitle("1 implementation")
	s.notification(notificationFor(t, "textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": fileURI(path), "version": 2},
		"contentChanges": []any{map[string]any{"range": sourceRange{Start: position{2, 10}, End: position{2, 11}}, "text": "N"}},
	}))
	assertTitle("0 implementations")
	// A stale version must not roll the overlay backward.
	s.notification(notificationFor(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "version": 1}, "contentChanges": []any{map[string]any{"text": openText}}}))
	assertTitle("0 implementations")
	if err := os.WriteFile(path, []byte(openText), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "textDocument/didSave", map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}, "text": openText}))
	assertTitle("1 implementation")
	if err := os.WriteFile(path, []byte("package specimen\ntype T struct{}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}}))
	assertTitle("0 implementations")
	if err := os.WriteFile(path, []byte(openText), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": fileURI(path), "type": 2}}}))
	assertTitle("1 implementation")
}

func TestEmptyConfigurationPreservesInitializationOptions(t *testing.T) {
	arch := "386"
	if runtime.GOARCH == arch {
		arch = "amd64"
	}
	source := "package specimen\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"go.mod":    "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":   source,
		"tagged.go": "//go:build custom && " + arch + "\n\npackage specimen\ntype T struct{}\nfunc (T) M() {}\n",
	})
	s, _ := testServer(t, root)
	s.initialized = false
	opts := fixtureSnapshot(root).options
	opts.BuildFlags = []string{"-tags=custom"}
	opts.Env["GOOS"], opts.Env["GOARCH"], opts.Env["CGO_ENABLED"] = "linux", arch, "0"
	s.initialize(requestFor(t, "1", "initialize", map[string]any{
		"rootUri": fileURI(root), "initializationOptions": opts,
	}))
	assertTitle := func(want string) {
		t.Helper()
		result, err := s.getAnalysis(context.Background(), s.generation)
		if err != nil {
			t.Fatal(err)
		}
		if got := typeLens(t, result, filepath.Join(root, "main.go"), source, "I").Command.Title; got != want {
			t.Fatalf("got %s, want %s", got, want)
		}
	}
	for _, settings := range []map[string]any{
		{}, {"goImplView": map[string]any{}}, {"unrelated": true},
	} {
		s.notification(notificationFor(t, "workspace/didChangeConfiguration", map[string]any{"settings": settings}))
		assertTitle("1 implementation")
	}
	// Missing fields retain their initialized value; explicit empty fields reset.
	s.notification(notificationFor(t, "workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"buildFlags": []string{}}}))
	assertTitle("0 implementations")
	s.notification(notificationFor(t, "workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"goImplView": map[string]any{"buildFlags": []string{"-tags=custom"}}}}))
	assertTitle("1 implementation")
	s.notification(notificationFor(t, "workspace/didChangeConfiguration", map[string]any{"settings": map[string]any{"env": map[string]string{}}}))
	if len(s.options.Env) != 0 || len(s.options.BuildFlags) != 1 {
		t.Fatalf("explicit env reset changed other options: %#v", s.options)
	}
}

func TestGoWorkOutsideOpenedRootsCannotReuseCachedCounts(t *testing.T) {
	source := "package api\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"api/go.work":  "go 1.26.0\n\nuse .\n",
		"api/go.mod":   "module example.com/api\n\ngo 1.26.0\n",
		"api/api.go":   source,
		"impl/go.mod":  "module example.com/impl\n\ngo 1.26.0\n",
		"impl/impl.go": "package impl\ntype T struct{}\nfunc (T) M() {}\n",
	})
	api := filepath.Join(root, "api")
	work := filepath.Join(api, "go.work")
	s, _ := testServer(t, api)
	s.options.Env["GOWORK"] = work
	result, err := s.getAnalysis(context.Background(), s.generation)
	if err != nil {
		t.Fatal(err)
	}
	if got := typeLens(t, result, filepath.Join(api, "api.go"), source, "I").Command.Title; got != "0 implementations" {
		t.Fatal(got)
	}
	if err := os.WriteFile(work, []byte("go 1.26.0\n\nuse (\n .\n ../impl\n)\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": fileURI(work), "type": 2}}}))
	if result, err := s.getAnalysis(context.Background(), s.generation); result != nil || err == nil || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("unwatched go.work use reused cached counts: %#v, %v", result, err)
	}
}

func TestInvalidIncrementalEditCannotReuseCounts(t *testing.T) {
	s, _ := testServer(t, t.TempDir())
	path := filepath.Join(s.roots[0], "main.go")
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "version": 1, "text": "package p\n// 😀\n"}}))
	s.notification(notificationFor(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "version": 2}, "contentChanges": []any{map[string]any{"range": sourceRange{Start: position{1, 4}, End: position{1, 4}}, "text": "x"}}}))
	if result, err := s.getAnalysis(context.Background(), s.generation); err == nil || result != nil {
		t.Fatalf("invalid edit returned counts: %#v %v", result, err)
	}
	if s.documents[path].invalid == "" {
		t.Fatal("document not marked out of sync")
	}
}

func TestPendingAnalysisClearsLensesThenRefreshesOnce(t *testing.T) {
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": "package specimen\ntype I interface { M() }\n"})
	s, output := testServer(t, root)
	s.refreshSupport = true
	result, err := s.getAnalysis(context.Background(), s.generation)
	if err != nil {
		t.Fatal(err)
	}
	// A fast analysis may already be ready. Otherwise the immediate response
	// must be empty; the completion refresh supplies the actual current lenses.
	if result == nil && s.cache == nil {
		t.Fatal("no pending analysis")
	}
	select {
	case <-s.cache.done:
	case <-time.After(30 * time.Second):
		t.Fatal("analysis did not finish")
	}
	s.workers.Wait()
	before := len(decodedFrames(t, output.bytes()))
	if before != 1 {
		t.Fatalf("expected one completion refresh, got %d", before)
	}
	result, err = s.getAnalysis(context.Background(), s.generation)
	if err != nil || result == nil {
		t.Fatalf("ready analysis missing: %#v %v", result, err)
	}
	if after := len(decodedFrames(t, output.bytes())); after != before {
		t.Fatalf("ready cache caused a refresh loop: %d -> %d", before, after)
	}
}

func TestClientCancellationIsHonored(t *testing.T) {
	s, _ := testServer(t, t.TempDir())
	ctx, cancel := context.WithCancel(s.generationCtx)
	defer cancel()
	s.requests[`"request"`] = cancel
	s.notification(notificationFor(t, "$/cancelRequest", map[string]any{"id": "request"}))
	if ctx.Err() == nil {
		t.Fatal("client cancellation ignored")
	}
}

func TestServerResponsesAreNotTreatedAsRequests(t *testing.T) {
	var input bytes.Buffer
	encoder := &connection{writer: &input}
	if err := encoder.send(map[string]any{"jsonrpc": "2.0", "id": "refresh", "result": nil}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "shutdown"}); err != nil {
		t.Fatal(err)
	}
	if err := encoder.notify("exit", nil); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	s := newServer(&connection{reader: bufio.NewReader(&input), writer: &output})
	s.initialized = true
	if err := s.run(); err != nil {
		t.Fatal(err)
	}
	frames := decodedFrames(t, output.Bytes())
	if len(frames) != 1 || string(frames[0]["id"]) != "1" || !strings.EqualFold(string(frames[0]["result"]), "null") {
		t.Fatalf("unexpected responses: %#v", frames)
	}
}
