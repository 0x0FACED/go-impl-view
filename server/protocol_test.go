package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUTF16PositionsAndIncrementalRanges(t *testing.T) {
	text := []byte("a😀b\r\nЖz\n")
	cases := []struct {
		offset   int
		position position
	}{
		{0, position{0, 0}}, {1, position{0, 1}}, {5, position{0, 3}},
		{6, position{0, 4}}, {8, position{1, 0}}, {10, position{1, 1}}, {12, position{2, 0}},
	}
	for _, test := range cases {
		pos, err := offsetPosition(text, test.offset)
		if err != nil || pos != test.position {
			t.Fatalf("offset %d: %#v, %v", test.offset, pos, err)
		}
		offset, err := positionOffset(text, test.position)
		if err != nil || offset != test.offset {
			t.Fatalf("position %#v: %d, %v", test.position, offset, err)
		}
	}
	for _, pos := range []position{{0, 2}, {0, 8}, {3, 0}, {-1, 0}} {
		if _, err := positionOffset(text, pos); err == nil {
			t.Fatalf("accepted invalid position %#v", pos)
		}
	}
	if _, err := offsetPosition(text, 3); err == nil {
		t.Fatal("accepted split UTF-8 rune")
	}
}
func TestFileURIRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spaces # percent% Ж.go")
	if runtime.GOOS == "windows" {
		path = strings.ToUpper(path[:1]) + path[1:]
	}
	decoded, err := uriPath(fileURI(path))
	if err != nil || decoded != path {
		t.Fatalf("round trip %q: %q %v", path, decoded, err)
	}
	for _, uri := range []string{"https://example.com/file", "file://remote/path", "file:relative", "file://user@localhost/path", "file:///path?query", "file:///path#fragment"} {
		if _, err := uriPath(uri); err == nil {
			t.Fatalf("accepted %s", uri)
		}
	}
}

func TestWindowsLocalDriveFileURIs(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows drive paths require Windows filepath semantics")
	}
	path := `C:\workspace\spaces # percent% Ж.go`
	wantURI := "file:///C:/workspace/spaces%20%23%20percent%25%20%D0%96.go"
	if got := fileURI(path); got != wantURI {
		t.Fatalf("fileURI = %q, want %q", got, wantURI)
	}
	for _, uri := range []string{
		wantURI,
		"file:///c:/workspace/spaces%20%23%20percent%25%20%D0%96.go",
		"file:///c%3A/workspace/spaces%20%23%20percent%25%20%D0%96.go",
		"file://localhost/C:/workspace/spaces%20%23%20percent%25%20%D0%96.go",
	} {
		if got, err := uriPath(uri); err != nil || got != path {
			t.Fatalf("uriPath(%q) = %q, %v; want %q", uri, got, err, path)
		}
	}
	if got := fileURI(`c:\workspace\main.go`); got != "file:///C:/workspace/main.go" {
		t.Fatalf("lowercase drive was not normalized: %q", got)
	}
	for _, uri := range []string{"file:///workspace/main.go", "file:///C:relative", "file:////server/share/main.go", "file://server/share/main.go", "file:///1:/main.go", "file:///C:/path%5Cfile"} {
		if got, err := uriPath(uri); err == nil {
			t.Fatalf("accepted non-local or drive-relative URI %q as %q", uri, got)
		}
	}
}
func TestTransportFramesUTF8BytesAndSerialMessages(t *testing.T) {
	var wire bytes.Buffer
	writer := &connection{writer: &wire}
	if err := writer.notify("example", map[string]string{"text": "😀Ж"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.reply(json.RawMessage(`"request"`), []codeLens{}, nil); err != nil {
		t.Fatal(err)
	}
	reader := &connection{reader: bufio.NewReader(&wire)}
	first, err := reader.read()
	if err != nil || first.Method != "example" || !strings.Contains(string(first.Params), "😀Ж") {
		t.Fatalf("first frame %#v %v", first, err)
	}
	second, err := reader.read()
	if err != nil || string(second.ID) != `"request"` || second.Method != "" {
		t.Fatalf("second frame %#v %v", second, err)
	}
	if _, err := reader.read(); err != io.EOF {
		t.Fatal(err)
	}
}
func TestTransportRejectsInvalidFraming(t *testing.T) {
	for _, wire := range []string{
		"Content-Length: -1\r\n\r\n", "Content-Length: 1\r\nContent-Length: 1\r\n\r\nx",
		"Content-Length: 33554433\r\n\r\n", "X-Test: yes\r\n\r\n", "Content-Length: 1\r\n\r\nx",
	} {
		conn := &connection{reader: bufio.NewReader(strings.NewReader(wire))}
		if _, err := conn.read(); err == nil {
			t.Fatalf("accepted %q", wire)
		}
	}
}
