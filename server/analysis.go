package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/mod/modfile"
	"golang.org/x/tools/go/packages"
)

type options struct {
	BuildFlags []string          `json:"buildFlags"`
	Env        map[string]string `json:"env"`
}
type snapshot struct {
	roots   []string
	overlay map[string][]byte
	options options
}
type analysis struct {
	lenses map[string][]codeLens
}
type declaration struct {
	named    *types.Named
	pointer  *types.Pointer
	location location
	key      string
	iface    *types.Interface
	isStruct bool
}

func configuredEnv(opts options) []string {
	env := os.Environ()
	keys := make([]string, 0, len(opts.Env))
	for key := range opts.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		env = append(env, key+"="+opts.Env[key])
	}
	return env
}

// A single Load is essential: go/types identities cannot be mixed across loads.
// Only opened roots are watched by the client: both go.work and every use
// module must be covered by those roots. Refuse incomplete coverage rather
// than publish exact-looking counts that external edits can silently stale.
func loadDirectories(ctx context.Context, snap snapshot) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string
	watchedRoots := make([]string, len(snap.roots))
	for i, root := range snap.roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolving workspace root %s: %w", root, err)
		}
		watchedRoots[i] = resolved
	}
	requireWatched := func(path, kind string) error {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolving %s %s: %w", kind, path, err)
		}
		if !workspacePath(resolved, watchedRoots) {
			return fmt.Errorf("%s %s is outside the opened workspace roots; open a root covering it before analyzing this Go workspace", kind, path)
		}
		return nil
	}
	for _, root := range snap.roots {
		cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOWORK", "GOMOD")
		cmd.Dir = root
		cmd.Env = configuredEnv(snap.options)
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if failure, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("go env: %s", strings.TrimSpace(string(failure.Stderr)))
			}
			return nil, fmt.Errorf("go env: %w", err)
		}
		var env struct {
			GOWORK string
			GOMOD  string
		}
		if err := json.Unmarshal(out, &env); err != nil {
			return nil, err
		}
		candidates := []string{root}
		if env.GOWORK != "" && env.GOWORK != "off" {
			if err := requireWatched(env.GOWORK, "go.work file"); err != nil {
				return nil, err
			}
			data, err := os.ReadFile(env.GOWORK)
			if err != nil {
				return nil, err
			}
			work, err := modfile.ParseWork(env.GOWORK, data, nil)
			if err != nil {
				return nil, err
			}
			candidates = nil
			for _, use := range work.Use {
				dir := filepath.FromSlash(use.Path)
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(filepath.Dir(env.GOWORK), dir)
				}
				if err := requireWatched(dir, "go.work use module"); err != nil {
					return nil, err
				}
				candidates = append(candidates, dir)
			}
		} else if env.GOMOD == "" || env.GOMOD == os.DevNull {
			return nil, fmt.Errorf("%s is not in a Go module or go.work workspace", root)
		}
		for _, dir := range candidates {
			dir = filepath.Clean(dir)
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("workspace has no Go modules")
	}
	sort.Strings(dirs)
	return dirs, nil
}

