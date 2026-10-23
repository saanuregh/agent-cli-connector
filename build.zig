const std = @import("std");

const providers = [_][]const u8{ "claude", "codex", "pi" };

/// The platforms release files are built for. Bindings download the files
/// for their platform by these names (`<file>-<name>.<ext>`). `go_name` is
/// the Go module directory, `bindings/go/lib/<go_name>`, that holds the
/// platform's static library.
const release_platforms = [_]struct { name: []const u8, zig_target: []const u8, go_name: []const u8 }{
    .{ .name = "linux-x86_64", .zig_target = "x86_64-linux-gnu.2.28", .go_name = "linux-amd64" },
    .{ .name = "linux-aarch64", .zig_target = "aarch64-linux-gnu.2.28", .go_name = "linux-arm64" },
    .{ .name = "macos-x86_64", .zig_target = "x86_64-macos", .go_name = "darwin-amd64" },
    .{ .name = "macos-aarch64", .zig_target = "aarch64-macos", .go_name = "darwin-arm64" },
    .{ .name = "windows-x86_64", .zig_target = "x86_64-windows-gnu", .go_name = "windows-amd64" },
};

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});
    const strip = b.option(bool, "strip", "Leave out debug info (default: on for release builds)") orelse (optimize != .debug);
    const enabled = b.option([]const u8, "providers", "Comma-separated providers to build in (default: claude,codex,pi)") orelse "claude,codex,pi";
    // Left-out providers are replaced by an empty module.
    const no_provider = b.createModule(.{ .root_source_file = b.addWriteFiles().add("no_provider.zig", "") });

    const agent = b.addModule("agent", .{
        .root_source_file = b.path("src/root.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });

    // The C ABI as a static and a shared library, plus its header.
    const c_api = b.createModule(.{
        .root_source_file = b.path("src/c_api.zig"),
        .target = target,
        .optimize = optimize,
        .strip = strip,
        .link_libc = true,
        .imports = &.{.{ .name = "agent", .module = agent }},
    });
    const static_lib = b.addLibrary(.{ .name = "agent-cli-connector", .linkage = .static, .root_module = c_api });
    // Hosts link it with their own toolchain, which lacks Zig's runtime.
    static_lib.bundle_compiler_rt = true;
    static_lib.installHeader(b.path("include/agent.h"), "agent.h");
    b.installArtifact(static_lib);
    b.installArtifact(b.addLibrary(.{ .name = "agent-cli-connector", .linkage = .dynamic, .root_module = c_api }));

    // agentd: the same protocol over standard input and output.
    b.installArtifact(b.addExecutable(.{
        .name = "agentd",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/agentd.zig"),
            .target = target,
            .optimize = optimize,
            .strip = strip,
            .link_libc = true,
            .imports = &.{.{ .name = "agent", .module = agent }},
        }),
    }));

    // Examples.
    const oneshot = b.addExecutable(.{
        .name = "oneshot",
        .root_module = b.createModule(.{
            .root_source_file = b.path("examples/oneshot/main.zig"),
            .target = target,
            .optimize = optimize,
            .strip = strip,
            .imports = &.{.{ .name = "agent", .module = agent }},
        }),
    });
    b.installArtifact(oneshot);

    // Binding types and JSON Schema generated from the protocol types and the
    // providers' options, copied into the source tree. The generator reads
    // only those, so its copy of the core gets empty provider modules and
    // builds even while a provider does not.
    const protocol = b.createModule(.{ .root_source_file = b.path("src/root.zig"), .target = target, .link_libc = true });
    inline for (providers) |name| protocol.addImport(name, no_provider);
    const bindgen_module = b.createModule(.{
        .root_source_file = b.path("tools/bindgen.zig"),
        .target = target,
        .imports = &.{.{ .name = "agent", .module = protocol }},
    });
    // Each provider's options file depends only on the core types.
    inline for (providers) |name| bindgen_module.addImport(name ++ "_options", b.createModule(.{
        .root_source_file = b.path("src/providers/" ++ name ++ "/options.zig"),
        .target = target,
        .imports = &.{.{ .name = "agent", .module = protocol }},
    }));
    const bindgen = b.addRunArtifact(b.addExecutable(.{ .name = "bindgen", .root_module = bindgen_module }));
    const generated = bindgen.addOutputDirectoryArg("bindings");
    const update_bindings = b.addUpdateSourceFiles();
    update_bindings.addCopyFileToSource(generated.path(b, "protocol.schema.json"), "schema/protocol.schema.json");
    update_bindings.addCopyFileToSource(generated.path(b, "protocol.ts"), "bindings/typescript/src/protocol.ts");
    update_bindings.addCopyFileToSource(generated.path(b, "protocol.go"), "bindings/go/protocol.go");
    update_bindings.addCopyFileToSource(generated.path(b, "protocol.rs"), "bindings/rust/src/protocol.rs");
    update_bindings.addCopyFileToSource(generated.path(b, "methods.ts"), "bindings/typescript/src/methods.ts");
    update_bindings.addCopyFileToSource(generated.path(b, "methods.go"), "bindings/go/methods.go");
    update_bindings.addCopyFileToSource(generated.path(b, "methods.rs"), "bindings/rust/src/methods.rs");
    b.step("bindgen", "Generate the binding types from the protocol types").dependOn(&update_bindings.step);

    // `zig build release`: the libraries, agentd, and header for every
    // platform the bindings download, in zig-out/release.
    const release = b.step("release", "Build the release files for every platform into zig-out/release");
    release.dependOn(&b.addInstallFileWithDir(b.path("include/agent.h"), .{ .custom = "release" }, "agent.h").step);
    // `zig build go-libs`: the release static libraries and the header,
    // copied into the Go binding's platform modules, which link them with cgo.
    // Windows' `.lib` is a GNU archive, so it keeps the `.a` name cgo expects.
    const go_libs = b.addUpdateSourceFiles();
    go_libs.addCopyFileToSource(b.path("include/agent.h"), "bindings/go/agent.h");
    for (release_platforms) |platform| {
        const query = std.Target.Query.parse(.{ .arch_os_abi = platform.zig_target }) catch @panic("bad release target");
        const release_target = b.resolveTargetQuery(query);
        const release_static_lib = addReleaseFiles(b, release, release_target, platform.name);
        const go_lib = localizeLibcSymbols(b, release_static_lib.getEmittedBin(), release_target.result);
        go_libs.addCopyFileToSource(go_lib, b.fmt("bindings/go/lib/{s}/libagent-cli-connector.a", .{platform.go_name}));
    }
    b.step("go-libs", "Copy the release static libraries and header into bindings/go").dependOn(&go_libs.step);

    const test_step = b.step("test", "Run all tests");
    test_step.dependOn(&b.addRunArtifact(b.addTest(.{ .root_module = c_api })).step);
    const core_tests = b.addRunArtifact(b.addTest(.{ .root_module = agent }));
    test_step.dependOn(&core_tests.step);

    // Each provider is its own module, importing the core as "agent", so a
    // provider is compiled and tested on its own.
    inline for (providers) |name| {
        const mod = b.createModule(.{
            .root_source_file = b.path("src/providers/" ++ name ++ "/root.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
        });
        mod.addImport("agent", agent);
        const on = std.mem.indexOf(u8, enabled, name) != null;
        agent.addImport(name, if (on) mod else no_provider);

        const run = b.addRunArtifact(b.addTest(.{ .root_module = mod }));
        b.step("test-" ++ name, "Run the " ++ name ++ " provider tests").dependOn(&run.step);
        if (on) test_step.dependOn(&run.step);
    }
}

/// A copy of a static library whose bundled compiler-rt no longer exports the
/// libc and libm functions it defines (memcpy, strlen, sin, ...). Exported, they
/// would replace the system C library's for every C object in a Go program
/// that links the library. Only agent_*, the compiler's `__` helpers, and the
/// rounding functions the library calls (round, roundl on aarch64 Linux,
/// roundf128) stay global; the system libm is not linked by cgo. Needs llvm-objcopy 20
/// or later (older ones refuse Mach-O), as `zig objcopy` cannot localize
/// symbols. llvm-objcopy cannot for COFF either,
/// so a Windows library is returned unchanged.
fn localizeLibcSymbols(b: *std.Build, static_lib: std.Build.LazyPath, target: std.Target) std.Build.LazyPath {
    if (target.os.tag == .windows) return static_lib;
    const prefix = if (target.os.tag.isDarwin()) "_" else "";
    // Found when go-libs runs, so other steps do not need it. Homebrew's llvm
    // is keg-only and LLVM's Windows installer may leave PATH alone; looking
    // in their directories keeps their clang off cgo's PATH.
    const llvm_objcopy = b.findProgramLazy(.{ .names = &.{
        "llvm-objcopy",
        "/opt/homebrew/opt/llvm/bin/llvm-objcopy",
        "/usr/local/opt/llvm/bin/llvm-objcopy",
        "C:\\Program Files\\LLVM\\bin\\llvm-objcopy",
    } });
    const objcopy = std.Build.Step.Run.create(b, "llvm-objcopy");
    objcopy.addFileArg(llvm_objcopy);
    objcopy.addArg("--wildcard");
    for ([_][]const u8{ "agent_*", "__*", "round", "roundl", "roundf128" }) |kept| {
        objcopy.addArg(b.fmt("--localize-symbol=!{s}{s}", .{ prefix, kept }));
    }
    objcopy.addArg("--localize-symbol=*");
    objcopy.addFileArg(static_lib);
    return objcopy.addOutputFileArg("libagent-cli-connector.a");
}

/// One platform's release files: the shared and static libraries and
/// agentd, built with every provider. Returns the static library.
fn addReleaseFiles(b: *std.Build, step: *std.Build.Step, target: std.Build.ResolvedTarget, platform: []const u8) *std.Build.Step.Compile {
    const optimize: std.builtin.OptimizeMode = .ReleaseFast;
    const agent = b.createModule(.{ .root_source_file = b.path("src/root.zig"), .target = target, .optimize = optimize, .link_libc = true });
    inline for (providers) |name| {
        const mod = b.createModule(.{
            .root_source_file = b.path("src/providers/" ++ name ++ "/root.zig"),
            .target = target,
            .optimize = optimize,
            .link_libc = true,
        });
        mod.addImport("agent", agent);
        agent.addImport(name, mod);
    }
    const c_api = b.createModule(.{
        .root_source_file = b.path("src/c_api.zig"),
        .target = target,
        .optimize = optimize,
        .strip = true,
        .link_libc = true,
        .imports = &.{.{ .name = "agent", .module = agent }},
    });
    const static_lib = b.addLibrary(.{ .name = "agent-cli-connector", .linkage = .static, .root_module = c_api });
    static_lib.bundle_compiler_rt = true;
    const shared_lib = b.addLibrary(.{ .name = "agent-cli-connector", .linkage = .dynamic, .root_module = c_api });
    const agentd = b.addExecutable(.{
        .name = "agentd",
        .root_module = b.createModule(.{
            .root_source_file = b.path("src/agentd.zig"),
            .target = target,
            .optimize = optimize,
            .strip = true,
            .link_libc = true,
            .imports = &.{.{ .name = "agent", .module = agent }},
        }),
    });

    const os = target.result.os.tag;
    const lib_prefix = if (os == .windows) "" else "lib";
    const shared_ext = switch (os) {
        .windows => "dll",
        .macos => "dylib",
        else => "so",
    };
    const files = [_]struct { artifact: *std.Build.Step.Compile, name: []const u8 }{
        .{ .artifact = shared_lib, .name = b.fmt("{s}agent-cli-connector-{s}.{s}", .{ lib_prefix, platform, shared_ext }) },
        .{ .artifact = static_lib, .name = b.fmt("{s}agent-cli-connector-{s}.{s}", .{ lib_prefix, platform, if (os == .windows) "lib" else "a" }) },
        .{ .artifact = agentd, .name = b.fmt("agentd-{s}{s}", .{ platform, if (os == .windows) ".exe" else "" }) },
    };
    for (files) |f| {
        step.dependOn(&b.addInstallArtifact(f.artifact, .{
            .dest_dir = .{ .override = .{ .custom = "release" } },
            .dest_sub_path = f.name,
            .implib_dir = .disabled,
            .pdb_dir = .disabled,
            .h_dir = .disabled,
        }).step);
    }
    return static_lib;
}
