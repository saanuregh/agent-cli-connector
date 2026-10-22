const std = @import("std");

const providers = [_][]const u8{ "claude", "codex", "pi" };

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