func analyze(ctx context.Context, snap snapshot) (*analysis, error) {
	dirs, err := loadDirectories(ctx, snap)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	sources := map[string][]byte{}
	var sourcesMu sync.Mutex
	cfg := &packages.Config{
		Context: ctx, Dir: snap.roots[0], Env: configuredEnv(snap.options),
		BuildFlags: snap.options.BuildFlags, Tests: true, Overlay: snap.overlay, Fset: fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			sourcesMu.Lock()
			sources[filepath.Clean(filename)] = src
			sourcesMu.Unlock()
			return parser.ParseFile(fset, filename, src, parser.AllErrors)
		},
	}
	patterns := make([]string, len(dirs))
	for i, dir := range dirs {
		patterns[i] = filepath.ToSlash(dir) + "/..."
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, fmt.Errorf("loading Go workspace: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no buildable Go packages in workspace")
	}
	var loadErr error
	var workspacePackages []*packages.Package
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		if loadErr == nil && len(pkg.Errors) > 0 {
			loadErr = fmt.Errorf("workspace analysis incomplete: %s", pkg.Errors[0])
		}
		if loadErr == nil && pkg.IllTyped {
			loadErr = fmt.Errorf("workspace analysis incomplete: ill-typed package %s", pkg.PkgPath)
		}
		for _, path := range pkg.GoFiles {
			if workspacePath(path, dirs) {
				workspacePackages = append(workspacePackages, pkg)
				break
			}
		}
	})
	if loadErr != nil {
		return nil, loadErr
	}
	var declarations []declaration
	genericTypesExcluded := false
	genericInterfacesExcluded := false
	for _, pkg := range workspacePackages {
		if strings.HasSuffix(pkg.PkgPath, ".test") && pkg.Name == "main" {
			continue
		}
		if pkg.TypesInfo == nil || pkg.Types == nil {
			return nil, fmt.Errorf("missing type information for %s", pkg.PkgPath)
		}
		for _, file := range pkg.Syntax {
			var declarationErr error
			genericDepth := 0
			var scopes []bool
			ast.Inspect(file, func(node ast.Node) bool {
				if ctx.Err() != nil || declarationErr != nil {
					return false
				}
				if node == nil {
					if scopes[len(scopes)-1] {
						genericDepth--
					}
					scopes = scopes[:len(scopes)-1]
					return true
				}
				genericScope := false
				if function, ok := node.(*ast.FuncDecl); ok {
					genericScope = function.Type.TypeParams != nil && len(function.Type.TypeParams.List) > 0
					if function.Recv != nil {
						for _, field := range function.Recv.List {
							receiver := field.Type
							if pointer, ok := receiver.(*ast.StarExpr); ok {
								receiver = pointer.X
							}
							switch receiver.(type) {
							case *ast.IndexExpr, *ast.IndexListExpr:
								genericScope = true
							}
						}
					}
				}
				scopes = append(scopes, genericScope)
				if genericScope {
					genericDepth++
				}
				spec, ok := node.(*ast.TypeSpec)
				if !ok || spec.Assign.IsValid() {
					return true
				}
				obj, ok := pkg.TypesInfo.Defs[spec.Name].(*types.TypeName)
				if !ok {
					declarationErr = fmt.Errorf("missing type definition for %s", spec.Name.Name)
					return false
				}
				named, ok := obj.Type().(*types.Named)
				if !ok {
					return true
				}
				iface, isInterface := named.Underlying().(*types.Interface)
				if isInterface && !iface.Complete().IsMethodSet() {
					return true
				}
				if named.TypeParams().Len() > 0 || genericDepth > 0 {
					if isInterface {
						genericInterfacesExcluded = true
					} else {
						genericTypesExcluded = true
					}
					return true
				}
				start := fset.PositionFor(spec.Name.Pos(), false)
				end := fset.PositionFor(spec.Name.End(), false)
				path := filepath.Clean(start.Filename)
				if !workspacePath(path, dirs) {
					declarationErr = fmt.Errorf("cannot safely locate named types in generated source %s (including cgo rewrites); no implementation counts are available", path)
					return false
				}
				text, exists := sources[path]
				if !exists {
					declarationErr = fmt.Errorf("missing source text for %s", path)
					return false
				}
				begin, err := offsetPosition(text, start.Offset)
				if err != nil {
					declarationErr = err
					return false
				}
				finish, err := offsetPosition(text, end.Offset)
				if err != nil {
					declarationErr = err
					return false
				}
				loc := location{URI: fileURI(path), Range: sourceRange{Start: begin, End: finish}}
				decl := declaration{named: named, iface: iface, location: loc, key: locationKey(loc)}
				if !isInterface {
					_, decl.isStruct = named.Underlying().(*types.Struct)
					decl.pointer = types.NewPointer(named)
				}
				declarations = append(declarations, decl)
				return true
			})
			if declarationErr != nil {
				return nil, declarationErr
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result := &analysis{lenses: map[string][]codeLens{}}
	// The ordinary and augmented test variants repeat declarations. Compare each
	// graph's interface against its candidates, then union by source declaration.
	type matches struct {
		declaration declaration
		targets     map[string]location
	}
	entries := map[string]*matches{}
	for _, decl := range declarations {
		if decl.iface != nil || decl.isStruct {
			if entries[decl.key] == nil {
				entries[decl.key] = &matches{declaration: decl, targets: map[string]location{}}
			}
		}
	}
	for _, decl := range declarations {
		if decl.iface == nil {
			continue
		}
		entry := entries[decl.key]
		for _, candidate := range declarations {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if candidate.iface != nil {
				continue
			}
			if types.Implements(candidate.named, decl.iface) || types.Implements(candidate.pointer, decl.iface) {
				entry.targets[candidate.key] = candidate.location
				if candidate.isStruct {
					entries[candidate.key].targets[decl.key] = decl.location
				}
			}
		}
	}
	for _, entry := range entries {
		targets := make([]location, 0, len(entry.targets))
		for _, loc := range entry.targets {
			targets = append(targets, loc)
		}
		sort.Slice(targets, func(i, j int) bool { return locationKey(targets[i]) < locationKey(targets[j]) })
		plural, singular := "implementations", "implementation"
		incomplete := genericTypesExcluded
		exclusion := "generic types excluded"
		if entry.declaration.iface == nil {
			plural, singular = "interfaces", "interface"
			incomplete = genericInterfacesExcluded
			exclusion = "generic interfaces excluded"
		}
		title := fmt.Sprintf("%d %s", len(targets), plural)
		if len(targets) == 1 {
			title = "1 " + singular
		}
		if incomplete {
			title = fmt.Sprintf("Known %s (incomplete: %s)", plural, exclusion)
		}
		lens := codeLens{Range: entry.declaration.location.Range, Command: &command{Title: title, Command: "editor.action.goToLocations", Arguments: []any{entry.declaration.location.URI, entry.declaration.location.Range.Start, targets}}}
		path, _ := uriPath(entry.declaration.location.URI)
		result.lenses[path] = append(result.lenses[path], lens)
	}
	for path := range result.lenses {
		sort.Slice(result.lenses[path], func(i, j int) bool {
			a, b := result.lenses[path][i].Range.Start, result.lenses[path][j].Range.Start
			return a.Line < b.Line || (a.Line == b.Line && a.Character < b.Character)
		})
	}
	return result, nil
}
func locationKey(loc location) string {
	return fmt.Sprintf("%s:%010d:%010d", loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
}

func workspacePath(path string, dirs []string) bool {
	for _, dir := range dirs {
		relative, err := filepath.Rel(dir, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
			return true
		}
	}
	return false
}
