package main

import (
	"context"
	"go/version"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assertTypeTargets(t *testing.T, result *analysis, path, source, name string, names ...string) codeLens {
	t.Helper()
	lens := typeLens(t, result, path, source, name)
	want := make(map[string]bool, len(names))
	for _, targetName := range names {
		target := typeLens(t, result, path, source, targetName)
		want[locationKey(location{URI: fileURI(path), Range: target.Range})] = true
	}
	targets := targetsOf(t, lens)
	if len(targets) != len(want) {
		t.Fatalf("%s: got targets %#v, want declarations %v", name, targets, names)
	}
	for _, target := range targets {
		key := locationKey(target)
		if !want[key] {
			t.Fatalf("%s: unexpected or duplicate target %#v", name, target)
		}
		delete(want, key)
	}
	return lens
}

func TestObservedGenericValuePointerAndInterfaceInstantiations(t *testing.T) {
	source := `package specimen
type IntI interface { Put(int) }
type StringI interface { Put(string) }
type GenericI[T any] interface { Put(T) }
type UnobservedI[T any] interface { Put(bool) }
type Value[T any] struct{}
func (Value[T]) Put(T) {}
type Pointer[T any] struct{}
func (*Pointer[T]) Put(T) {}
type Unobserved[T any] struct{}
func (Unobserved[T]) Put(int) {}
type Concrete struct{}
func (Concrete) Put(int) {}
type Wrong struct{}
func (Wrong) Put(bool) {}
type Alias = Value[int]
type AliasI = GenericI[int]
type Constraint[T any] interface { ~[]T }
var _ Value[int]
var _ Value[string]
var _ Value[int]
var _ *Pointer[int]
var _ GenericI[int]
var _ GenericI[string]
var _ GenericI[int]
`
	root := fixture(t, map[string]string{
		"go.mod":       "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":      source,
		"main_test.go": "package specimen\nvar _ Value[int]\nvar _ *Pointer[int]\nvar _ GenericI[int]\n",
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	for _, test := range []struct {
		name string
		want []string
	}{
		{"IntI", []string{"Value", "Pointer", "Concrete"}},
		{"StringI", []string{"Value"}},
		{"GenericI", []string{"Value", "Pointer", "Concrete"}},
		{"Value", []string{"IntI", "StringI", "GenericI"}},
		{"Pointer", []string{"IntI", "GenericI"}},
		{"Concrete", []string{"IntI", "GenericI"}},
		{"Wrong", nil},
		{"Unobserved", nil},
		{"UnobservedI", nil},
	} {
		lens := assertTypeTargets(t, result, path, source, test.name, test.want...)
		if !strings.Contains(lens.Command.Title, "+") {
			t.Fatalf("%s claimed an exact generic count: %q", test.name, lens.Command.Title)
		}
	}
	for _, name := range []string{"Alias", "AliasI", "Constraint"} {
		assertNoTypeLens(t, result, path, source, name)
	}
}

func TestGenericObservationsIncludeInferredAndNestedInstances(t *testing.T) {
	source := `package specimen
type GenericI[T any] interface { Get() T }
type Value[T any] struct{}
func (Value[T]) Get() T { var zero T; return zero }
type Pointer[T any] struct{}
func (*Pointer[T]) Get() T { var zero T; return zero }
type Outer[T any] struct { Nested Value[T] }
func (Outer[T]) NestedPointer() *Pointer[T] { return nil }
func inferred[T any](value T) Value[T] { return Value[T]{} }
var _ = inferred(1)
var _ Outer[string]
var _ GenericI[int]
var _ GenericI[string]
var _ Value[map[string]func(*int) []int]
var _ GenericI[map[string]func(*int) []int]
`
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	assertTypeTargets(t, result, path, source, "GenericI", "Value", "Pointer")
	assertTypeTargets(t, result, path, source, "Value", "GenericI")
	assertTypeTargets(t, result, path, source, "Pointer", "GenericI")
}

func TestFreeParametersAndLocalGenericDeclarationsAreNotConcreteEvidence(t *testing.T) {
	source := `package specimen
type Any interface{}
type GenericI[T any] interface { Get() T }
type Value[T any] struct{}
func (Value[T]) Get() T { var zero T; return zero }
func free[T any]() {
 var _ Value[T]
 var _ GenericI[T]
 var _ Value[map[string]func(*T) []T]
 var _ GenericI[map[string]func(*T) []T]
 type Local struct{}
 type Captured struct { Field T }
 type LocalI interface{}
 type CapturedI interface { Get() T }
 var _ Local
 var _ Captured
 var _ LocalI
 var _ CapturedI
 var _ Value[Local]
 var _ GenericI[Local]
 var _ Value[map[*Local]bool]
 var _ GenericI[map[*Local]bool]
}
func (Value[T]) local() { type ReceiverLocal struct{}; var _ ReceiverLocal }
func ordinary() { type Ordinary struct{}; var _ Ordinary }
`
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	assertTypeTargets(t, result, path, source, "Any", "Ordinary")
	for _, name := range []string{"GenericI", "Value", "Local", "Captured", "LocalI", "CapturedI", "ReceiverLocal"} {
		lens := assertTypeTargets(t, result, path, source, name)
		if !strings.Contains(lens.Command.Title, "0+") {
			t.Fatalf("%s claimed exact absence: %q", name, lens.Command.Title)
		}
	}
}

func TestObservedGenericArgumentsPreserveNamedTypeIdentity(t *testing.T) {
	source := `package specimen
import (
 "example.com/specimen/left"
 "example.com/specimen/right"
)
type GenericI[T any] interface { Put(T) }
type Value[T any] struct{}
func (Value[T]) Put(T) {}
var _ Value[left.Token]
var _ GenericI[right.Token]
`
	root := fixture(t, map[string]string{
		"go.mod":         "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go":        source,
		"left/token.go":  "package left\ntype Token struct{}\n",
		"right/token.go": "package right\ntype Token struct{}\n",
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	assertTypeTargets(t, result, path, source, "GenericI")
	assertTypeTargets(t, result, path, source, "Value")
}

func TestObservedGenericsUseIntermediateTestVariantGraphs(t *testing.T) {
	api := "package api\ntype I[T any] interface { M(T) }\n"
	implementation := "package implementation\ntype Value[T any] struct{}\nfunc (Value[T]) M(T) {}\n"
	root := fixture(t, map[string]string{
		"go.mod":                  "module example.com/specimen\n\ngo 1.26.0\n",
		"api/api.go":              api,
		"implementation/value.go": implementation,
		"shared/shared.go":        "package shared\ntype Arg struct{}\n",
		"shared/internal_test.go": "package shared\ntype TestAnchor struct{}\n",
		"ordinary/use.go":         "package ordinary\nimport (\"example.com/specimen/api\"; \"example.com/specimen/shared\")\nvar _ api.I[shared.Arg]\n",
		"shared/external_test.go": `package shared_test
import (
 "example.com/specimen/api"
 "example.com/specimen/implementation"
 "example.com/specimen/shared"
)
var _ api.I[shared.Arg] = implementation.Value[shared.Arg]{}
`,
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	iface := typeLens(t, result, filepath.Join(root, "api", "api.go"), api, "I")
	value := typeLens(t, result, filepath.Join(root, "implementation", "value.go"), implementation, "Value")
	for _, test := range []struct {
		lens codeLens
		uri  string
		want sourceRange
	}{
		{iface, fileURI(filepath.Join(root, "implementation", "value.go")), value.Range},
		{value, fileURI(filepath.Join(root, "api", "api.go")), iface.Range},
	} {
		targets := targetsOf(t, test.lens)
		if len(targets) != 1 || targets[0].URI != test.uri || targets[0].Range != test.want {
			t.Fatalf("generic test graph lost source identity: %#v", targets)
		}
	}
}

func TestDependencyGenericDeclarationsStayOutsideResults(t *testing.T) {
	source := `package specimen
import "example.com/dependency"
type I interface { M() }
type Local[T any] struct{}
func (Local[T]) M() {}
type GenericI[T any] interface { M() }
var _ dependency.External[int]
var _ dependency.Interface[int]
var _ Local[int]
var _ GenericI[int]
`
	root := fixture(t, map[string]string{
		"workspace/go.mod":  "module example.com/specimen\n\ngo 1.26.0\n\nrequire example.com/dependency v0.0.0\nreplace example.com/dependency => ../dependency\n",
		"workspace/main.go": source,
		"dependency/go.mod": "module example.com/dependency\n\ngo 1.26.0\n",
		"dependency/dep.go": "package dependency\ntype External[T any] struct{}\nfunc (External[T]) M() {}\ntype Interface[T any] interface { M() }\n",
	})
	dir := filepath.Join(root, "workspace")
	result, err := analyze(context.Background(), fixtureSnapshot(dir))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "main.go")
	assertTypeTargets(t, result, path, source, "I", "Local")
	assertTypeTargets(t, result, path, source, "GenericI", "Local")
	assertTypeTargets(t, result, path, source, "Local", "I", "GenericI")
}

func TestUnrelatedTestVariantImportsDoNotPreventStructuralMatches(t *testing.T) {
	implementation := `package implementation
import "example.com/specimen/shared"
var _ shared.Arg
type Value struct{}
func (Value) M() {}
type Generic[T any] struct{}
func (Generic[T]) M() {}
var _ Generic[int]
`
	interfaces := `package shared_test
import "example.com/specimen/shared"
var _ shared.Arg
type TestI interface { M() }
type GenericTestI[T any] interface { M() }
var _ GenericTestI[int]
`
	root := fixture(t, map[string]string{
		"go.mod":                  "module example.com/specimen\n\ngo 1.26.0\n",
		"implementation/value.go": implementation,
		"shared/shared.go":        "package shared\ntype Arg struct{}\n",
		"shared/internal_test.go": "package shared\ntype TestAnchor struct{}\n",
		"shared/external_test.go": interfaces,
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	implementationPath := filepath.Join(root, "implementation", "value.go")
	interfacePath := filepath.Join(root, "shared", "external_test.go")
	for _, name := range []string{"TestI", "GenericTestI"} {
		lens := typeLens(t, result, interfacePath, interfaces, name)
		targets := targetsOf(t, lens)
		want := map[sourceRange]bool{}
		for _, targetName := range []string{"Value", "Generic"} {
			target := typeLens(t, result, implementationPath, implementation, targetName)
			want[target.Range] = true
		}
		if len(targets) != len(want) {
			t.Fatalf("%s: unrelated imports hid implementations: %#v", name, targets)
		}
		for _, target := range targets {
			if target.URI != fileURI(implementationPath) || !want[target.Range] {
				t.Fatalf("%s: unexpected or duplicate target: %#v", name, target)
			}
			delete(want, target.Range)
		}
	}
}

func TestObservedGenericNonStructsRemainImplementationCandidates(t *testing.T) {
	source := `package specimen
type I interface { Len() int }
type Sequence[T any] []T
func (value Sequence[T]) Len() int { return len(value) }
var _ Sequence[int]
`
	root := fixture(t, map[string]string{"go.mod": "module example.com/specimen\n\ngo 1.26.0\n", "main.go": source})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	lens := typeLens(t, result, path, source, "I")
	position, err := offsetPosition([]byte(source), strings.Index(source, "type Sequence")+len("type "))
	if err != nil {
		t.Fatal(err)
	}
	targets := targetsOf(t, lens)
	if len(targets) != 1 || targets[0].URI != fileURI(path) || targets[0].Range.Start != position {
		t.Fatalf("generic non-struct implementation missing: %#v", targets)
	}
	assertNoTypeLens(t, result, path, source, "Sequence")
}

func TestDependencyExpressionsExposeWorkspaceGenericInstances(t *testing.T) {
	api := `package api
type IntI interface { Get() int }
type StringI interface { Get() string }
type BoolI interface { Get() bool }
type UintI interface { Get() uint }
type GenericI[T any] interface { Get() T }
type Value[T any] struct{}
func (Value[T]) Get() T { var zero T; return zero }
type Pointer[T any] struct{}
func (*Pointer[T]) Get() T { var zero T; return zero }
`
	dependency := `package dependency
import "example.com/specimen/api"
func New() api.Value[int] { return api.Value[int]{} }
func NewInterface() api.GenericI[int] { return New() }
func NewPointer() *api.Pointer[bool] { return nil }
var Value api.Value[string]
var Interface api.GenericI[string]
var PointerInterface api.GenericI[bool]
type Bundle struct {
 Value api.Value[uint]
 Interface api.GenericI[uint]
}
`
	root := fixture(t, map[string]string{
		"workspace/go.mod":     "module example.com/specimen\n\ngo 1.26.0\n\nrequire example.com/dependency v0.0.0\nreplace example.com/dependency => ../dependency\n",
		"workspace/api/api.go": api,
		"workspace/main.go": `package specimen
import "example.com/dependency"
var _ = dependency.New()
var _ = dependency.NewInterface()
var _ = dependency.NewPointer()
var _ = dependency.Value
var _ = dependency.Interface
var _ = dependency.PointerInterface
var _ dependency.Bundle
`,
		"dependency/go.mod":        "module example.com/dependency\n\ngo 1.26.0\n\nrequire example.com/specimen v0.0.0\n",
		"dependency/dependency.go": dependency,
	})
	dir := filepath.Join(root, "workspace")
	result, err := analyze(context.Background(), fixtureSnapshot(dir))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "api", "api.go")
	for _, name := range []string{"IntI", "StringI", "UintI"} {
		assertTypeTargets(t, result, path, api, name, "Value")
	}
	assertTypeTargets(t, result, path, api, "BoolI", "Pointer")
	assertTypeTargets(t, result, path, api, "GenericI", "Value", "Pointer")
	assertTypeTargets(t, result, path, api, "Value", "IntI", "StringI", "UintI", "GenericI")
	assertTypeTargets(t, result, path, api, "Pointer", "BoolI", "GenericI")
}

func TestGenericMethodBindersAreNotConcreteInstantiations(t *testing.T) {
	toolchain := strings.TrimPrefix(runtime.Version(), "devel ")
	toolchain, _, _ = strings.Cut(toolchain, "-")
	if !version.IsValid(toolchain) || version.Compare(toolchain, "go1.27") < 0 {
		t.Skip("generic methods require Go 1.27")
	}
	for _, test := range []struct {
		name         string
		method       string
		use          string
		stringResult bool
	}{
		{
			name:   "RecursiveUninstantiatedResult",
			method: "func (Value[T]) Next[U any]() Value[U] { return Value[U]{} }\n",
		},
		{
			name:   "IndependentMethodParameter",
			method: "func (Value[T]) F[U any](U) {}\n",
		},
		{
			name:         "ObservedMethodInstantiation",
			method:       "func (Value[T]) Next[U any]() Value[U] { return Value[U]{} }\n",
			use:          "var _ = Value[int]{}.Next[string]()\n",
			stringResult: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := `package specimen
type IntI interface { Get() int }
type StringI interface { Get() string }
type Value[T any] struct{}
func (Value[T]) Get() T { var zero T; return zero }
var _ Value[int]
` + test.method + test.use
			root := fixture(t, map[string]string{
				"go.mod":  "module example.com/specimen\n\ngo 1.27.0\n",
				"main.go": source,
			})
			result, err := analyze(context.Background(), fixtureSnapshot(root))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "main.go")
			assertTypeTargets(t, result, path, source, "IntI", "Value")
			if test.stringResult {
				assertTypeTargets(t, result, path, source, "StringI", "Value")
				assertTypeTargets(t, result, path, source, "Value", "IntI", "StringI")
			} else {
				assertTypeTargets(t, result, path, source, "StringI")
				assertTypeTargets(t, result, path, source, "Value", "IntI")
			}
		})
	}
}

func TestParenthesizedGenericReceiversKeepLocalTypesUnresolved(t *testing.T) {
	source := `package specimen
type Any interface{}
type Generic[T any] struct{}
func (value ((Generic[T]))) valueScope() {
 type ValueLocal struct{}
 type ValueLocalI interface{}
 var _ ValueLocal
 var _ ValueLocalI
}
func (value (*((Generic[T])))) pointerScope() {
 type PointerLocal struct{}
 type PointerLocalI interface{}
 var _ PointerLocal
 var _ PointerLocalI
}
var _ Generic[int]
`
	root := fixture(t, map[string]string{
		"go.mod":  "module example.com/specimen\n\ngo 1.26.0\n",
		"main.go": source,
	})
	result, err := analyze(context.Background(), fixtureSnapshot(root))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "main.go")
	assertTypeTargets(t, result, path, source, "Any", "Generic")
	assertTypeTargets(t, result, path, source, "Generic", "Any")
	for _, name := range []string{"ValueLocal", "ValueLocalI", "PointerLocal", "PointerLocalI"} {
		lens := assertTypeTargets(t, result, path, source, name)
		if !strings.Contains(lens.Command.Title, "0+") {
			t.Fatalf("%s claimed exact absence: %q", name, lens.Command.Title)
		}
	}
}
