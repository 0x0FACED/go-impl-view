package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type document struct {
	text    []byte
	version int
	invalid string
}
type flight struct {
	done   chan struct{}
	result *analysis
	err    error
}
type server struct {
	conn            *connection
	mu              sync.Mutex
	roots           []string
	documents       map[string]document
	options         options
	configErr       error
	generation      uint64
	generationCtx   context.Context
	invalidate      context.CancelFunc
	cache           map[string]*flight
	errorReported   map[string]bool
	warningReported map[string]bool
	requests        map[string]context.CancelFunc
	initialized     bool
	shutdown        bool
	refreshSupport  bool
	watchSupport    bool
	sequence        uint64
	workers         sync.WaitGroup
}

func newServer(conn *connection) *server {
	ctx, cancel := context.WithCancel(context.Background())
	return &server{conn: conn, documents: map[string]document{}, requests: map[string]context.CancelFunc{}, generationCtx: ctx, invalidate: cancel}
}
func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("go-impl-view: ")
	s := newServer(&connection{reader: bufio.NewReader(os.Stdin), writer: os.Stdout})
	if err := s.run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
func (s *server) run() error {
	defer func() {
		s.mu.Lock()
		s.invalidate()
		for _, cancel := range s.requests {
			cancel()
		}
		s.mu.Unlock()
		s.workers.Wait()
	}()
	for {
		m, err := s.conn.read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		// Client replies to server refresh/registration requests carry no method.
		if m.Method == "" {
			continue
		}
		if m.Method == "exit" {
			s.mu.Lock()
			clean := s.shutdown
			s.mu.Unlock()
			if !clean {
				return fmt.Errorf("exit received before shutdown")
			}
			return nil
		}
		if len(m.ID) == 0 {
			s.notification(m)
			continue
		}
		if m.Method == "initialize" {
			s.initialize(m)
			continue
		}
		s.mu.Lock()
		if !s.initialized || s.shutdown {
			code, text := -32002, "server is not initialized"
			if s.shutdown {
				code, text = -32600, "server has shut down"
			}
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: code, Message: text})
			continue
		}
		if m.Method == "shutdown" {
			s.shutdown = true
			s.invalidate()
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, nil)
			continue
		}
		if m.Method != "textDocument/codeLens" {
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32601, Message: "method not found"})
			continue
		}
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: "invalid codeLens parameters"})
			continue
		}
		path, err := uriPath(params.TextDocument.URI)
		if err != nil {
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: err.Error()})
			continue
		}
		key := string(m.ID)
		if _, exists := s.requests[key]; exists {
			s.mu.Unlock()
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32600, Message: "duplicate pending request ID"})
			continue
		}
		ctx, cancel := context.WithCancel(s.generationCtx)
		s.requests[key] = cancel
		generation := s.generation
		s.workers.Add(1)
		s.mu.Unlock()
		go s.codeLenses(ctx, cancel, m.ID, path, generation)
	}
}

