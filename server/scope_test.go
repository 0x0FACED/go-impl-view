package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNestedModulesAnalyzeOnlyTheRequestedModule(t *testing.T) {
	parent := "package parent\nimport _ \"example.com/nested\"\ntype I interface { M() }\ntype T struct{}\nfunc (T) M() {}\n"
	nested := "package nested\ntype I interface { M() }\ntype T struct{}\nfunc (T) M() {}\n"
	root := fixture(t, map[string]string{
		"project/go.mod":         "module example.com/parent\n\ngo 1.26.0\n\nrequire example.com/nested v0.0.0\nreplace example.com/nested => ./nested\n",
		"project/main.go":        parent,
		"project/nested/go.mod":  "module example.com/nested\n\ngo 1.26.0\n",
		"project/nested/main.go": nested,
		"other/go.mod":           "module example.com/other\n\ngo 1.26.0\n",
		"other/main.go":          "package other\nvar broken = undefined\n",
		"unowned.go":             "package unowned\ntype I interface { M() }\n",
	})
	for _, test := range []struct {
		path   string
		source string
	}{
		{"project/main.go", parent},
		{"project/nested/main.go", nested},
	} {
		t.Run(test.path, func(t *testing.T) {
			snap := fixtureSnapshot(root)
			snap.path = filepath.Join(root, filepath.FromSlash(test.path))
			result, err := analyze(context.Background(), snap)
			if err != nil {
				t.Fatal(err)
			}
			lens := typeLens(t, result, snap.path, test.source, "I")
			if lens.Command.Title != "1 implementation" {
				t.Fatalf("independent module leaked into counts: %s", lens.Command.Title)
			}
			for _, target := range targetsOf(t, lens) {
				if target.URI != fileURI(snap.path) {
					t.Fatalf("foreign module target: %#v", target)
				}
			}
		})
	}
	for _, path := range []string{"other/main.go", "unowned.go"} {
		snap := fixtureSnapshot(root)
		snap.path = filepath.Join(root, filepath.FromSlash(path))
		if result, err := analyze(context.Background(), snap); result != nil || err == nil {
			t.Fatalf("invalid document %s returned counts: %#v, %v", path, result, err)
		}
	}
}

func TestGoWorkDoesNotAbsorbUnlistedNestedModules(t *testing.T) {
	source := "package api\ntype I interface { M() }\ntype T struct{}\nfunc (T) M() {}\n"
	root := fixture(t, map[string]string{
		"go.work":           "go 1.26.0\n\nuse (\n ./api\n ./impl\n)\n",
		"api/go.mod":        "module example.com/api\n\ngo 1.26.0\n",
		"api/main.go":       source,
		"api/child/go.mod":  "module example.com/child\n\ngo 1.26.0\n",
		"api/child/main.go": source,
		"impl/go.mod":       "module example.com/impl\n\ngo 1.26.0\n",
		"impl/main.go":      "package impl\ntype T struct{}\nfunc (T) M() {}\n",
	})
	for _, test := range []struct {
		file string
		want string
	}{
		{"api/main.go", "2 implementations"},
		{"api/child/main.go", "1 implementation"},
	} {
		snap := fixtureSnapshot(root)
		snap.options.Env["GOWORK"] = ""
		snap.path = filepath.Join(root, filepath.FromSlash(test.file))
		result, err := analyze(context.Background(), snap)
		if err != nil {
			t.Fatal(err)
		}
		if got := typeLens(t, result, snap.path, source, "I").Command.Title; got != test.want {
			t.Fatalf("%s: got %s, want %s", test.file, got, test.want)
		}
	}
}

