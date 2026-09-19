{
  description = "Let agents efficiently read web sources themselves, with Exa and Jev";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = {
    self,
    nixpkgs,
  }: let
    inherit (nixpkgs) lib;
    forAllSystems = lib.genAttrs ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
  in {
    packages = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = pkgs.buildGoModule {
        pname = "quarry";
        version = self.shortRev or self.dirtyShortRev or "dev";
        src = self;
        vendorHash = "sha256-JLuEfmJCH5nYBlucAyhYZBl+pG6Wxk3eliWMSap6VPc=";
        env.CGO_ENABLED = 0;
        ldflags = ["-s" "-w"];
        nativeBuildInputs = [pkgs.makeWrapper];
        # pdftotext reads the PDFs a fetch saves
        postInstall = ''
          wrapProgram $out/bin/quarry --prefix PATH : ${lib.makeBinPath [pkgs.poppler-utils]}
        '';
        meta = {
          description = "Let agents efficiently read web sources themselves, with Exa and Jev";
          homepage = "https://github.com/jackboykin/quarry";
          license = lib.licenses.mit;
          mainProgram = "quarry";
        };
      };
    });

    devShells = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
    in {
      default = pkgs.mkShell {packages = [pkgs.go pkgs.typescript pkgs.biome];};
    });

    overlays.default = final: _: {quarry = self.packages.${final.stdenv.hostPlatform.system}.default;};

    formatter = forAllSystems (system: nixpkgs.legacyPackages.${system}.alejandra);
  };
}