func (s *server) initialize(m message) {
	var params struct {
		RootURI          string `json:"rootUri"`
		RootPath         string `json:"rootPath"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
		InitializationOptions options `json:"initializationOptions"`
		Capabilities          struct {
			Workspace struct {
				CodeLens struct {
					RefreshSupport bool `json:"refreshSupport"`
				} `json:"codeLens"`
				DidChangeWatchedFiles struct {
					DynamicRegistration bool `json:"dynamicRegistration"`
				} `json:"didChangeWatchedFiles"`
			} `json:"workspace"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: err.Error()})
		return
	}
	var roots []string
	for _, folder := range params.WorkspaceFolders {
		path, err := uriPath(folder.URI)
		if err != nil {
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: err.Error()})
			return
		}
		roots = append(roots, path)
	}
	if len(roots) == 0 && params.RootURI != "" {
		path, err := uriPath(params.RootURI)
		if err != nil {
			_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: err.Error()})
			return
		}
		roots = append(roots, path)
	}
	if len(roots) == 0 && params.RootPath != "" {
		roots = append(roots, params.RootPath)
	}
	if len(roots) == 0 {
		_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32602, Message: "a local Go workspace folder is required"})
		return
	}
	s.mu.Lock()
	if s.initialized {
		s.mu.Unlock()
		_ = s.conn.reply(m.ID, nil, &rpcError{Code: -32600, Message: "server already initialized"})
		return
	}
	s.roots, s.options, s.refreshSupport, s.initialized = roots, params.InitializationOptions, params.Capabilities.Workspace.CodeLens.RefreshSupport, true
	s.mu.Unlock()
	_ = s.conn.reply(m.ID, map[string]any{
		"capabilities": map[string]any{
			"positionEncoding": "utf-16", "textDocumentSync": map[string]any{"openClose": true, "change": 2, "save": map[string]any{"includeText": true}},
			"codeLensProvider": map[string]any{"resolveProvider": false},
			"workspace":        map[string]any{"workspaceFolders": map[string]any{"supported": true, "changeNotifications": true}},
		},
		"serverInfo": map[string]any{"name": "go-impl-view", "version": "0.1.1"},
	}, nil)
	// Store registration capability separately: registration is sent only after
	// the initialized notification, when the LSP permits server requests.
	s.mu.Lock()
	s.watchSupport = params.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.mu.Unlock()
}

func (s *server) codeLenses(ctx context.Context, cancel context.CancelFunc, id json.RawMessage, path string, generation uint64) {
	defer s.workers.Done()
	defer cancel()
	result, err := s.getAnalysis(ctx, generation, path)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.requests, string(id))
	var failure *rpcError
	if generation != s.generation {
		failure = &rpcError{Code: -32801, Message: "workspace changed; request a fresh CodeLens"}
	} else if ctx.Err() != nil {
		failure = &rpcError{Code: -32800, Message: "request cancelled"}
	}
	lenses := []codeLens{}
	if failure == nil && err != nil {
		errorKey, keyErr := s.analysisKey(path)
		if keyErr != nil {
			errorKey = path
		}
		if !s.errorReported[errorKey] {
			if s.errorReported == nil {
				s.errorReported = map[string]bool{}
			}
			s.errorReported[errorKey] = true
			s.report(err)
		}
	}
	if failure == nil && result != nil {
		if len(result.warnings) > 0 {
			key, keyErr := s.analysisKey(path)
			if keyErr != nil {
				key = path
			}
			if !s.warningReported[key] {
				if s.warningReported == nil {
					s.warningReported = map[string]bool{}
				}
				s.warningReported[key] = true
				for _, warning := range result.warnings {
					_ = s.conn.notify("window/logMessage", map[string]any{"type": 2, "message": "go-impl-view: " + warning})
				}
			}
		}
		found := result.lenses[path]
		if found == nil {
			found = result.lenses[canonicalFile(path)]
		}
		if found != nil {
			lenses = found
		}
	}
	_ = s.conn.reply(id, lenses, failure)
}

// Caller holds mu. Include the nearest workspace boundary because two files
// in one module can have different nested go.work files.
func (s *server) analysisKey(path string) (string, error) {
	exists := func(path string) bool {
		if _, exists := s.documents[path]; exists {
			return true
		}
		name := filepath.Base(path)
		resolved := ""
		for candidate := range s.documents {
			if filepath.Base(candidate) != name {
				continue
			}
			if resolved == "" {
				resolved = canonicalFile(path)
			}
			if canonicalFile(candidate) == resolved {
				return true
			}
		}
		return false
	}
	mod, err := ancestorFile(path, "go.mod", exists)
	if err != nil {
		return "", err
	}
	key := filepath.Dir(path)
	if mod != "" {
		key = filepath.Dir(mod)
	}
	setting, explicit := s.options.Env["GOWORK"]
	if !explicit {
		setting = os.Getenv("GOWORK")
	}
	if setting != "" && setting != "auto" {
		return key, nil
	}
	work, err := ancestorFile(path, "go.work", exists)
	if err != nil {
		return "", err
	}
	if work != "" {
		key += "\x00" + work
	}
	return key, nil
}