func TestLocalDependencyOverlaysRemainAvailableWithoutLeakingCandidates(t *testing.T) {
	source := "package main\nimport \"example.com/dep\"\ntype I interface { M(dep.Arg) }\ntype T struct{}\nfunc (T) M(string) {}\nfunc main() {}\n"
	root := fixture(t, map[string]string{
		"project/go.mod":  "module example.com/project\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\nreplace example.com/dep => ../dep\n",
		"project/main.go": source,
		"dep/go.mod":      "module example.com/dep\n\ngo 1.26.0\n",
		"dep/dep.go":      "package dep\ntype Arg int\n",
		"other/go.mod":    "module example.com/other\n\ngo 1.26.0\n",
		"other/main.go":   "package other\n",
	})
	project := filepath.Join(root, "project")
	snap := fixtureSnapshot(project)
	dep := filepath.Join(root, "dep", "dep.go")
	other := filepath.Join(root, "other", "main.go")
	snap.overlay[dep] = []byte("package dep\ntype Arg = string\ntype External struct{}\nfunc (External) M(Arg) {}\n")
	snap.overlay[other] = []byte("invalid unrelated Go text")
	snap.invalid = map[string]string{other: "invalid incremental UTF-16 edit range"}
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, snap.path, source, "I")
	if lens.Command.Title != "1 implementation" || targetsOf(t, lens)[0].URI != fileURI(snap.path) {
		t.Fatalf("dependency overlay was ignored or dependency candidate leaked: %#v", lens)
	}
	snap.invalid[dep] = "invalid incremental UTF-16 edit range"
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "out of sync") {
		t.Fatalf("invalid loaded dependency returned counts: %#v, %v", result, err)
	}
}

func TestGoWorkOverlaysRebaseUseAndReplacePaths(t *testing.T) {
	source := "package api\nimport \"example.com/dep\"\ntype I interface { M(dep.Arg) }\n"
	initial := "go 1.26.0\n\nuse ./api\nreplace example.com/dep => ./dep\n"
	updated := "go 1.26.0\n\nuse (\n ./api\n ./impl\n)\nreplace example.com/dep => ./dep\n"
	for _, existing := range []bool{true, false} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"api/go.mod":   "module example.com/api\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\n",
				"api/api.go":   source,
				"impl/go.mod":  "module example.com/impl\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\n",
				"impl/impl.go": "package impl\nimport \"example.com/dep\"\ntype T struct{}\nfunc (T) M(dep.Arg) {}\n",
				"dep/go.mod":   "module example.com/dep\n\ngo 1.26.0\n",
				"dep/dep.go":   "package dep\ntype Arg struct{}\ntype External struct{}\nfunc (External) M(Arg) {}\n",
			}
			if existing {
				files["go.work"] = initial
			}
			root := fixture(t, files)
			snap := fixtureSnapshot(root)
			snap.path = filepath.Join(root, "api", "api.go")
			snap.options.Env["GOWORK"] = ""
			work := filepath.Join(root, "go.work")
			for _, test := range []struct {
				work string
				want string
			}{
				{updated, "1 implementation"},
				{initial, "0 implementations"},
			} {
				snap.overlay[work] = []byte(test.work)
				result, err := analyze(context.Background(), snap)
				if err != nil {
					t.Fatal(err)
				}
				if got := typeLens(t, result, snap.path, source, "I").Command.Title; got != test.want {
					t.Fatalf("got %s, want %s", got, test.want)
				}
			}
		})
	}
}

