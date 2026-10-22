//! Links the static agent-cli-connector library. It looks, in order, in
//! `AGENT_CLI_CONNECTOR_LIB_DIR`, in the repository's `zig-out/lib`, in a
//! fresh `zig build` of the repository, and in the GitHub release for this
//! crate's version.
use std::env;
use std::fs;
use std::io::ErrorKind;
use std::path::{Path, PathBuf};
use std::process::Command;

use sha2::{Digest, Sha256};

const LIB_DIR_VAR: &str = "AGENT_CLI_CONNECTOR_LIB_DIR";
const RELEASE_URL_VAR: &str = "AGENT_CLI_CONNECTOR_RELEASE_URL";
const NO_DOWNLOAD_VAR: &str = "AGENT_CLI_CONNECTOR_NO_DOWNLOAD";

/// A release platform: its name in release file names and its Zig target,
/// which matches `release_platforms` in the repository's `build.zig`.
struct Platform {
    name: &'static str,
    zig_target: &'static str,
}

fn main() {
    println!("cargo:rerun-if-changed=build.rs");
    for var in [LIB_DIR_VAR, RELEASE_URL_VAR, NO_DOWNLOAD_VAR] {
        println!("cargo:rerun-if-env-changed={var}");
    }
    let target_os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let target_env = env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();
    let windows = target_os == "windows";
    if windows && target_env == "msvc" {
        println!(
            "cargo:warning=agent-cli-connector: the library is built for the GNU ABI; linking it into an MSVC \
             target may fail. Use the x86_64-pc-windows-gnu Rust target if it does."
        );
    }
    let file = if windows {
        "agent-cli-connector.lib"
    } else {
        "libagent-cli-connector.a"
    };

    let dir = find_library(file, windows).unwrap_or_else(|reasons| {
        panic!(
            "agent-cli-connector: could not get the static library {file}.\n{}\n\
             Get it in one of these ways:\n\
             1. Set {LIB_DIR_VAR} to a directory that holds {file}.\n\
             2. Run `zig build -Doptimize=ReleaseFast` in the repository root, which creates zig-out/lib.\n\
             3. Put Zig on PATH while building from a repository checkout (a path or git dependency), \
             and this build script runs `zig build` itself.\n\
             4. Let this build script download it from the GitHub release: leave {NO_DOWNLOAD_VAR} unset, \
             and set {RELEASE_URL_VAR} only to download from a different base URL.",
            reasons.join("\n")
        )
    });
    println!("cargo:rustc-link-search=native={}", dir.display());
    println!("cargo:rustc-link-lib=static=agent-cli-connector");
    if !windows {
        println!("cargo:rustc-link-lib=c");
    }
    // The Zig objects carry no .note.GNU-stack section, which GNU ld would
    // read as a request for an executable stack.
    if target_os == "linux" {
        println!("cargo:rustc-link-arg=-Wl,-z,noexecstack");
    }
}

/// Returns the directory holding `file`, or why each way to get it failed.
fn find_library(file: &str, windows: bool) -> Result<PathBuf, Vec<String>> {
    if let Some(dir) = env::var_os(LIB_DIR_VAR) {
        let dir = PathBuf::from(dir);
        let lib = dir.join(file);
        if lib.exists() {
            println!("cargo:rerun-if-changed={}", lib.display());
            return Ok(dir);
        }
        return Err(vec![format!(
            "{LIB_DIR_VAR} is set, but {} does not exist.",
            lib.display()
        )]);
    }

    let manifest_dir = PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap());
    let repo_root = manifest_dir.join("../..");
    let zig_out = repo_root.join("zig-out/lib");
    let lib = zig_out.join(file);
    if lib.exists() {
        println!("cargo:rerun-if-changed={}", lib.display());
        return Ok(zig_out);
    }
    let mut reasons = vec![format!("{} does not exist.", lib.display())];

    let out_dir = PathBuf::from(env::var("OUT_DIR").unwrap());
    let platform = platform();
    match &platform {
        Ok(platform) => match zig_build(&repo_root, &out_dir, platform, file) {
            Ok(dir) => return Ok(dir),
            Err(reason) => reasons.push(reason),
        },
        Err(reason) => reasons.push(format!("Not building with Zig: {reason}")),
    }

    if env::var(NO_DOWNLOAD_VAR).as_deref() == Ok("1") {
        reasons.push(format!(
            "Downloading is off because {NO_DOWNLOAD_VAR}=1; set {LIB_DIR_VAR} instead."
        ));
        return Err(reasons);
    }
    let platform = match platform {
        Ok(platform) => platform,
        Err(reason) => {
            reasons.push(format!(
                "No release download: {reason} Build the library with Zig and set {LIB_DIR_VAR}."
            ));
            return Err(reasons);
        }
    };
    match download(&out_dir, &platform, file, windows) {
        Ok(dir) => Ok(dir),
        Err(reason) => {
            reasons.push(reason);
            Err(reasons)
        }
    }
}

