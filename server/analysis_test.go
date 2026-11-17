package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func fixtureSnapshot(root string) snapshot {
	return snapshot{path: filepath.Join(root, "main.go"), roots: []string{root}, overlay: map[string][]byte{}, options: options{Env: map[string]string{"GOWORK": "off", "GOFLAGS": "", "GOPROXY": "off"}}}
}
func typeLens(t *testing.T, result *analysis, path, source, name string) codeLens {
	t.Helper()
	index := strings.Index(source, "type "+name+" ")
	if index < 0 {
		index = strings.Index(source, "type "+name+"[")
	}
	if index < 0 {
		t.Fatalf("type %s not in fixture", name)
	}
	pos, err := offsetPosition([]byte(source), index+len("type "))
	if err != nil {
		t.Fatal(err)
	}
	for _, lens := range result.lenses[path] {
		if lens.Range.Start == pos {
			return lens
		}
	}
	t.Fatalf("no lens for %s: %#v", name, result.lenses[path])
	return codeLens{}
}

func assertNoTypeLens(t *testing.T, result *analysis, path, source, name string) {
	t.Helper()
	index := strings.Index(source, "type "+name+" ")
	if index < 0 {
		index = strings.Index(source, "type "+name+"[")
	}
	if index < 0 {
		t.Fatalf("type %s not in fixture", name)
	}
	pos, err := offsetPosition([]byte(source), index+len("type "))
	if err != nil {
		t.Fatal(err)
	}
	for _, lens := range result.lenses[path] {
		if lens.Range.Start == pos {
			t.Fatalf("unexpected lens for %s: %#v", name, lens)
		}
	}
}

func targetsOf(t *testing.T, lens codeLens) []location {
	t.Helper()
	if lens.Command == nil || lens.Command.Command != "editor.action.goToLocations" {
		t.Fatalf("invalid navigation command: %#v", lens.Command)
	}
	if len(lens.Command.Arguments) != 3 {
		t.Fatalf("unexpected arguments: %#v", lens.Command.Arguments)
	}
	targets, ok := lens.Command.Arguments[2].([]location)
	if !ok {
		t.Fatalf("invalid locations: %#v", lens.Command.Arguments[2])
	}
	return targets
}

