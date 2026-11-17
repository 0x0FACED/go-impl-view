package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}
type sourceRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}
type location struct {
	URI   string      `json:"uri"`
	Range sourceRange `json:"range"`
}
type command struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments"`
}
type codeLens struct {
	Range   sourceRange `json:"range"`
	Command *command    `json:"command,omitempty"`
}
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type connection struct {
	reader *bufio.Reader
	writer io.Writer
	mu     sync.Mutex
}

func (c *connection) read() (message, error) {
	length := -1
	headerBytes := 0
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return message{}, err
		}
		headerBytes += len(line)
		if headerBytes > 8192 {
			return message{}, fmt.Errorf("LSP headers exceed 8 KiB")
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return message{}, fmt.Errorf("malformed LSP header")
		}
		if strings.EqualFold(key, "Content-Length") {
			if length != -1 {
				return message{}, fmt.Errorf("duplicate Content-Length")
			}
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 || length > 32<<20 {
				return message{}, fmt.Errorf("invalid Content-Length")
			}
		}
	}
	if length < 0 {
		return message{}, fmt.Errorf("missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(c.reader, body); err != nil {
		return message{}, err
	}
	var m message
	if err := json.Unmarshal(body, &m); err != nil {
		return message{}, fmt.Errorf("invalid JSON-RPC: %w", err)
	}
	if m.JSONRPC != "2.0" {
		return message{}, fmt.Errorf("unsupported JSON-RPC version")
	}
	return m, nil
}

func (c *connection) send(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.writer, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.writer.Write(body)
	return err
}
func (c *connection) reply(id json.RawMessage, result any, failure *rpcError) error {
	if failure != nil {
		return c.send(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   *rpcError       `json:"error"`
		}{"2.0", id, failure})
	}
	return c.send(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{"2.0", id, result})
}
func (c *connection) notify(method string, params any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func fileURI(path string) string {
	path = filepath.ToSlash(path)
	if runtime.GOOS == "windows" && len(path) >= 2 && path[1] == ':' {
		path = "/" + strings.ToUpper(path[:1]) + path[1:]
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
func uriPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("only local file URIs are supported: %q", uri)
	}
	path := u.Path
	if runtime.GOOS == "windows" {
		if strings.Contains(path, `\`) {
			return "", fmt.Errorf("file URI must use slash separators: %q", uri)
		}
		// Local Windows URIs require /C:/..., not a rooted path on the
		// process's current drive. UNC/network shares are intentionally unsupported.
		if len(path) < 4 || path[0] != '/' || path[2] != ':' || path[3] != '/' || !((path[1] >= 'a' && path[1] <= 'z') || (path[1] >= 'A' && path[1] <= 'Z')) {
			return "", fmt.Errorf("file URI must name an absolute local Windows drive: %q", uri)
		}
		path = strings.ToUpper(path[1:2]) + path[2:]
	}
	path = filepath.FromSlash(path)
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("file URI is not absolute")
	}
	return filepath.Clean(path), nil
}

// Offsets are byte offsets; protocol positions always count UTF-16 code units.
func offsetPosition(text []byte, offset int) (position, error) {
	if offset < 0 || offset > len(text) {
		return position{}, fmt.Errorf("offset outside document")
	}
	p := position{}
	for i := 0; i < offset; {
		r, size := utf8.DecodeRune(text[i:])
		if i+size > offset {
			return position{}, fmt.Errorf("offset splits UTF-8 rune")
		}
		if r == '\n' {
			p.Line++
			p.Character = 0
		} else if r > 0xffff {
			p.Character += 2
		} else {
			p.Character++
		}
		i += size
	}
	return p, nil
}
func positionOffset(text []byte, p position) (int, error) {
	if p.Line < 0 || p.Character < 0 {
		return 0, fmt.Errorf("negative document position")
	}
	line := 0
	i := 0
	for line < p.Line && i < len(text) {
		if text[i] == '\n' {
			line++
		}
		i++
	}
	if line != p.Line {
		return 0, fmt.Errorf("line outside document")
	}
	units := 0
	for i < len(text) && text[i] != '\n' && !(text[i] == '\r' && i+1 < len(text) && text[i+1] == '\n') {
		if units == p.Character {
			return i, nil
		}
		r, size := utf8.DecodeRune(text[i:])
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > p.Character {
			return 0, fmt.Errorf("position splits UTF-16 surrogate pair")
		}
		units += width
		i += size
	}
	if units != p.Character {
		return 0, fmt.Errorf("character outside document line")
	}
	return i, nil
}
