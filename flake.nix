{
  description = "Graft - Your local environment, everywhere.";

  # Pinned for go_1_26 support (nixpkgs#497104)
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/09612cb5a0972cf82bfadb2a63872376bbf00d2d";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
    forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
  in {
    packages = forAllSystems (pkgs: rec {
      graft = pkgs.buildGoModule.override {go = pkgs.go_1_26;} {
        pname = "graft";
        version = self.shortRev or "dev";
        src = ./.;
        vendorHash = "sha256-1yH+Ln8H4+1pku0J2guZ/PlBnPqICmQjYoUbJMLxDbo=";
        goSum = ./go.sum;
        env.CGO_ENABLED = "0";
        ldflags = ["-w" "-s"];
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
