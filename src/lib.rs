use std::{
    fs::{self, File, OpenOptions},
    io::{self, Read},
    path::Path,
};

use sha2::{Digest, Sha256};
use zed_extension_api as zed;

const GITHUB_REPOSITORY: &str = "0x0FACED/go-impl-view";
const SERVER_NAME: &str = "go-impl-view";
const MAX_BINARY_SIZE: u64 = 128 * 1024 * 1024;

struct GoImplView {
    cached_binary: Option<String>,
}

impl GoImplView {
    fn release_binary(&mut self, server_id: &zed::LanguageServerId) -> zed::Result<String> {
        if let Some(path) = &self.cached_binary {
            if is_regular_binary(Path::new(path)) {
                return Ok(path.clone());
            }
        }

        let (os, architecture) = zed::current_platform();
        let goos = match os {
            zed::Os::Mac => "darwin",
            zed::Os::Linux => "linux",
            zed::Os::Windows => "windows",
        };
        let goarch = match architecture {
            zed::Architecture::Aarch64 => "arm64",
            zed::Architecture::X8664 => "amd64",
            zed::Architecture::X86 => {
                return Err(
                    "No go-impl-view release binary is available for 32-bit x86; \
                            build ./server and put it on Zed's PATH."
                        .to_string(),
                );
            }
        };
        let windows = matches!(os, zed::Os::Windows);
        let binary_name = if windows {
            "go-impl-view.exe"
        } else {
            SERVER_NAME
        };

        zed::set_language_server_installation_status(
            server_id,
            &zed::LanguageServerInstallationStatus::CheckingForUpdate,
        );
        let release = zed::latest_github_release(
            GITHUB_REPOSITORY,
            zed::GithubReleaseOptions {
                require_assets: true,
                pre_release: false,
            },
        )
        .map_err(|error| format!("Failed to find the latest go-impl-view release: {error}"))?;
        validate_version(&release.version)?;
        let directory = format!("{SERVER_NAME}-{}-{goos}-{goarch}", release.version);
        let binary_path = format!("{directory}/{binary_name}");
        if !is_regular_directory(Path::new(&directory))
            || !is_regular_binary(Path::new(&binary_path))
        {
            let suffix = if windows { "zip" } else { "tar.gz" };
            let asset_name = format!("{directory}.{suffix}");
            let archive_url = release_asset(&release, &asset_name)?;
            let checksums_url = release_asset(&release, "SHA256SUMS")?;
            zed::set_language_server_installation_status(
                server_id,
                &zed::LanguageServerInstallationStatus::Downloading,
            );
            install_release(
                &directory,
                binary_name,
                &asset_name,
                archive_url,
                checksums_url,
                windows,
            )
            .map_err(|error| {
                format!(
                    "Failed to install go-impl-view {}: {error}",
                    release.version
                )
            })?;
        }

        let absolute_path = std::env::current_dir()
            .map_err(|error| format!("Cannot locate the extension working directory: {error}"))?
            .join(binary_path);
        let command = absolute_path
            .to_str()
            .ok_or_else(|| "The go-impl-view executable path is not valid UTF-8".to_string())?
            .to_string();
        self.cached_binary = Some(command.clone());
        Ok(command)
    }
}

impl zed::Extension for GoImplView {
    fn new() -> Self {
        Self {
            cached_binary: None,
        }
    }

    fn language_server_command(
        &mut self,
        language_server_id: &zed::LanguageServerId,
        worktree: &zed::Worktree,
    ) -> zed::Result<zed::Command> {
        let result = match worktree.which(SERVER_NAME) {
            Some(command) => Ok(command),
            None => self.release_binary(language_server_id),
        };
        let status = match &result {
            Ok(_) => zed::LanguageServerInstallationStatus::None,
            Err(error) => zed::LanguageServerInstallationStatus::Failed(error.clone()),
        };
        zed::set_language_server_installation_status(language_server_id, &status);
        Ok(zed::Command {
            command: result?,
            args: Vec::new(),
            env: worktree.shell_env(),
        })
    }
}

fn validate_version(version: &str) -> zed::Result<()> {
    // Tags enter both filenames and URLs. Never accept separators or traversal components.
    if version.len() < 2
        || version.len() > 128
        || !version.starts_with('v')
        || !version.as_bytes()[1].is_ascii_digit()
        || !version
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b".-_+".contains(&byte))
        || version.contains("..")
    {
        return Err(format!("Unsupported go-impl-view release tag: {version:?}"));
    }
    Ok(())
}

fn release_asset<'a>(release: &'a zed::GithubRelease, name: &str) -> zed::Result<&'a str> {
    let mut assets = release.assets.iter().filter(|asset| asset.name == name);
    let asset = assets.next().ok_or_else(|| {
        format!(
            "Release {} does not contain the required asset {name}",
            release.version
        )
    })?;
    if assets.next().is_some() || !asset.download_url.starts_with("https://") {
        return Err(format!(
            "Release {} has an invalid asset {name}",
            release.version
        ));
    }
    Ok(&asset.download_url)
}

fn is_regular_directory(path: &Path) -> bool {
    fs::symlink_metadata(path).is_ok_and(|metadata| metadata.file_type().is_dir())
}

fn is_regular_binary(path: &Path) -> bool {
    fs::symlink_metadata(path)
        .is_ok_and(|metadata| metadata.file_type().is_file() && metadata.len() > 0)
}

