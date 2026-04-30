{
  description = "Graft - Your local environment, everywhere.";

  # Pinned for go_1_26 support (nixpkgs#497104)
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/09612cb5a0972cf82bfadb2a63872376bbf00d2d";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["x86_64-linux" "aarch64-linux" "aarch64-darwin"];
    forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
  in {
    packages = forAllSystems (pkgs: let
      go = pkgs.go_1_26;
      version = self.shortRev or self.dirtyShortRev or "nix";
    in rec {
      graft = pkgs.buildGoModule.override {inherit go;} {
        pname = "graft";
        inherit version;
        src = ./.;
        vendorHash = "sha256-RWDQ0+caE+KVwasOFO1uF81Sfs7QC0fb6sEfa5acMFs=";
        goSum = ./go.sum;
        # darwin needs cgo for mutagen's FSEvents watcher.
        env.CGO_ENABLED =
          if pkgs.stdenv.hostPlatform.isDarwin
          then "1"
          else "0";

        nativeBuildInputs = [pkgs.just pkgs.zstd];

        # Cross-compile and embed daemon binaries using the justfile
        preBuild = ''
          if [[ "$name" != *"go-modules"* ]]; then
            export BUILD_VERSION="${version}"
            just prepare-embedded
          fi
        '';

        tags = ["embed_binaries"];

        ldflags = [
          "-w"
          "-s"
          "-X github.com/edaniels/graft/pkg.buildVersion=${version}"
        ];
        subPackages = ["cmd/graft"];
        doCheck = false;
        meta.mainProgram = "graft";
      };
      default = graft;
    });

    overlays.default = final: prev: {
      graft = self.packages.${final.system}.graft;
    };
  };
}
