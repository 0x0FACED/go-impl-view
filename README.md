# Go Implementation View for Zed

CodeLens for named Go interfaces and structs, with navigation to their implementations and implemented interfaces. This extension runs the `go-impl-view` language server alongside `gopls`; it does not replace `gopls`.

- Extension ID: `go-impl-view-lsp`
- Language server ID: `go-impl-view`

## Installation

This extension is being prepared for publication and is not yet listed in Zed's extension registry. Install it from this checkout using the development installation below.

A Go toolchain must be available on Zed's language-server `PATH` even when using a prebuilt server: analysis runs `go env` and loads packages through Go tooling. Use Go 1.26 or newer to build the server from source.

### Development installation

Install Zed, Go 1.26 or newer, and Rust through `rustup`. From this repository's root, build the server:

macOS or Linux:

```sh
mkdir -p "$(go env GOPATH)/bin"
go build -o "$(go env GOPATH)/bin/go-impl-view" ./server
```

Windows PowerShell:

```powershell
$goBin = Join-Path (go env GOPATH) "bin"
New-Item -ItemType Directory -Force $goBin | Out-Null
go build -o (Join-Path $goBin "go-impl-view.exe") ./server
```

Add that `bin` directory to the environment `PATH` used by Zed, then restart Zed if it was already running. In the command palette, run `zed: install dev extension` and select this repository's root. Zed builds the Rust extension for `wasm32-wasip2`; with Rust installed through `rustup`, Zed installs the target automatically.

After changing server source, rebuild the server with the same command and run `editor: restart language server` in a Go file. After changing the Rust extension, rebuild the development extension from Zed's Extensions page.

### Server selection and release downloads

A `go-impl-view` executable on Zed's language-server `PATH` takes precedence over release downloads. Use this to run a locally built or manually installed server.

When no executable is found, the extension downloads the latest stable GitHub release for macOS, Linux, or Windows on x86-64 or ARM64, verifies its SHA-256 checksum, and caches the server by release version and platform. Until the first release is published, use the source installation above.

## Enable CodeLens

Add these settings to Zed's `settings.json`:

```json
{
  "code_lens": "on",
  "languages": {
    "Go": {
      "language_servers": ["go-impl-view", "gopls", "..."]
    }
  }
}
```

Use the server ID `go-impl-view` in settings, not the extension ID `go-impl-view-lsp`. Keep `gopls` enabled for completion, diagnostics, and other Go language features.

## Navigation

An interface lens shows the number of named concrete type declarations that implement it. A struct lens shows `0 interfaces`, `1 interface`, or `N interfaces`. Click a lens to navigate to the matching declarations.

Navigation uses `editor.action.goToLocations`, which Zed handles but does not document as a stable extension API. A single match navigates directly; multiple matches may open a multibuffer rather than a separate results list. Nearby declarations in one file may appear together. An empty result opens no list. This behavior may change between Zed versions.

## Analysis scope and limitations

- Analysis uses `go/packages` and `go/types` with the active Go build configuration, including test packages. It is not a search across all installed modules.
- Without `go.work`, the search covers the opened workspace directory and its subpackages within the module. With `go.work`, it covers the modules in its `use` directives. The `go.work` file and all referenced modules must be inside opened workspace roots; otherwise analysis is refused rather than showing potentially stale counts. Open unrelated modules separately.
- Dependencies are loaded for type checking but are not implementation candidates outside this scope. Package loading or type-checking errors prevent counts from being displayed.
- A concrete declaration counts once if either its value or pointer type implements the interface. Other interfaces and type aliases are not counted as separate implementations; aliases receive no lens. Reverse navigation is provided for named structs, not every concrete type.
- Generic types and declarations inside generic functions or methods are excluded. Interface lenses mark results as incomplete when generic concrete types were excluded; struct lenses do the same when generic interfaces were excluded. Generic interfaces, generic structs, and interfaces usable only as type-set constraints receive no lens. Constraint-only interfaces are not listed as interfaces implemented by a struct.
- Generated source locations outside the workspace, including cgo rewrites, can make analysis unavailable. Windows UNC paths and network shares are unsupported.
- Unsaved Go buffers sent by Zed are included. Outdated computations are cancelled and CodeLens refreshes are requested, but Zed may briefly retain an old lens during editing. Disk changes require the client's LSP watched-file support; clients without CodeLens refresh support may retain old lenses until another request.

## Build configuration

The server's Go toolchain and environment determine `GOOS`, `GOARCH`, `CGO_ENABLED`, `GOFLAGS`, `GOWORK`, and build tags. Settings for `gopls` are not automatically forwarded to this separate server.

Configure build flags or environment overrides for `go-impl-view` itself, then restart the language server:

```json
{
  "lsp": {
    "go-impl-view": {
      "initialization_options": {
        "buildFlags": ["-tags=integration"],
        "env": { "GOOS": "linux" }
      }
    }
  }
}
```

## Troubleshooting

- If the server cannot start, check `zed: open log`. Ensure `go` is visible in Zed's language-server environment. If using a locally installed server instead of a release download, ensure `go-impl-view` is also visible there. Restart Zed after changing its environment.
- If lenses are missing, check the settings above, resolve package-loading or type-checking errors, and ensure the entire Go workspace is open. Run `editor: restart language server` after changing the server binary or initialization options.
- If lenses still do not appear, try `editor: toggle code lens` or restart Zed. A CodeLens display issue was previously observed and stopped after restarting Zed; its cause is unknown.

## Releasing

Update the extension version in `extension.toml`, then push a matching tag such as `v0.1.0`. The release workflow builds the server for macOS, Linux, and Windows on amd64 and arm64, publishes the archives and `SHA256SUMS` to GitHub Releases, and marks prerelease tags accordingly. Automatic installation uses stable releases only.

Submit the extension to the [Zed extension registry](https://zed.dev/docs/extensions/publishing/publishing-guide) after testing the exact submitted commit in Zed. Registry updates require a matching version in the registry's `extensions.toml`.

## Publication status

Publish the first release with the platform assets before submitting the extension to the registry.
