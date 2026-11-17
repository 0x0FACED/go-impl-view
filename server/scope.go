package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

type analysisScope struct {
	dir     string
	dirs    []string
	env     []string
	work    string
	vendor  string
	modules map[string]bool
	watched []string
}

// Find only ancestors of the requested document, never scan the opened tree.
func ancestorFile(path, name string, exists func(string) bool) (string, error) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		path := filepath.Join(dir, name)
		if exists(path) {
			return path, nil
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		} else if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			return "", nil
		}
	}
}

func moduleDirectory(path string, overlay map[string][]byte) (string, error) {
	mod, err := ancestorFile(path, "go.mod", func(path string) bool {
		_, exists := overlayText(overlay, path)
		return exists
	})
	if err != nil || mod == "" {
		return "", err
	}
	return filepath.Dir(mod), nil
}

// Resolve the existing prefix too: a new overlay package may introduce several
// directories below a symlinked workspace root.
func physicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) || filepath.Dir(path) == path {
		return "", err
	}
	parent, err := physicalPath(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

func canonicalFile(path string) string {
	if resolved, err := physicalPath(path); err == nil {
		return resolved
	}
	return path
}

func existingDirectory(path string) (string, error) {
	for {
		info, err := os.Stat(path)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", path)
			}
			return path, nil
		}
		if !os.IsNotExist(err) || filepath.Dir(path) == path {
			return "", err
		}
		path = filepath.Dir(path)
	}
}

func overlayText(overlay map[string][]byte, path string) ([]byte, bool) {
	if text, exists := overlay[path]; exists {
		return text, true
	}
	name := filepath.Base(path)
	resolved := ""
	for candidate, text := range overlay {
		if filepath.Base(candidate) != name {
			continue
		}
		if resolved == "" {
			resolved = canonicalFile(path)
		}
		if canonicalFile(candidate) == resolved {
			return text, true
		}
	}
	return nil, false
}

func snapshotFile(snap snapshot, path string) ([]byte, error) {
	resolved := canonicalFile(path)
	for candidate, reason := range snap.invalid {
		if reason != "" && canonicalFile(candidate) == resolved {
			return nil, fmt.Errorf("document %s is out of sync: %s; reopen it", candidate, reason)
		}
	}
	if text, exists := overlayText(snap.overlay, path); exists {
		return text, nil
	}
	return os.ReadFile(path)
}

func resolveScope(ctx context.Context, snap snapshot) (*analysisScope, error) {
	watched := make([]string, len(snap.roots))
	for i, root := range snap.roots {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, fmt.Errorf("resolving workspace root %s: %w", root, err)
		}
		watched[i] = resolved
	}
	requireWatched := func(path, kind string) error {
		resolved, err := physicalPath(path)
		if err != nil {
			return fmt.Errorf("resolving %s %s: %w", kind, path, err)
		}
		if !workspacePath(resolved, watched) {
			return fmt.Errorf("%s %s is outside the opened workspace roots; open a root covering it before analyzing this Go workspace", kind, path)
		}
		return nil
	}
	if err := requireWatched(filepath.Dir(snap.path), "document directory"); err != nil {
		return nil, err
	}
	module, err := moduleDirectory(snap.path, snap.overlay)
	if err != nil {
		return nil, err
	}
	if module == "" {
		return nil, fmt.Errorf("%s is not in a Go module or go.work use module", snap.path)
	}
	if err := requireWatched(module, "Go module"); err != nil {
		return nil, err
	}
	scope := &analysisScope{dir: module, dirs: []string{module}, env: configuredEnv(snap.options), watched: watched}
	directory, err := existingDirectory(filepath.Dir(snap.path))
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "go", "env", "GOWORK")
	cmd.Dir, cmd.Env = directory, scope.env
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
	env := struct{ GOWORK string }{GOWORK: strings.TrimRight(string(out), "\r\n")}
	// An unsaved new go.work may shadow a workspace found on disk. Explicit
	// GOWORK settings, including off, always win over automatic discovery.
	setting, explicit := snap.options.Env["GOWORK"]
	if !explicit {
		setting = os.Getenv("GOWORK")
	}
	if setting == "" || setting == "auto" {
		work, err := ancestorFile(snap.path, "go.work", func(path string) bool {
			_, exists := overlayText(snap.overlay, path)
			return exists
		})
		if err != nil {
			return nil, err
		}
		if work != "" {
			env.GOWORK = work
		}
	}
	if env.GOWORK == "" || env.GOWORK == "off" {
		return scope, nil
	}
	// Even a workspace that does not list this module must not silently bypass
	// the existing protection against unwatched go.work files.
	if _, err := os.Stat(env.GOWORK); os.IsNotExist(err) {
		if err := requireWatched(filepath.Dir(env.GOWORK), "go.work directory"); err != nil {
			return nil, err
		}
	} else if err := requireWatched(env.GOWORK, "go.work file"); err != nil {
		return nil, err
	}
	data, err := snapshotFile(snap, env.GOWORK)
	if err != nil {
		return nil, err
	}
	work, err := modfile.ParseWork(env.GOWORK, data, nil)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var dirs []string
	included := false
	moduleResolved, err := filepath.EvalSymlinks(module)
	if err != nil {
		return nil, err
	}
	for _, use := range work.Use {
		dir := filepath.FromSlash(use.Path)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(env.GOWORK), dir)
		}
		dir = filepath.Clean(dir)
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			included = included || resolved == moduleResolved
		}
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	if !included {
		// A nested independent module is not part of its parent's workspace.
		scope.env = append(scope.env, "GOWORK=off")
		return scope, nil
	}
	for _, dir := range dirs {
		if err := requireWatched(dir, "go.work use module"); err != nil {
			return nil, err
		}
	}
	sort.Strings(dirs)
	scope.dirs, scope.work = dirs, env.GOWORK
	scope.env = append(scope.env, "GOWORK="+scope.work)
	return scope, nil
}