/// The release platform for the cargo target, or why the release has none.
fn platform() -> Result<Platform, String> {
    let os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    let arch = env::var("CARGO_CFG_TARGET_ARCH").unwrap_or_default();
    let abi = env::var("CARGO_CFG_TARGET_ENV").unwrap_or_default();
    let (name, zig_target) = match (os.as_str(), arch.as_str(), abi.as_str()) {
        ("linux", "x86_64", "gnu") => ("linux-x86_64", "x86_64-linux-gnu.2.28"),
        ("linux", "aarch64", "gnu") => ("linux-aarch64", "aarch64-linux-gnu.2.28"),
        ("linux", _, "musl") => {
            return Err(
                "the release covers Linux with glibc (2.28 or newer) only, not musl.".to_string(),
            );
        }
        ("macos", "x86_64", _) => ("macos-x86_64", "x86_64-macos"),
        ("macos", "aarch64", _) => ("macos-aarch64", "aarch64-macos"),
        ("windows", "x86_64", _) => ("windows-x86_64", "x86_64-windows-gnu"),
        _ => {
            let target = env::var("TARGET").unwrap_or_default();
            return Err(format!(
                "the release has no library for the target {target}."
            ));
        }
    };
    Ok(Platform { name, zig_target })
}

/// Builds the library with `zig build` into `OUT_DIR`, if Zig is on `PATH`
/// and the crate sits inside a repository checkout.
fn zig_build(
    repo_root: &Path,
    out_dir: &Path,
    platform: &Platform,
    file: &str,
) -> Result<PathBuf, String> {
    if !repo_root.join("build.zig").exists() {
        return Err(format!(
            "Not building with Zig: {} has no build.zig.",
            repo_root.display()
        ));
    }
    let prefix = out_dir.join("zig");
    // The cache goes in OUT_DIR so the build writes nothing in the checkout.
    let output = Command::new("zig")
        .current_dir(repo_root)
        .arg("build")
        .arg("-Doptimize=ReleaseFast")
        .arg(format!("-Dtarget={}", platform.zig_target))
        .arg("--prefix")
        .arg(&prefix)
        .arg("--cache-dir")
        .arg(out_dir.join("zig-cache"))
        .output();
    let output = match output {
        Ok(output) => output,
        Err(err) if err.kind() == ErrorKind::NotFound => {
            return Err("Not building with Zig: zig is not on PATH.".to_string());
        }
        Err(err) => return Err(format!("Could not run zig: {err}")),
    };
    if !output.status.success() {
        return Err(format!(
            "`zig build` failed ({}):\n{}",
            output.status,
            String::from_utf8_lossy(&output.stderr).trim_end()
        ));
    }
    let lib_dir = prefix.join("lib");
    if !lib_dir.join(file).exists() {
        return Err(format!(
            "`zig build` did not create {}.",
            lib_dir.join(file).display()
        ));
    }
    // Watch only the build's inputs that exist: a missing path would make
    // cargo rerun this script on every build.
    for input in ["build.zig", "build.zig.zon", "src", "include"] {
        let path = repo_root.join(input);
        if path.exists() {
            println!("cargo:rerun-if-changed={}", path.display());
        }
    }
    Ok(lib_dir)
}

/// Downloads the static library from the release into `OUT_DIR`, checks it
/// against the release's `SHA256SUMS`, and stores it under the name `file`
/// that the link line expects.
fn download(
    out_dir: &Path,
    platform: &Platform,
    file: &str,
    windows: bool,
) -> Result<PathBuf, String> {
    let mut base = match env::var(RELEASE_URL_VAR) {
        Ok(url) if !url.is_empty() => url,
        _ => format!(
            "https://github.com/saanuregh/agent-cli-connector/releases/download/v{}/",
            env::var("CARGO_PKG_VERSION").unwrap()
        ),
    };
    if !base.ends_with('/') {
        base.push('/');
    }
    let release_file = if windows {
        format!("agent-cli-connector-{}.lib", platform.name)
    } else {
        format!("libagent-cli-connector-{}.a", platform.name)
    };

    let sums_url = format!("{base}SHA256SUMS");
    let sums = curl(&["-fsSL", &sums_url])?;
    let sums = String::from_utf8_lossy(&sums);
    let expected = sums
        .lines()
        .filter_map(|line| line.split_once("  ").or_else(|| line.split_once(" *")))
        .find(|(_, name)| name.trim() == release_file)
        .map(|(hash, _)| hash.trim().to_ascii_lowercase())
        .ok_or_else(|| format!("{sums_url} has no line for {release_file}."))?;

    let dir = out_dir.join("download");
    fs::create_dir_all(&dir).map_err(|err| format!("Could not create {}: {err}", dir.display()))?;
    let final_path = dir.join(file);
    let _ = fs::remove_file(&final_path);
    let part = dir.join(format!("{file}.part"));
    let url = format!("{base}{release_file}");
    let part_arg = part.to_string_lossy();
    if let Err(err) = curl(&["-fsSL", "-o", &part_arg, &url]) {
        let _ = fs::remove_file(&part);
        return Err(err);
    }
    let bytes =
        fs::read(&part).map_err(|err| format!("Could not read {}: {err}", part.display()))?;
    let actual: String = Sha256::digest(&bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect();
    if actual != expected {
        let _ = fs::remove_file(&part);
        return Err(format!(
            "{url} does not match SHA256SUMS: expected {expected}, got {actual}. Nothing was kept."
        ));
    }
    fs::rename(&part, &final_path)
        .map_err(|err| format!("Could not move {} into place: {err}", part.display()))?;
    Ok(dir)
}

/// Runs `curl` with `args` and returns its standard output.
fn curl(args: &[&str]) -> Result<Vec<u8>, String> {
    let url = args.last().copied().unwrap_or_default();
    let output = Command::new("curl")
        .args(args)
        .output()
        .map_err(|err| format!("Could not run curl to download {url}: {err}"))?;
    if !output.status.success() {
        return Err(format!(
            "Downloading {url} failed ({}): {}",
            output.status,
            String::from_utf8_lossy(&output.stderr).trim_end()
        ));
    }
    Ok(output.stdout)
}
