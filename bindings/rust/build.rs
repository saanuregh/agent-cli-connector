//! Links the static agent-cli-connector library built by `zig build`.
use std::env;
use std::path::PathBuf;

fn main() {
    const LIB_DIR_VAR: &str = "AGENT_CLI_CONNECTOR_LIB_DIR";
    println!("cargo:rerun-if-env-changed={LIB_DIR_VAR}");
    let dir = match env::var_os(LIB_DIR_VAR) {
        Some(dir) => PathBuf::from(dir),
        None => PathBuf::from(env::var("CARGO_MANIFEST_DIR").unwrap()).join("../../zig-out/lib"),
    };
    let windows = env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("windows");
    let file = if windows {
        "agent-cli-connector.lib"
    } else {
        "libagent-cli-connector.a"
    };
    let lib = dir.join(file);
    if !lib.exists() {
        panic!(
            "agent-cli-connector: {} not found. Build it with `zig build -Doptimize=ReleaseFast` in the \
             repository root, or set {LIB_DIR_VAR} to the directory that holds it.",
            lib.display()
        );
    }
    println!("cargo:rerun-if-changed={}", lib.display());
    println!("cargo:rustc-link-search=native={}", dir.display());
    println!("cargo:rustc-link-lib=static=agent-cli-connector");
    if !windows {
        println!("cargo:rustc-link-lib=c");
    }
    // The Zig objects carry no .note.GNU-stack section, which GNU ld would
    // read as a request for an executable stack.
    if env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("linux") {
        println!("cargo:rustc-link-arg=-Wl,-z,noexecstack");
    }
}