func (scope *analysisScope) ownsModule(dir string) bool {
	if scope.modules == nil {
		scope.modules = make(map[string]bool, len(scope.dirs))
		for _, candidate := range scope.dirs {
			if actual, err := filepath.EvalSymlinks(candidate); err == nil {
				scope.modules[actual] = true
			}
		}
	}
	if scope.modules[dir] {
		return true
	}
	resolved, err := filepath.EvalSymlinks(dir)
	return err == nil && scope.modules[resolved]
}

// Existing workspace overlays are handled by Go's overlay filesystem. A new
// file or changed toolchain directive needs a real file before Go starts.
func (scope *analysisScope) prepareWorkspace(snap snapshot) (func(), error) {
	if scope.work == "" {
		return func() {}, nil
	}
	text, exists := overlayText(snap.overlay, scope.work)
	if !exists {
		return func() {}, nil
	}
	saved, readErr := os.ReadFile(scope.work)
	if readErr != nil && !os.IsNotExist(readErr) {
		return nil, readErr
	}
	if readErr == nil && bytes.Equal(saved, text) {
		return func() {}, nil
	}
	work, err := modfile.ParseWork(scope.work, text, nil)
	if err != nil {
		return nil, err
	}
	if readErr == nil {
		if disk, err := modfile.ParseWork(scope.work, saved, nil); err == nil {
			sameGo := disk.Go == nil && work.Go == nil || disk.Go != nil && work.Go != nil && disk.Go.Version == work.Go.Version
			sameToolchain := disk.Toolchain == nil && work.Toolchain == nil || disk.Toolchain != nil && work.Toolchain != nil && disk.Toolchain.Name == work.Toolchain.Name
			if sameGo && sameToolchain {
				return func() {}, nil
			}
		}
	}
	base := filepath.Dir(scope.work)
	for _, use := range work.Use {
		if !filepath.IsAbs(use.Path) {
			use.Path = filepath.Join(base, filepath.FromSlash(use.Path))
			if err := work.AddUse(use.Path, use.ModulePath); err != nil {
				return nil, err
			}
		}
	}
	for _, replace := range work.Replace {
		if replace.New.Version == "" && !filepath.IsAbs(replace.New.Path) {
			if err := work.AddReplace(replace.Old.Path, replace.Old.Version, filepath.Join(base, filepath.FromSlash(replace.New.Path)), ""); err != nil {
				return nil, err
			}
		}
	}
	dir, err := os.MkdirTemp("", "go-impl-work-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "go.work")
	if err := os.WriteFile(path, modfile.Format(work.Syntax), 0600); err != nil {
		cleanup()
		return nil, err
	}
	if sums, err := snapshotFile(snap, scope.work+".sum"); err == nil {
		if err := os.WriteFile(path+".sum", sums, 0600); err != nil {
			cleanup()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		cleanup()
		return nil, err
	}
	// Workspace vendoring is relative to go.work, not to its use modules.
	// Preserve that view without copying the vendor tree.
	vendor := filepath.Join(base, "vendor")
	if _, err := os.Lstat(vendor); err == nil {
		scope.vendor = filepath.Join(dir, "vendor")
		if err := os.Symlink(vendor, scope.vendor); err != nil {
			cleanup()
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		cleanup()
		return nil, err
	}
	scope.env = append(scope.env, "GOWORK="+path)
	return cleanup, nil
}

func (scope *analysisScope) loadError(err error) error {
	if scope.vendor != "" && strings.Contains(err.Error(), "inconsistent vendoring") {
		return fmt.Errorf("cannot analyze unsaved go.work with inconsistent workspace vendoring; save go.work and synchronize vendor, or use -mod=mod: %w", err)
	}
	return err
}
