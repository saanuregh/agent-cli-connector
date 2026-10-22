{
  description = "agent-cli-connector: one API over coding-agent CLIs";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { nixpkgs, ... }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forEachSystem = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      # The toolchains for the library and every binding. The harness CLIs
      # (claude, codex, pi) come from their own installers.
      devShells = forEachSystem (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            zig_0_17
            zls
            go
            gopls
            cargo
            rustc
            clippy
            rustfmt
            rust-analyzer
            nodejs_24
          ];
        };
      });

      formatter = forEachSystem (pkgs: pkgs.nixfmt);
    };
}