func TestImplementationsAreUniqueConcreteDeclarationsIncludingTestsAndLocals(t *testing.T) {
	source := `package specimen

type I interface { M() }
type J interface { M() }
type Value struct{}
func (Value) M() {}
type Pointer struct{}
func (*Pointer) M() {}
type Embedded struct { Value }
type Alias = Value
type AliasI = I
type Distinct Value
func local() {
 type Local struct { Value }
 type LocalI interface { M() }
 _ = Local{}
}
`
	root := fixture(t, map[string]string{
		"go.mod":           "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":          source,
		"internal_test.go": "package specimen\ntype InternalTest struct{}\nfunc (InternalTest) M() {}\n",
		"external_test.go": "package specimen_test\ntype External struct{}\nfunc (External) M() {}\n",
		"other/other.go":   "package other\ntype Foreign struct{}\nfunc (Foreign) M() {}\n",
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	for _, name := range []string{"I", "J", "LocalI"} {
		lens := typeLens(t, result, path, source, name)
		if lens.Command.Title != "7 implementations" {
			t.Fatalf("%s: %s", name, lens.Command.Title)
		}
		targets := targetsOf(t, lens)
		if len(targets) != 7 {
			t.Fatalf("got %d targets", len(targets))
		}
		seen := map[string]bool{}
		for _, target := range targets {
			key := locationKey(target)
			if seen[key] {
				t.Fatalf("duplicate target %s", key)
			}
			seen[key] = true
		}
	}
	assertNoTypeLens(t, result, path, source, "Alias")
	assertNoTypeLens(t, result, path, source, "AliasI")
	for _, name := range []string{"Value", "Pointer", "Embedded", "Local"} {
		lens := typeLens(t, result, path, source, name)
		if lens.Command.Title != "3 interfaces" || len(targetsOf(t, lens)) != 3 {
			t.Fatalf("%s: %#v", name, lens)
		}
	}
	if lens := typeLens(t, result, path, source, "Distinct"); lens.Command.Title != "0 interfaces" || len(targetsOf(t, lens)) != 0 {
		t.Fatalf("derived struct unexpectedly inherited methods: %#v", lens)
	}
}

func TestAnalysisUsesUnsavedOverlaysAndBuildFlags(t *testing.T) {
	source := "package specimen\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"go.mod":    "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":   source,
		"impl.go":   "package specimen\ntype T struct{}\n",
		"tagged.go": "//go:build custom\n\npackage specimen\ntype Tagged struct{}\nfunc (Tagged) M() {}\n",
	})
	snap := fixtureSnapshot(root)
	snap.overlay[filepath.Join(root, "impl.go")] = []byte("package specimen\ntype T struct{}\nfunc (*T) M() {}\n")
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if title := typeLens(t, result, filepath.Join(root, "main.go"), source, "I").Command.Title; title != "1 implementation" {
		t.Fatal(title)
	}
	if lens := typeLens(t, result, filepath.Join(root, "impl.go"), string(snap.overlay[filepath.Join(root, "impl.go")]), "T"); lens.Command.Title != "1 interface" || len(targetsOf(t, lens)) != 1 {
		t.Fatalf("unsaved struct methods missing: %#v", lens)
	}
	snap.options.BuildFlags = []string{"-tags=custom"}
	result, err = analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if title := typeLens(t, result, filepath.Join(root, "main.go"), source, "I").Command.Title; title != "2 implementations" {
		t.Fatal(title)
	}
	tagged := "//go:build custom\n\npackage specimen\ntype Tagged struct{}\nfunc (Tagged) M() {}\n"
	if lens := typeLens(t, result, filepath.Join(root, "tagged.go"), tagged, "Tagged"); lens.Command.Title != "1 interface" || len(targetsOf(t, lens)) != 1 {
		t.Fatalf("build-tagged struct missing: %#v", lens)
	}
	delete(snap.overlay, filepath.Join(root, "impl.go"))
	snap.options.BuildFlags = nil
	result, err = analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, filepath.Join(root, "main.go"), source, "I")
	if lens.Command.Title != "0 implementations" || len(targetsOf(t, lens)) != 0 {
		t.Fatalf("zero lens: %#v", lens)
	}
	if lens := typeLens(t, result, filepath.Join(root, "impl.go"), "package specimen\ntype T struct{}\n", "T"); lens.Command.Title != "0 interfaces" || len(targetsOf(t, lens)) != 0 {
		t.Fatalf("removed method left stale interfaces: %#v", lens)
	}
	editedInterfaces := "package specimen\ntype I interface { M() }\ntype J interface {}\n"
	snap.overlay[filepath.Join(root, "main.go")] = []byte(editedInterfaces)
	result, err = analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	reverse := typeLens(t, result, filepath.Join(root, "impl.go"), "package specimen\ntype T struct{}\n", "T")
	j := typeLens(t, result, filepath.Join(root, "main.go"), editedInterfaces, "J")
	if targets := targetsOf(t, reverse); reverse.Command.Title != "1 interface" || len(targets) != 1 || targets[0].Range != j.Range {
		t.Fatalf("new interface missing after edit: %#v", reverse)
	}
}

func TestUnobservedGenericDeclarationsDoNotClaimExactCounts(t *testing.T) {
	source := `package specimen
type I interface { M() }
type A struct{}
func (A) M() {}
type Generic[T any] struct{}
func (Generic[T]) M() {}
type GenericI[T any] interface { Put(T) }
type Constraint interface { ~int | ~string }
func generic[T any]() { type Local struct{}; _ = Local{} }
`
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	lens := typeLens(t, result, path, source, "I")
	if lens.Command.Title != "1+ implementations" {
		t.Fatal(lens.Command.Title)
	}
	if len(targetsOf(t, lens)) != 1 {
		t.Fatalf("generic candidate leaked: %#v", targetsOf(t, lens))
	}
	reverse := typeLens(t, result, path, source, "A")
	if reverse.Command.Title != "1+ interfaces" {
		t.Fatal(reverse.Command.Title)
	}
	if targets := targetsOf(t, reverse); len(targets) != 1 || targets[0].Range != lens.Range {
		t.Fatalf("generic interface leaked: %#v", targets)
	}
	for _, name := range []string{"Generic", "GenericI", "Local"} {
		lens := typeLens(t, result, path, source, name)
		if !strings.Contains(lens.Command.Title, "0+") || len(targetsOf(t, lens)) != 0 {
			t.Fatalf("unobserved declaration %s claimed an exact count: %#v", name, lens)
		}
	}
	assertNoTypeLens(t, result, path, source, "Constraint")
}

