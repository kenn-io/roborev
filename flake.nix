{
  description = "roborev - automatic code review daemon for git commits";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          goPinned = pkgs.go_1_27.overrideAttrs (_: rec {
            version = "1.27.0";
            src = pkgs.fetchurl {
              url = "https://go.dev/dl/go${version}.src.tar.gz";
              hash = "sha256-cAJAPXzERSnvbSb2mkSBgmM5Xq18FsBaWAiuBH6+sOU=";
            };
          });
          buildGoModule = pkgs.buildGoModule.override { go = goPinned; };
        in
        {
          default = buildGoModule {
            pname = "roborev";
            version = "0.67.0";

            src = ./.;

            vendorHash = "sha256-A5ZcODNZyjMTTFI8QZqaYn0Wddr8+899R+C1n27TI1U=";

            subPackages = [ "cmd/roborev" ];

            nativeCheckInputs = [ pkgs.git ];

            meta = with pkgs.lib; {
              description = "Automatic code review daemon for git commits";
              homepage = "https://github.com/roborev-dev/roborev";
              license = licenses.mit;
              mainProgram = "roborev";
            };
          };
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/roborev";
        };
        roborev = self.apps.${system}.default;
      });

      formatter = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
        in
        pkgs.nixfmt
      );

      devShells = forAllSystems (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          goPinned = pkgs.go_1_27.overrideAttrs (_: rec {
            version = "1.27.0";
            src = pkgs.fetchurl {
              url = "https://go.dev/dl/go${version}.src.tar.gz";
              hash = "sha256-cAJAPXzERSnvbSb2mkSBgmM5Xq18FsBaWAiuBH6+sOU=";
            };
          });
        in
        {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              goPinned
              gopls
              gotools
            ];
          };
        }
      );
    };
}