func (s *server) getAnalysis(ctx context.Context, generation uint64, path string) (*analysis, error) {
	s.mu.Lock()
	if generation != s.generation || ctx.Err() != nil {
		s.mu.Unlock()
		return nil, context.Canceled
	}
	if s.configErr != nil {
		err := s.configErr
		s.mu.Unlock()
		return nil, err
	}
	key, err := s.analysisKey(path)
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	current := s.cache[key]
	if current == nil {
		current = &flight{done: make(chan struct{})}
		if s.cache == nil {
			s.cache = map[string]*flight{}
		}
		s.cache[key] = current
		snap := snapshot{path: path, roots: append([]string(nil), s.roots...), overlay: make(map[string][]byte, len(s.documents)), invalid: map[string]string{}, options: s.options}
		for path, doc := range s.documents {
			snap.overlay[path] = doc.text
			if doc.invalid != "" {
				snap.invalid[path] = doc.invalid
			}
		}
		generationCtx := s.generationCtx
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			current.result, current.err = analyze(generationCtx, snap)
			close(current.done)
			s.mu.Lock()
			defer s.mu.Unlock()
			if generation == s.generation && generationCtx.Err() == nil && s.refreshSupport {
				s.sequence++
				_ = s.conn.send(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("go-impl-refresh-%d", s.sequence), "method": "workspace/codeLens/refresh"})
			}
		}()
	}
	clearWhileLoading := s.refreshSupport
	s.mu.Unlock()
	if clearWhileLoading {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.done:
			return current.result, current.err
		default:
			return nil, nil
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-current.done:
		return current.result, current.err
	}
}