func TestAnalysisErrorsNeverReturnCounts(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.mod":  "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go": "package specimen\ntype I interface { M() }\ntype T struct{}\nvar broken = missing\n",
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err == nil || result != nil {
		t.Fatalf("incomplete workspace returned counts: %#v, %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := analyze(ctx, fixtureSnapshot(root)); err == nil || result != nil {
		t.Fatalf("cancelled analysis returned counts: %#v, %v", result, err)
	}
}

func TestGoWorkModulesShareOneTypeGraph(t *testing.T) {
	root := fixture(t, map[string]string{
		"go.work":      "go 1.26.0\n\nuse (\n ./api\n ./impl\n)\n",
		"api/go.mod":   "module example.com/api\n\ngo 1.26.0\n",
		"api/api.go":   "package api\ntype Arg struct{}\ntype I interface { M(Arg) }\n",
		"impl/go.mod":  "module example.com/impl\n\ngo 1.26.0\n\nrequire example.com/api v0.0.0\n",
		"impl/impl.go": "package impl\nimport \"example.com/api\"\ntype T struct{}\nfunc (T) M(api.Arg) {}\n",
	})
	snap := fixtureSnapshot(root)
	snap.path = filepath.Join(root, "api", "api.go")
	snap.options.Env["GOWORK"] = filepath.Join(root, "go.work")
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	source := "package api\ntype Arg struct{}\ntype I interface { M(Arg) }\n"
	lens := typeLens(t, result, filepath.Join(root, "api", "api.go"), source, "I")
	if lens.Command.Title != "1 implementation" {
		t.Fatal(lens.Command.Title)
	}
	implementation := "package impl\nimport \"example.com/api\"\ntype T struct{}\nfunc (T) M(api.Arg) {}\n"
	reverse := typeLens(t, result, filepath.Join(root, "impl", "impl.go"), implementation, "T")
	if targets := targetsOf(t, reverse); reverse.Command.Title != "1 interface" || len(targets) != 1 || targets[0].Range != lens.Range || targets[0].URI != fileURI(filepath.Join(root, "api", "api.go")) {
		t.Fatalf("workspace interface missing: %#v", reverse)
	}
}

func TestGoWorkRejectsUnwatchedUseModules(t *testing.T) {
	source := "package api\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"api/go.work":  "go 1.26.0\n\nuse (\n .\n ../impl\n)\n",
		"api/go.mod":   "module example.com/api\n\ngo 1.26.0\n",
		"api/api.go":   source,
		"impl/go.mod":  "module example.com/impl\n\ngo 1.26.0\n",
		"impl/impl.go": "package impl\ntype T struct{}\nfunc (T) M() {}\n",
	})
	api := filepath.Join(root, "api")
	snap := fixtureSnapshot(api)
	snap.options.Env["GOWORK"] = filepath.Join(api, "go.work")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "go.work use module") || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("unwatched module returned counts or an unclear error: %#v, %v", result, err)
	}
	snap.roots = append(snap.roots, filepath.Join(root, "impl"))
	result, err := analyze(context.Background(), snap)
	if err != nil {
		t.Fatal(err)
	}
	if got := typeLens(t, result, filepath.Join(api, "api.go"), source, "I").Command.Title; got != "1 implementation" {
		t.Fatal(got)
	}
}

func TestGoWorkRejectsUnwatchedWorkFile(t *testing.T) {
	source := "package api\ntype I interface { M() }\n"
	root := fixture(t, map[string]string{
		"go.work":    "go 1.26.0\n\nuse ./api\n",
		"api/go.mod": "module example.com/api\n\ngo 1.26.0\n",
		"api/api.go": source,
	})
	api := filepath.Join(root, "api")
	snap := fixtureSnapshot(api)
	snap.options.Env["GOWORK"] = filepath.Join(root, "go.work")
	if result, err := analyze(context.Background(), snap); result != nil || err == nil || !strings.Contains(err.Error(), "go.work file") || !strings.Contains(err.Error(), "outside the opened workspace roots") {
		t.Fatalf("unwatched go.work returned counts or an unclear error: %#v, %v", result, err)
	}
	snap.roots = []string{root}
	if result, err := analyze(context.Background(), snap); result == nil || err != nil {
		t.Fatalf("opened go.work root was rejected: %#v, %v", result, err)
	}
}

