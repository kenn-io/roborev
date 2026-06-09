{
  description = "roborev - automatic code review daemon for git commits";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages = {
          default = pkgs.buildGoModule {
            pname = "roborev";
            version = "0.57.1";
            go = pkgs.go_1_26;

            src = ./.;

            vendorHash = "sha256-FB1T8h9VwqZO7mzmtbHawwt1ktf6Zs2LASQL2uXQ8Q4=";

            subPackages = [ "cmd/roborev" ];

            nativeCheckInputs = [ pkgs.git ];

            meta = with pkgs.lib; {
              description = "Automatic code review daemon for git commits";
              homepage = "https://github.com/roborev-dev/roborev";
              license = licenses.mit;
              mainProgram = "roborev";
            };
          };
        };

        apps = {
          default = flake-utils.lib.mkApp {
            drv = self.packages.${system}.default;
            exePath = "/bin/roborev";
          };
          roborev = self.apps.${system}.default;
        };

        formatter = pkgs.nixfmt;

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go_1_26
            gopls
            gotools
          ];
        };
      }
    );
}