func requestDocumentLenses(t *testing.T, s *server, output *capturedOutput, path string) ([]codeLens, []string) {
	t.Helper()
	before := len(decodedFrames(t, output.bytes()))
	ctx, cancel := context.WithCancel(s.generationCtx)
	s.workers.Add(1)
	s.codeLenses(ctx, cancel, json.RawMessage("100"), path, s.generation)
	var lenses []codeLens
	var errors []string
	for _, frame := range decodedFrames(t, output.bytes())[before:] {
		if failure, exists := frame["error"]; exists {
			t.Fatalf("codeLens request failed: %s", failure)
		}
		if raw, exists := frame["result"]; exists {
			if err := json.Unmarshal(raw, &lenses); err != nil {
				t.Fatal(err)
			}
		}
		if string(frame["method"]) == `"window/logMessage"` {
			var params struct {
				Type    int    `json:"type"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(frame["params"], &params); err != nil {
				t.Fatal(err)
			}
			if params.Type == 1 {
				errors = append(errors, params.Message)
			}
		}
	}
	return lenses, errors
}

func TestDocumentRequestsRespectNestedWorkspacesInOneModule(t *testing.T) {
	source := "package api\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"api/go.mod":      "module example.com/api\n\ngo 1.26.0\n",
		"api/api.go":      source,
		"api/sub/sub.go":  "package sub\ntype I interface { M() }\n",
		"api/sub/go.work": "go 1.26.0\n\nuse (\n ..\n ../../impl\n)\n",
		"impl/go.mod":     "module example.com/impl\n\ngo 1.26.0\n",
		"impl/impl.go":    "package impl\ntype T struct{}\nfunc (T) M() {}\n",
	})
	s, output := testServer(t, root)
	s.options.Env["GOWORK"] = ""
	for _, test := range []struct {
		file string
		want string
	}{
		{"api/api.go", "0 implementations"},
		{"api/sub/sub.go", "1 implementation"},
		{"api/api.go", "0 implementations"},
	} {
		path := filepath.Join(root, filepath.FromSlash(test.file))
		lenses, errors := requestDocumentLenses(t, s, output, path)
		if len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != test.want {
			t.Fatalf("%s: lenses %#v, errors %v, want %s", test.file, lenses, errors, test.want)
		}
	}
}

func TestModuleBoundaryChangesInvalidateDocumentResults(t *testing.T) {
	root := fixture(t, map[string]string{
		"project/go.mod":         "module example.com/project\n\ngo 1.26.0\n",
		"project/api.go":         "package project\ntype I interface { M() }\n",
		"project/nested/impl.go": "package nested\ntype T struct{}\nfunc (T) M() {}\n",
	})
	s, output := testServer(t, root)
	api := filepath.Join(root, "project", "api.go")
	impl := filepath.Join(root, "project", "nested", "impl.go")
	assertTitle := func(path, want string) {
		t.Helper()
		lenses, errors := requestDocumentLenses(t, s, output, path)
		if len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != want {
			t.Fatalf("got lenses %#v, errors %v; want %s", lenses, errors, want)
		}
	}
	assertTitle(api, "1 implementation")
	assertTitle(impl, "1 interface")
	mod := filepath.Join(root, "project", "nested", "go.mod")
	if err := os.WriteFile(mod, []byte("module example.com/nested\n\ngo 1.26.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": fileURI(mod), "type": 1}}}))
	assertTitle(api, "0 implementations")
	assertTitle(impl, "0 interfaces")
	s.notification(notificationFor(t, "workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"removed": []any{map[string]any{"uri": fileURI(root)}}}}))
	if lenses, errors := requestDocumentLenses(t, s, output, api); len(lenses) != 0 || len(errors) != 1 || !strings.Contains(errors[0], "outside the opened workspace roots") {
		t.Fatalf("removed root reused results: %#v, %v", lenses, errors)
	}
}

func TestInvalidDocumentsAndAnalysisErrorsAreIsolatedByModule(t *testing.T) {
	root := fixture(t, map[string]string{
		"good/go.mod":   "module example.com/good\n\ngo 1.26.0\n",
		"good/main.go":  "package good\ntype I interface { M() }\n",
		"bad/go.mod":    "module example.com/bad\n\ngo 1.26.0\n",
		"bad/main.go":   "package bad\n// 😀\ntype I interface { M() }\n",
		"other/go.mod":  "module example.com/other\n\ngo 1.26.0\n",
		"other/main.go": "package other\nvar broken = missing\n",
	})
	s, output := testServer(t, root)
	bad := filepath.Join(root, "bad", "main.go")
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(bad), "version": 1, "text": "package bad\n// 😀\ntype I interface { M() }\n"}}))
	s.notification(notificationFor(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(bad), "version": 2}, "contentChanges": []any{map[string]any{"range": sourceRange{Start: position{1, 4}, End: position{1, 4}}, "text": "x"}}}))
	if lenses, errors := requestDocumentLenses(t, s, output, bad); len(lenses) != 0 || len(errors) != 1 || !strings.Contains(errors[0], "out of sync") {
		t.Fatalf("out-of-sync module returned counts: %#v, %v", lenses, errors)
	}
	good := filepath.Join(root, "good", "main.go")
	if lenses, errors := requestDocumentLenses(t, s, output, good); len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != "0 implementations" {
		t.Fatalf("unrelated invalid module blocked good document: %#v, %v", lenses, errors)
	}
	other := filepath.Join(root, "other", "main.go")
	if lenses, errors := requestDocumentLenses(t, s, output, other); len(lenses) != 0 || len(errors) != 1 || !strings.Contains(errors[0], "workspace analysis incomplete") {
		t.Fatalf("second module error was suppressed: %#v, %v", lenses, errors)
	}
	s.notification(notificationFor(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": fileURI(bad), "version": 3}, "contentChanges": []any{map[string]any{"text": "package bad\ntype I interface { M() }\n"}}}))
	if lenses, errors := requestDocumentLenses(t, s, output, bad); len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != "0 implementations" {
		t.Fatalf("full edit did not recover module: %#v, %v", lenses, errors)
	}
}

func TestGoModOverlaysChangeLocalDependencySelection(t *testing.T) {
	source := "package main\nimport \"example.com/dep\"\ntype I interface { M(dep.Arg) }\ntype T struct{}\nfunc (T) M(string) {}\nfunc main() {}\n"
	module := "module example.com/project\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\nreplace example.com/dep => ../first\n"
	root := fixture(t, map[string]string{
		"project/go.mod":  module,
		"project/main.go": source,
		"first/go.mod":    "module example.com/dep\n\ngo 1.26.0\n",
		"first/dep.go":    "package dep\ntype Arg int\n",
		"second/go.mod":   "module example.com/dep\n\ngo 1.26.0\n",
		"second/dep.go":   "package dep\ntype Arg = string\n",
	})
	project := filepath.Join(root, "project")
	s, output := testServer(t, project)
	path := filepath.Join(project, "main.go")
	assertTitle := func(want string) {
		t.Helper()
		lenses, errors := requestDocumentLenses(t, s, output, path)
		found := typeLens(t, &analysis{lenses: map[string][]codeLens{path: lenses}}, path, source, "I")
		if len(errors) != 0 || found.Command.Title != want {
			t.Fatalf("got %s, errors %v; want %s", found.Command.Title, errors, want)
		}
	}
	assertTitle("0 implementations")
	modURI := fileURI(filepath.Join(project, "go.mod"))
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": modURI, "version": 1, "text": strings.Replace(module, "../first", "../second", 1)}}))
	assertTitle("1 implementation")
	s.notification(notificationFor(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": modURI}}))
	assertTitle("0 implementations")
}

func TestSymlinkedRootsKeepOverlaysAndDocumentLocations(t *testing.T) {
	source := "package specimen\ntype I interface { M() }\ntype T struct{}\n"
	root := fixture(t, map[string]string{
		"go.mod":  "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go": source,
	})
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	snap := fixtureSnapshot(alias)
	source += "func (T) M() {}\n"
	snap.overlay[snap.path] = []byte(source)
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, snap.path, source, "I")
	if lens.Command.Title != "1 implementation" || targetsOf(t, lens)[0].URI != fileURI(snap.path) {
		t.Fatalf("symlink URI or overlay was lost: %#v", lens)
	}
}

func TestWorkspaceSymlinksCannotEscapeWatchedRoots(t *testing.T) {
	external := fixture(t, map[string]string{
		"go.mod":  "module example.com/external\n\ngo 1.26.0\n",
		"go.work": "go 1.26.0\n\nuse .\n",
		"main.go": "package external\n",
	})
	root := fixture(t, map[string]string{
		"go.work":     "go 1.26.0\n\nuse (\n ./api\n ./external\n)\n",
		"api/go.mod":  "module example.com/api\n\ngo 1.26.0\n",
		"api/main.go": "package api\ntype I interface { M() }\n",
	})
	if err := os.Symlink(external, filepath.Join(root, "external")); err != nil {
		t.Fatal(err)
	}
	snap := fixtureSnapshot(root)
	snap.path = filepath.Join(root, "api", "main.go")
	snap.options.Env["GOWORK"] = filepath.Join(root, "go.work")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("unwatched use symlink returned counts: %#v, %v", result, err)
	}
	workAlias := filepath.Join(root, "linked.work")
	if err := os.Symlink(filepath.Join(external, "go.work"), workAlias); err != nil {
		t.Fatal(err)
	}
	snap.options.Env["GOWORK"] = workAlias
	snap.overlay[workAlias] = []byte("go 1.26.0\n\nuse ./api\n")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("overlay bypassed go.work symlink protection: %#v, %v", result, err)
	}
}

func TestIndependentWorkspaceFoldersDoNotShareResultsOrFailures(t *testing.T) {
	root := fixture(t, map[string]string{
		"good/go.mod":     "module example.com/good\n\ngo 1.26.0\n",
		"good/main.go":    "package good\ntype I interface { M() }\n",
		"bad/go.mod":      "invalid module contents\n",
		"bad/main.go":     "package bad\n",
		"good/sub/sub.go": "package sub\ntype I interface { M() }\n",
	})
	snap := fixtureSnapshot(filepath.Join(root, "good"))
	snap.roots = append(snap.roots, filepath.Join(root, "bad"))
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if lens := typeLens(t, result, snap.path, "package good\ntype I interface { M() }\n", "I"); lens.Command.Title != "0 implementations" {
		t.Fatal(lens.Command.Title)
	}
	snap.path = filepath.Join(root, "bad", "main.go")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil {
		t.Fatalf("broken workspace folder returned counts: %#v, %v", result, err)
	}
	sub := filepath.Join(root, "good", "sub")
	snap = fixtureSnapshot(sub)
	snap.path = filepath.Join(sub, "sub.go")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("unwatched parent module returned exact counts: %#v, %v", result, err)
	}
}

func TestNewPackageOverlaysCanIntroduceMissingDirectories(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod": "module example.com/specimen\n\ngo 1.26.0\n",
		"api.go": "package specimen\ntype I interface { M() }\n",
	})
	alias := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	s, output := testServer(t, alias)
	path := filepath.Join(alias, "new", "deep", "impl.go")
	s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": fileURI(path), "version": 1, "text": "package impl\ntype T struct{}\nfunc (T) M() {}\n"}}))
	for _, test := range []struct {
		path string
		want string
	}{
		{path, "1 interface"},
		{filepath.Join(alias, "api.go"), "1 implementation"},
	} {
		lenses, errors := requestDocumentLenses(t, s, output, test.path)
		if len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != test.want {
			t.Fatalf("new package overlay: lenses %#v, errors %v; want %s", lenses, errors, test.want)
		}
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("analysis materialized a source directory: %v", err)
	}
	s.notification(notificationFor(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": fileURI(path)}}))
	if lenses, errors := requestDocumentLenses(t, s, output, filepath.Join(alias, "api.go")); len(errors) != 0 || len(lenses) != 1 || lenses[0].Command.Title != "0 implementations" {
		t.Fatalf("closed new package overlay survived: %#v, %v", lenses, errors)
	}
}

func TestGoWorkUseSymlinksLoadTheirDeclaredPaths(t *testing.T) {
	source := "package api\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"go.work":           "go 1.26.0\n\nuse (\n ./api\n ./impl\n)\n",
		"real/api/go.mod":   "module example.com/api\n\ngo 1.26.0\n",
		"real/api/api.go":   source,
		"real/impl/go.mod":  "module example.com/impl\n\ngo 1.26.0\n",
		"real/impl/impl.go": "package impl\ntype T struct{}\nfunc (T) M() {}\n",
	})
	for _, name := range []string{"api", "impl"} {
		if err := os.Symlink(filepath.Join(root, "real", name), filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	snap := fixtureSnapshot(root)
	snap.path = filepath.Join(root, "api", "api.go")
	snap.options.Env["GOWORK"] = filepath.Join(root, "go.work")
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, snap.path, source, "I")
	if lens.Command.Title != "1 implementation" || targetsOf(t, lens)[0].URI != fileURI(filepath.Join(root, "impl", "impl.go")) {
		t.Fatalf("workspace use alias was lost: %#v", lens)
	}
}

func TestWorkspaceVendorOverlaysKeepTheOriginalVendorBase(t *testing.T) {
	work := "go 1.26.0\n\nuse ./api\nreplace example.com/dep => ./dep\n"
	source := "package api\nimport \"example.com/dep\"\ntype I interface { M(dep.Arg) }\ntype T struct{}\nfunc (T) M(string) {}\n"
	for _, existing := range []bool{true, false} {
		name := "new"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			files := map[string]string{
				"api/go.mod":                    "module example.com/api\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\n",
				"api/api.go":                    source,
				"dep/go.mod":                    "module example.com/dep\n\ngo 1.26.0\n",
				"dep/dep.go":                    "package dep\ntype Arg int\n",
				"vendor/modules.txt":            "## workspace\n# example.com/dep v0.0.0 => ./dep\n## explicit; go 1.26.0\nexample.com/dep\n# example.com/dep => ./dep\n",
				"vendor/example.com/dep/dep.go": "package dep\ntype Arg = string\n",
			}
			if existing {
				files["go.work"] = work
			}
			root := fixture(t, files)
			if !existing {
				snap := fixtureSnapshot(root)
				snap.path = filepath.Join(root, "api", "api.go")
				snap.options.Env["GOWORK"] = "auto"
				snap.options.BuildFlags = []string{"-mod=vendor"}
				snap.overlay[filepath.Join(root, "go.work")] = []byte(work)
				if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "save go.work and synchronize vendor") {
					t.Fatalf("materialized vendor mismatch returned counts or an unclear error: %#v, %v", result, err)
				}
				return
			}
			s, output := testServer(t, root)
			s.options.Env["GOWORK"] = "auto"
			s.options.BuildFlags = []string{"-mod=vendor"}
			path := filepath.Join(root, "api", "api.go")
			assertTitle := func(want string) {
				t.Helper()
				lenses, errors := requestDocumentLenses(t, s, output, path)
				lens := typeLens(t, &analysis{lenses: map[string][]codeLens{path: lenses}}, path, source, "I")
				if len(errors) != 0 || lens.Command.Title != want {
					t.Fatalf("vendored overlay: got %s, errors %v; want %s", lens.Command.Title, errors, want)
				}
			}
			assertTitle("1 implementation")
			workURI := fileURI(filepath.Join(root, "go.work"))
			s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": workURI, "version": 1, "text": work}}))
			assertTitle("1 implementation")
			s.notification(notificationFor(t, "textDocument/didChange", map[string]any{"textDocument": map[string]any{"uri": workURI, "version": 2}, "contentChanges": []any{map[string]any{"text": work + "// unsaved comment\n"}}}))
			assertTitle("1 implementation")
			depURI := fileURI(filepath.Join(root, "vendor", "example.com", "dep", "dep.go"))
			s.notification(notificationFor(t, "textDocument/didOpen", map[string]any{"textDocument": map[string]any{"uri": depURI, "version": 1, "text": "package dep\ntype Arg int\n"}}))
			assertTitle("0 implementations")
			s.notification(notificationFor(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": depURI}}))
			assertTitle("1 implementation")
			s.notification(notificationFor(t, "textDocument/didClose", map[string]any{"textDocument": map[string]any{"uri": workURI}}))
			assertTitle("1 implementation")
		})
	}
}

func TestExternalLocalDependenciesReportTheirFreshnessBoundary(t *testing.T) {
	source := "package project\nimport \"example.com/dep\"\ntype I interface { M(dep.Arg) }\ntype T struct{}\nfunc (T) M(string) {}\n"
	root := fixture(t, map[string]string{
		"project/go.mod":  "module example.com/project\n\ngo 1.26.0\n\nrequire example.com/dep v0.0.0\nreplace example.com/dep => ../dep\n",
		"project/main.go": source,
		"dep/go.mod":      "module example.com/dep\n\ngo 1.26.0\n",
		"dep/dep.go":      "package dep\ntype Arg int\n",
	})
	project := filepath.Join(root, "project")
	s, output := testServer(t, project)
	path := filepath.Join(project, "main.go")
	assertTitle := func(want string) {
		t.Helper()
		lenses, errors := requestDocumentLenses(t, s, output, path)
		lens := typeLens(t, &analysis{lenses: map[string][]codeLens{path: lenses}}, path, source, "I")
		if len(errors) != 0 || lens.Command.Title != want {
			t.Fatalf("got %s, errors %v; want %s", lens.Command.Title, errors, want)
		}
	}
	warnings := func() int {
		t.Helper()
		count := 0
		for _, frame := range decodedFrames(t, output.bytes()) {
			if string(frame["method"]) == `"window/logMessage"` {
				var params struct {
					Type    int    `json:"type"`
					Message string `json:"message"`
				}
				if err := json.Unmarshal(frame["params"], &params); err != nil {
					t.Fatal(err)
				}
				if params.Type == 2 && strings.Contains(params.Message, "local replace dependencies") {
					count++
				}
			}
		}
		return count
	}
	assertTitle("0 implementations")
	assertTitle("0 implementations")
	if got := warnings(); got != 1 {
		t.Fatalf("expected one scope warning, got %d", got)
	}
	dep := filepath.Join(root, "dep", "dep.go")
	if err := os.WriteFile(dep, []byte("package dep\ntype Arg = string\n"), 0644); err != nil {
		t.Fatal(err)
	}
	s.notification(notificationFor(t, "workspace/didChangeWatchedFiles", map[string]any{"changes": []any{map[string]any{"uri": fileURI(dep), "type": 2}}}))
	assertTitle("1 implementation")
	if got := warnings(); got != 2 {
		t.Fatalf("new generation did not report its freshness boundary: %d", got)
	}
	s.notification(notificationFor(t, "workspace/didChangeWorkspaceFolders", map[string]any{"event": map[string]any{"added": []any{map[string]any{"uri": fileURI(filepath.Join(root, "dep"))}}}}))
	assertTitle("1 implementation")
	if got := warnings(); got != 2 {
		t.Fatalf("covered dependency still reported as unwatched: %d", got)
	}
}
