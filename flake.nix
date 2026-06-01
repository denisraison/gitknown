{
  description = "gitknown - multi-repo/worktree git review dashboard";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { nixpkgs, flake-utils, ... }: flake-utils.lib.eachSystem
    [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" ]
    (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go            # matches go.mod (1.26.x)
            nodejs        # for the Rolldown/Solid frontend
            just
            gopls
            wgo           # live reload for the Go backend (just dev)
            golangci-lint # 2.12.x, built with go1.26
            lefthook      # git hook manager (see lefthook.yml)
            git
          ];
        };
      }
    );
}