fn remove_install_directory(path: &str) -> zed::Result<()> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_dir() => {
            fs::remove_dir_all(path).map_err(|error| format!("Cannot remove {path}: {error}"))
        }
        Ok(_) => Err(format!(
            "Refusing to replace non-directory installation path {path}"
        )),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(format!("Cannot inspect {path}: {error}")),
    }
}

fn install_release(
    directory: &str,
    binary_name: &str,
    asset_name: &str,
    archive_url: &str,
    checksums_url: &str,
    windows: bool,
) -> zed::Result<()> {
    let staging = format!("{directory}.download");
    remove_install_directory(&staging)?;
    fs::create_dir(&staging)
        .map_err(|error| format!("Cannot create installation directory {staging}: {error}"))?;
    let result = (|| {
        let archive_path = format!("{staging}/release.archive");
        let checksums_path = format!("{staging}/SHA256SUMS");
        for (url, path) in [
            (checksums_url, &checksums_path),
            (archive_url, &archive_path),
        ] {
            zed::download_file(url, path, zed::DownloadedFileType::Uncompressed)
                .map_err(|error| format!("Cannot download {url}: {error}"))?;
        }
        verify_checksum(&archive_path, &checksums_path, asset_name)?;
        let binary_path = format!("{staging}/{binary_name}");
        extract_binary(&archive_path, &binary_path, binary_name, windows)?;
        zed::make_file_executable(&binary_path)
            .map_err(|error| format!("Cannot make {binary_name} executable: {error}"))?;
        for path in [&archive_path, &checksums_path] {
            fs::remove_file(path)
                .map_err(|error| format!("Cannot remove downloaded file {path}: {error}"))?;
        }
        remove_install_directory(directory)?;
        fs::rename(&staging, directory)
            .map_err(|error| format!("Cannot finalize installation in {directory}: {error}"))?;
        Ok(())
    })();
    if result.is_err() {
        // A partial download must never become a usable cached installation.
        let _ = fs::remove_dir_all(&staging);
    }
    result
}

fn verify_checksum(archive: &str, checksums: &str, asset_name: &str) -> zed::Result<()> {
    let contents = fs::read_to_string(checksums)
        .map_err(|error| format!("Cannot read release checksums: {error}"))?;
    let mut matching = contents.lines().filter_map(|line| {
        let mut fields = line.split_whitespace();
        let checksum = fields.next()?;
        let filename = fields.next()?;
        let filename = filename.strip_prefix('*').unwrap_or(filename);
        (filename == asset_name && fields.next().is_none()).then_some(checksum)
    });
    let expected = matching
        .next()
        .ok_or_else(|| format!("SHA256SUMS has no checksum for {asset_name}"))?;
    if matching.next().is_some()
        || expected.len() != 64
        || !expected.bytes().all(|byte| byte.is_ascii_hexdigit())
    {
        return Err(format!(
            "SHA256SUMS has an invalid checksum for {asset_name}"
        ));
    }
    let mut file = File::open(archive).map_err(|error| error.to_string())?;
    let mut hasher = Sha256::new();
    let mut buffer = [0; 64 * 1024];
    loop {
        let count = file.read(&mut buffer).map_err(|error| error.to_string())?;
        if count == 0 {
            break;
        }
        hasher.update(&buffer[..count]);
    }
    let actual = format!("{:x}", hasher.finalize());
    if !actual.eq_ignore_ascii_case(expected) {
        return Err(format!("SHA-256 checksum mismatch for {asset_name}"));
    }
    Ok(())
}

fn extract_binary(
    archive_path: &str,
    destination: &str,
    binary_name: &str,
    windows: bool,
) -> zed::Result<()> {
    let extract = || -> Result<(), Box<dyn std::error::Error>> {
        let file = File::open(archive_path)?;
        if windows {
            let mut archive = zip::ZipArchive::new(file)?;
            if archive.len() != 1 {
                return Err("Expected exactly one binary in the ZIP archive".into());
            }
            let mut entry = archive.by_index(0)?;
            if entry.name() != binary_name
                || entry.is_dir()
                || entry
                    .unix_mode()
                    .is_some_and(|mode| mode & 0o170000 != 0 && mode & 0o170000 != 0o100000)
            {
                return Err("ZIP archive does not contain the expected regular binary".into());
            }
            copy_binary(&mut entry, destination)?;
        } else {
            let mut archive = tar::Archive::new(flate2::read::GzDecoder::new(file));
            let mut entries = archive.entries()?;
            let mut entry = entries.next().ok_or("TAR archive is empty")??;
            if entry.path_bytes().as_ref() != binary_name.as_bytes()
                || !entry.header().entry_type().is_file()
            {
                return Err("TAR archive does not contain the expected regular binary".into());
            }
            copy_binary(&mut entry, destination)?;
            drop(entry);
            if entries.next().transpose()?.is_some() {
                return Err("Expected exactly one binary in the TAR archive".into());
            }
        }
        Ok(())
    };
    extract().map_err(|error| format!("Cannot safely extract {binary_name}: {error}"))
}

fn copy_binary(reader: &mut impl Read, destination: &str) -> io::Result<()> {
    // Never unpack archive paths, links, ownership or modes. Only copy bytes to this fixed path.
    let mut output = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(destination)?;
    let copied = io::copy(&mut reader.take(MAX_BINARY_SIZE + 1), &mut output)?;
    if copied == 0 || copied > MAX_BINARY_SIZE {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "Release binary is empty or exceeds the 128 MiB installation limit",
        ));
    }
    output.sync_all()
}

zed::register_extension!(GoImplView);
