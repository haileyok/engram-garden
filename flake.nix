{
  description = "Engram Garden: shared memory for AI agents on ATProto Spaces";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.3.0-${self.shortRev or self.dirtyShortRev or "dev"}";
    in
    {
      packages = forAll (pkgs: rec {
        # The agent tools: engram (the CLI, which also runs spaces) and
        # engram-mcp (the MCP server).
        engram = pkgs.buildGoModule {
          pname = "engram";
          inherit version;
          src = self;
          vendorHash = "sha256-NTMz4olzOB9vQGJPim4iCYgkI1KyYPBfr0ETkXcvqf8=";
          subPackages = [ "cmd/engram" "cmd/engram-mcp" ];
          env.CGO_ENABLED = 0;
          ldflags = [ "-s" "-w" ];
          # The tests start in-process servers; run them with `go test`.
          doCheck = false;
          meta = {
            description = "Store and recall memories in an Engram Garden memory space";
            homepage = "https://engram.garden";
            mainProgram = "engram";
          };
        };
        default = engram;
      });

      apps = forAll (pkgs: {
        default = { type = "app"; program = "${self.packages.${pkgs.system}.engram}/bin/engram"; };
        engram-mcp = { type = "app"; program = "${self.packages.${pkgs.system}.engram}/bin/engram-mcp"; };
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls pkgs.nodejs pkgs.pnpm ]; };
      });
    };
}