// Caller holds mu. Cancel before issuing refresh so no old count can be
// committed in response to a request after this document mutation.
func (s *server) changed() {
	s.invalidate()
	s.generation++
	s.generationCtx, s.invalidate = context.WithCancel(context.Background())
	s.cache = nil
	s.errorReported = nil
	s.warningReported = nil
	if s.refreshSupport {
		s.sequence++
		_ = s.conn.send(map[string]any{"jsonrpc": "2.0", "id": fmt.Sprintf("go-impl-refresh-%d", s.sequence), "method": "workspace/codeLens/refresh"})
	}
}
func (s *server) report(err error) {
	_ = s.conn.notify("window/logMessage", map[string]any{"type": 1, "message": "go-impl-view: " + err.Error()})
}
func (s *server) notification(m message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Method == "$/cancelRequest" {
		var params struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(m.Params, &params) == nil {
			if cancel := s.requests[string(params.ID)]; cancel != nil {
				cancel()
			}
		}
		return
	}
	if !s.initialized || s.shutdown {
		return
	}
	switch m.Method {
	case "initialized":
		if s.watchSupport {
			_ = s.conn.send(map[string]any{"jsonrpc": "2.0", "id": "go-impl-watch", "method": "client/registerCapability", "params": map[string]any{"registrations": []any{map[string]any{"id": "go-impl-files", "method": "workspace/didChangeWatchedFiles", "registerOptions": map[string]any{"watchers": []any{map[string]any{"globPattern": "**/*.{go,mod,sum,work}", "kind": 7}}}}}}})
		}
		if !s.refreshSupport {
			_ = s.conn.notify("window/logMessage", map[string]any{"type": 2, "message": "go-impl-view: client does not support workspace/codeLens/refresh; visible lenses may remain stale until the client requests them again. Server responses never reuse old workspace generations."})
		}
		if !s.watchSupport {
			_ = s.conn.notify("window/logMessage", map[string]any{"type": 2, "message": "go-impl-view: client does not support watched-file registration; external disk changes are only observed when the client sends didChangeWatchedFiles, didSave, or another workspace invalidation."})
		}
	case "textDocument/didOpen":
		var params struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Text    string `json:"text"`
				Version int    `json:"version"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.report(err)
			return
		}
		path, err := uriPath(params.TextDocument.URI)
		if err != nil {
			s.report(err)
			return
		}
		s.documents[path] = document{text: []byte(params.TextDocument.Text), version: params.TextDocument.Version}
		s.changed()
	case "textDocument/didChange":
		var params struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Range *sourceRange `json:"range"`
				Text  string       `json:"text"`
			} `json:"contentChanges"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.report(err)
			return
		}
		path, err := uriPath(params.TextDocument.URI)
		if err != nil {
			s.report(err)
			return
		}
		doc, exists := s.documents[path]
		if !exists {
			s.report(fmt.Errorf("change for unopened document %s", path))
			return
		}
		if params.TextDocument.Version <= doc.version {
			return
		}
		for _, change := range params.ContentChanges {
			if change.Range == nil {
				doc.text = []byte(change.Text)
				doc.invalid = ""
				continue
			}
			if doc.invalid != "" {
				continue
			}
			start, startErr := positionOffset(doc.text, change.Range.Start)
			end, endErr := positionOffset(doc.text, change.Range.End)
			if startErr != nil || endErr != nil || start > end {
				doc.invalid = "invalid incremental UTF-16 edit range"
				s.report(fmt.Errorf("%s: %s", path, doc.invalid))
				continue
			}
			text := make([]byte, 0, start+len(change.Text)+len(doc.text)-end)
			text = append(text, doc.text[:start]...)
			text = append(text, change.Text...)
			text = append(text, doc.text[end:]...)
			doc.text = text
		}
		doc.version = params.TextDocument.Version
		s.documents[path] = doc
		s.changed()
	case "textDocument/didClose", "textDocument/didSave":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.report(err)
			return
		}
		path, err := uriPath(params.TextDocument.URI)
		if err != nil {
			s.report(err)
			return
		}
		if m.Method == "textDocument/didClose" {
			delete(s.documents, path)
		} else if params.Text != nil {
			if doc, exists := s.documents[path]; exists {
				doc.text = []byte(*params.Text)
				doc.invalid = ""
				s.documents[path] = doc
			}
		}
		s.changed()
	case "workspace/didChangeWatchedFiles":
		s.changed()
	case "workspace/didChangeConfiguration":
		var params struct {
			Settings json.RawMessage `json:"settings"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.configErr = err
			s.changed()
			return
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal(params.Settings, &settings); err != nil {
			s.configErr = err
			s.changed()
			return
		}
		raw := params.Settings
		if nested, exists := settings["goImplView"]; exists {
			raw = nested
			settings = nil
			if err := json.Unmarshal(nested, &settings); err != nil {
				s.configErr = err
				s.changed()
				return
			}
		}
		// Notifications may contain only unrelated editor settings. Apply only
		// explicitly supplied options, preserving initializationOptions otherwise.
		var opts options
		s.configErr = json.Unmarshal(raw, &opts)
		if s.configErr == nil {
			if _, exists := settings["buildFlags"]; exists {
				s.options.BuildFlags = opts.BuildFlags
			}
			if _, exists := settings["env"]; exists {
				s.options.Env = opts.Env
			}
		}
		s.changed()
	case "workspace/didChangeWorkspaceFolders":
		var params struct {
			Event struct {
				Added []struct {
					URI string `json:"uri"`
				} `json:"added"`
				Removed []struct {
					URI string `json:"uri"`
				} `json:"removed"`
			} `json:"event"`
		}
		if err := json.Unmarshal(m.Params, &params); err != nil {
			s.report(err)
			return
		}
		for _, removed := range params.Event.Removed {
			path, err := uriPath(removed.URI)
			if err != nil {
				s.report(err)
				continue
			}
			for i := len(s.roots) - 1; i >= 0; i-- {
				if s.roots[i] == path {
					s.roots = append(s.roots[:i], s.roots[i+1:]...)
				}
			}
		}
		for _, added := range params.Event.Added {
			path, err := uriPath(added.URI)
			if err != nil {
				s.report(err)
				continue
			}
			s.roots = append(s.roots, path)
		}
		s.changed()
	case "$/setTrace", "$/logTrace":
	default:
		if !strings.HasPrefix(m.Method, "$/") {
			log.Printf("ignoring notification %s", m.Method)
		}
	}
}