func TestUnexportedMethodsArePackageScoped(t *testing.T) {
	source := "package specimen\ntype I interface { private() }\ntype T struct{}\nfunc (T) private() {}\n"
	root := fixture(t, map[string]string{
		"go.mod":         "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":        source,
		"other/other.go": "package other\ntype T struct{}\nfunc (T) private() {}\n",
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, filepath.Join(root, "main.go"), source, "I")
	if lens.Command.Title != "1 implementation" {
		t.Fatal(lens.Command.Title)
	}
}

func TestIntermediateTestVariantInterfacesUseTheirOwnTypeGraph(t *testing.T) {
	api := "package api\nimport \"example.com/specimen/shared\"\ntype I interface { M(shared.Arg) }\n"
	implementation := `package shared_test
import (
 "example.com/specimen/api"
 "example.com/specimen/shared"
)
type TestImplementation struct{}
func (TestImplementation) M(shared.Arg) {}
var _ api.I = TestImplementation{}
`
	root := fixture(t, map[string]string{
		"go.mod":                  "module example.com/specimen\n\ngo 1.26.0\n",
		"api/api.go":              api,
		"shared/shared.go":        "package shared\ntype Arg struct{}\n",
		"shared/internal_test.go": "package shared\ntype TestAnchor struct{}\n",
		"shared/external_test.go": implementation,
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, filepath.Join(root, "api", "api.go"), api, "I")
	if lens.Command.Title != "1 implementation" {
		t.Fatal(lens.Command.Title)
	}
	targets := targetsOf(t, lens)
	if len(targets) != 1 || targets[0].URI != fileURI(filepath.Join(root, "shared", "external_test.go")) {
		t.Fatalf("test-only implementation missing: %#v", targets)
	}
	reverse := typeLens(t, result, filepath.Join(root, "shared", "external_test.go"), implementation, "TestImplementation")
	if targets := targetsOf(t, reverse); reverse.Command.Title != "1 interface" || len(targets) != 1 || targets[0].Range != lens.Range || targets[0].URI != fileURI(filepath.Join(root, "api", "api.go")) {
		t.Fatalf("test graph interface missing: %#v", reverse)
	}
}

func TestDependencyTypesOutsideWorkspaceAreNotCounted(t *testing.T) {
	source := "package specimen\nimport \"example.com/dependency\"\ntype I interface { M(dependency.Arg) }\ntype T struct{}\nfunc (T) M(dependency.Arg) {}\n"
	root := fixture(t, map[string]string{
		"workspace/go.mod":  "module example.com/specimen\n\ngo 1.26.0\n\nrequire example.com/dependency v0.0.0\nreplace example.com/dependency => ../dependency\n",
		"workspace/main.go": source,
		"dependency/go.mod": "module example.com/dependency\n\ngo 1.26.0\n",
		"dependency/dep.go": "package dependency\ntype Arg struct{}\ntype External struct{}\nfunc (External) M(Arg) {}\n",
	})
	dir := filepath.Join(root, "workspace")
	result, err := analyze(context.Background(), fixtureSnapshot(dir))
	if err != nil {
		t.Fatal(err)
	}
	lens := typeLens(t, result, filepath.Join(dir, "main.go"), source, "I")
	if lens.Command.Title != "1 implementation" {
		t.Fatal(lens.Command.Title)
	}
	if target := targetsOf(t, lens)[0]; target.URI != fileURI(filepath.Join(dir, "main.go")) {
		t.Fatalf("dependency leaked into workspace candidates: %#v", target)
	}
}

func TestStructInterfacesIncludeValuePointerDerivedAndLocalDeclarations(t *testing.T) {
	source := `package specimen
type I interface { M() }
type J interface { M(); N() }
type Zero struct{}
type Value struct{}
func (Value) M() {}
type Pointer struct{}
func (*Pointer) M() {}
type Both struct{}
func (Both) M() {}
func (*Both) N() {}
type Derived Value
type Embedded struct { Value }
type Alias = Both
type AliasI = I
type Number int
func (Number) M() {}
type Slice []int
type Map map[string]int
type Function func()
type Channel chan int
type PointerType *Value
func local() {
 type Local struct { Value }
 _ = Local{}
}
`
	testSource := "package specimen\ntype TestI interface { M(); N() }\n"
	otherSource := "package other\ntype K interface { M(); N() }\n"
	root := fixture(t, map[string]string{
		"go.mod":           "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":          source,
		"internal_test.go": testSource,
		"other/other.go":   otherSource,
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	i := typeLens(t, result, path, source, "I")
	j := typeLens(t, result, path, source, "J")
	testI := typeLens(t, result, filepath.Join(root, "internal_test.go"), testSource, "TestI")
	k := typeLens(t, result, filepath.Join(root, "other", "other.go"), otherSource, "K")
	for _, test := range []struct {
		name    string
		title   string
		targets []location
	}{
		{"Zero", "0 interfaces", nil},
		{"Value", "1 interface", []location{{URI: fileURI(path), Range: i.Range}}},
		{"Pointer", "1 interface", []location{{URI: fileURI(path), Range: i.Range}}},
		{"Both", "4 interfaces", []location{
			{URI: fileURI(filepath.Join(root, "internal_test.go")), Range: testI.Range},
			{URI: fileURI(path), Range: i.Range},
			{URI: fileURI(path), Range: j.Range},
			{URI: fileURI(filepath.Join(root, "other", "other.go")), Range: k.Range},
		}},
		{"Derived", "0 interfaces", nil},
		{"Embedded", "1 interface", []location{{URI: fileURI(path), Range: i.Range}}},
		{"Local", "1 interface", []location{{URI: fileURI(path), Range: i.Range}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			lens := typeLens(t, result, path, source, test.name)
			if lens.Command.Title != test.title {
				t.Fatalf("got %q, want %q", lens.Command.Title, test.title)
			}
			targets := targetsOf(t, lens)
			if len(targets) != len(test.targets) {
				t.Fatalf("got targets %#v, want %#v", targets, test.targets)
			}
			for index, target := range targets {
				if target != test.targets[index] {
					t.Fatalf("target %d: got %#v, want %#v", index, target, test.targets[index])
				}
			}
		})
	}
	for _, name := range []string{"Alias", "AliasI", "Number", "Slice", "Map", "Function", "Channel", "PointerType"} {
		assertNoTypeLens(t, result, path, source, name)
	}
}

func TestConstraintsDoNotMakeStructInterfaceCountsIncomplete(t *testing.T) {
	source := `package specimen
type I interface { M() }
type T struct{}
func (T) M() {}
type Constraint interface { ~int | ~string }
type GenericConstraint[P any] interface { ~[]P }
type Comparable interface { comparable }
type MethodConstraint interface { M(); ~struct{} }
type Generic[P any] struct{}
`
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	lens := typeLens(t, result, path, source, "T")
	if lens.Command.Title != "1 interface" || len(targetsOf(t, lens)) != 1 {
		t.Fatalf("constraint or generic concrete type affected interfaces: %#v", lens)
	}
	for _, name := range []string{"Constraint", "GenericConstraint", "Comparable", "MethodConstraint"} {
		assertNoTypeLens(t, result, path, source, name)
	}
	generic := typeLens(t, result, path, source, "Generic")
	if generic.Command.Title != "0+ interfaces" || len(targetsOf(t, generic)) != 0 {
		t.Fatalf("unobserved generic struct: %#v", generic)
	}
}

func TestUnobservedGenericInterfacesMarkZeroCountsIncomplete(t *testing.T) {
	for _, source := range []string{
		"package specimen\ntype T struct{}\ntype GenericI[P any] interface { Put(P) }\n",
		"package specimen\ntype T struct{}\nfunc generic[P any]() { type LocalI interface { Put(P) }; var _ LocalI }\n",
		"package specimen\ntype T struct{}\ntype Generic[P any] struct{}\nfunc (Generic[P]) method() { type LocalI interface { Put(P) }; var _ LocalI }\n",
	} {
		root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
		result, err := analyze(context.Background(), fixtureSnapshot(root))
		if err != nil {
			t.Fatal(err)
		}
		lens := typeLens(t, result, filepath.Join(root, "main.go"), source, "T")
		if lens.Command.Title != "0+ interfaces" || len(targetsOf(t, lens)) != 0 {
			t.Fatalf("excluded generic interface claimed zero: %#v", lens)
		}
	}
}
