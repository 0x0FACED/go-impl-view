package main

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

type options struct {
	BuildFlags []string          `json:"buildFlags"`
	Env        map[string]string `json:"env"`
}
type snapshot struct {
	path    string
	roots   []string
	overlay map[string][]byte
	invalid map[string]string
	options options
}
type analysis struct {
	lenses   map[string][]codeLens
	warnings []string
}
type declaration struct {
	named        *types.Named
	location     location
	key          string
	iface        *types.Interface
	isStruct     bool
	incomplete   bool
	genericScope bool
	observations []observedType
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

func analyze(ctx context.Context, snap snapshot) (*analysis, error) {
	scope, err := resolveScope(ctx, snap)
	if err != nil {
		return nil, err
	}
	cleanup, err := scope.prepareWorkspace(snap)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	dirs := make([]string, len(scope.dirs))
	for i, dir := range scope.dirs {
		dirs[i] = canonicalFile(dir)
	}
	overlay := make(map[string][]byte, len(snap.overlay))
	invalid := make(map[string]string, len(snap.invalid))
	names := make(map[string]string, len(snap.overlay)+1)
	vendorSource := ""
	if scope.vendor != "" {
		vendorSource = canonicalFile(filepath.Join(filepath.Dir(scope.work), "vendor"))
	}
	for path, text := range snap.overlay {
		resolved := canonicalFile(path)
		if previous, exists := overlay[resolved]; exists && !bytes.Equal(previous, text) {
			invalid[resolved] = "conflicting overlays for the same file"
		}
		overlay[resolved], names[resolved] = text, path
		if resolved != path {
			overlay[path] = text
		}
		if vendorSource != "" && workspacePath(resolved, []string{vendorSource}) {
			relative, _ := filepath.Rel(vendorSource, resolved)
			overlay[filepath.Join(scope.vendor, relative)] = text
		}
	}
	if scope.work != "" {
		if text, exists := overlayText(snap.overlay, scope.work); exists {
			overlay[scope.work] = text
		}
	}
	for _, dir := range scope.dirs {
		path := filepath.Join(dir, "go.mod")
		if text, exists := overlayText(snap.overlay, path); exists {
			overlay[path] = text
		}
	}
	for path, reason := range snap.invalid {
		invalid[canonicalFile(path)] = reason
	}
	names[canonicalFile(snap.path)] = snap.path
	displayPath := func(path string) string {
		if original := names[path]; original != "" {
			return original
		}
		for i, dir := range dirs {
			if workspacePath(path, []string{dir}) {
				relative, _ := filepath.Rel(dir, path)
				return filepath.Join(scope.dirs[i], relative)
			}
		}
		return path
	}
	for path, reason := range invalid {
		if !workspacePath(path, dirs) {
			continue
		}
		module, err := moduleDirectory(path, overlay)
		if err != nil {
			return nil, err
		}
		if scope.ownsModule(module) {
			return nil, fmt.Errorf("document %s is out of sync: %s; reopen it", path, reason)
		}
	}
	fset := token.NewFileSet()
	sources := map[string][]byte{}
	var sourcesMu sync.Mutex
	cfg := &packages.Config{
		Context: ctx, Dir: scope.dir, Env: scope.env,
		BuildFlags: snap.options.BuildFlags, Tests: true, Overlay: overlay, Fset: fset,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
		ParseFile: func(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
			path := canonicalFile(filename)
			sourcesMu.Lock()
			sources[path] = src
			sourcesMu.Unlock()
			return parser.ParseFile(fset, filename, src, parser.AllErrors)
		},
	}
	patterns := make([]string, len(dirs))
	for i, dir := range scope.dirs {
		patterns[i] = filepath.ToSlash(dir) + "/..."
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, scope.loadError(fmt.Errorf("loading Go workspace: %w", err))
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no buildable Go packages in workspace")
	}
	var loadErr error
	var workspacePackages []*packages.Package
	var unwatchedDependencies map[string]bool
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		if pkg.Module != nil {
			if reason := invalid[canonicalFile(pkg.Module.GoMod)]; reason != "" && loadErr == nil {
				loadErr = fmt.Errorf("document %s is out of sync: %s; reopen it", pkg.Module.GoMod, reason)
			}
		}
		for _, path := range pkg.GoFiles {
			if reason := invalid[canonicalFile(path)]; reason != "" && loadErr == nil {
				loadErr = fmt.Errorf("document %s is out of sync: %s; reopen it", path, reason)
			}
		}
		if loadErr == nil && len(pkg.Errors) > 0 {
			loadErr = fmt.Errorf("workspace analysis incomplete: %s", pkg.Errors[0])
		}
		if loadErr == nil && pkg.IllTyped {
			loadErr = fmt.Errorf("workspace analysis incomplete: ill-typed package %s", pkg.PkgPath)
		}
		if pkg.Module != nil && pkg.Module.Replace != nil && pkg.Module.Replace.Version == "" && len(pkg.GoFiles) > 0 {
			dir := canonicalFile(filepath.Dir(pkg.GoFiles[0]))
			if !workspacePath(dir, scope.watched) {
				if unwatchedDependencies == nil {
					unwatchedDependencies = map[string]bool{}
				}
				unwatchedDependencies[dir] = true
			}
		}
		if pkg.Module == nil || !scope.ownsModule(pkg.Module.Dir) {
			return
		}
		for _, path := range pkg.GoFiles {
			if workspacePath(canonicalFile(path), dirs) {
				workspacePackages = append(workspacePackages, pkg)
				break
			}
		}
	})
	if loadErr != nil {
		return nil, scope.loadError(loadErr)
	}
	var declarations []declaration
	genericTypes := false
	genericInterfaces := false
	var analyzedPackages []*packages.Package
	for _, pkg := range workspacePackages {
		if strings.HasSuffix(pkg.PkgPath, ".test") && pkg.Name == "main" {
			continue
		}
		analyzedPackages = append(analyzedPackages, pkg)
	}
	for _, pkg := range analyzedPackages {
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
							receiver := ast.Unparen(field.Type)
							if pointer, ok := receiver.(*ast.StarExpr); ok {
								receiver = ast.Unparen(pointer.X)
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
				incomplete := named.TypeParams().Len() > 0 || genericDepth > 0
				if incomplete {
					if isInterface {
						genericInterfaces = true
					} else {
						genericTypes = true
					}
				}
				start := fset.PositionFor(spec.Name.Pos(), false)
				end := fset.PositionFor(spec.Name.End(), false)
				path := canonicalFile(start.Filename)
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
				loc := location{URI: fileURI(displayPath(path)), Range: sourceRange{Start: begin, End: finish}}
				decl := declaration{named: named, iface: iface, location: loc, key: locationKey(loc), incomplete: incomplete, genericScope: genericDepth > 0}
				if !isInterface {
					_, decl.isStruct = named.Underlying().(*types.Struct)
				}
				// Local declarations in a generic function or receiver scope have
				// per-instantiation identities that go/types does not materialize.
				if genericDepth == 0 && named.TypeParams().Len() == 0 {
					decl.observations = []observedType{observeType(named)}
				}
				declarations = append(declarations, decl)
				return true
			})
			if declarationErr != nil {
				return nil, declarationErr
			}
		}
	}
	// Instances belong to the graph where the use was checked, not merely the
	// graph containing the generic declaration. Never transplant type arguments
	// between ordinary and augmented test package identities.
	origins := map[*types.Named][]int{}
	localGenericTypes := map[*types.Named]bool{}
	for index, decl := range declarations {
		if decl.genericScope {
			localGenericTypes[decl.named] = true
		}
		if decl.named.TypeParams().Len() > 0 && !decl.genericScope {
			origins[decl.named] = append(origins[decl.named], index)
		}
	}
	for _, pkg := range analyzedPackages {
		seen := map[types.Type]bool{}
		closed := closedTypeChecker{seen: map[types.Type]bool{}, localGenericTypes: localGenericTypes}
		collect := func(typ types.Type) {
			walkTypes(typ, seen, func(typ types.Type) bool {
				named, ok := typ.(*types.Named)
				if !ok || named.TypeArgs().Len() == 0 || len(origins[named.Origin()]) == 0 || !closed.isClosed(named) {
					return true
				}
				for _, index := range origins[named.Origin()] {
					decl := &declarations[index]
					duplicate := false
					for _, previous := range decl.observations {
						if types.Identical(previous.named, named) {
							duplicate = true
							break
						}
					}
					if !duplicate {
						decl.observations = append(decl.observations, observeType(named))
					}
				}
				return true
			})
		}
		for _, instance := range pkg.TypesInfo.Instances {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			collect(instance.Type)
		}
		// Non-generic calls and imported values can expose workspace generic
		// instances without spelling or inferring a type argument at this use.
		for _, expression := range pkg.TypesInfo.Types {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			collect(expression.Type)
		}
		for _, objects := range [2]map[*ast.Ident]types.Object{pkg.TypesInfo.Defs, pkg.TypesInfo.Uses} {
			for _, object := range objects {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if object != nil {
					collect(object.Type())
				}
			}
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	result := &analysis{lenses: map[string][]codeLens{}}
	if len(unwatchedDependencies) > 0 {
		paths := make([]string, 0, len(unwatchedDependencies))
		for path := range unwatchedDependencies {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		result.warnings = []string{"local replace dependencies are outside the opened workspace roots: " + strings.Join(paths, ", ") + "; external edits are observed only after client file notifications or another workspace invalidation. Open roots covering these dependencies for automatic watched-file updates."}
	}
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
			if observedImplementation(candidate.observations, decl.observations) {
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
		incomplete := entry.declaration.incomplete || genericTypes
		if entry.declaration.iface == nil {
			plural, singular = "interfaces", "interface"
			incomplete = entry.declaration.incomplete || genericInterfaces
		}
		title := fmt.Sprintf("%d %s", len(targets), plural)
		if len(targets) == 1 {
			title = "1 " + singular
		}
		if incomplete {
			title = fmt.Sprintf("%d+ %s", len(targets), plural)
		}
		lens := codeLens{Range: entry.declaration.location.Range, Command: &command{Title: title, Command: "editor.action.goToLocations", Arguments: []any{entry.declaration.location.URI, entry.declaration.location.Range.Start, targets}}}
		path, _ := uriPath(entry.declaration.location.URI)
		result.lenses[path] = append(result.lenses[path], lens)
		if resolved := canonicalFile(path); resolved != path {
			result.lenses[resolved] = append(result.lenses[resolved], lens)
		}
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
