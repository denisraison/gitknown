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

        version = "0.1.0";

        # Frontend bundle (Vite/Rolldown). The Go binary embeds this dir via
        # //go:embed, so the package builds it first, then hands it to the Go
        # build below. npmDepsHash must be refreshed when web/package-lock.json
        # changes: `nix run nixpkgs#prefetch-npm-deps -- web/package-lock.json`.
        frontend = pkgs.buildNpmPackage {
          pname = "gitknown-web";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-eTbhF0bFVHe7Zcnxk/hrbYPqFtg16zfzYzSdw2yNOi8=";
          # `npm run build` (the default build) emits web/dist; ship just that.
          installPhase = ''
            runHook preInstall
            cp -r dist $out
            runHook postInstall
          '';
        };

        # The single embedding binary. vendorHash must be refreshed when
        # go.mod/go.sum change (build once with a wrong hash; nix prints the
        # right one).
        gitknown = pkgs.buildGoModule {
          pname = "gitknown";
          inherit version;
          src = ./.;
          vendorHash = "sha256-jsHSkYmrilhR1wRQ5cmCLsKdxtVU07hKtluR47pmOKs=";
          # web/dist is gitignored, so it isn't in the flake source. Drop the
          # built frontend in place before the //go:embed compile.
          preBuild = ''
            mkdir -p web/dist
            cp -r ${frontend}/* web/dist/
          '';
          subPackages = [ "." ];
          # The watcher tests need git + real filesystem events, which don't
          # belong in a sealed build sandbox; CI (`just verify`) runs them.
          doCheck = false;
          meta = {
            description = "Multi-repo/worktree git WIP review dashboard";
            mainProgram = "gitknown";
          };
        };
      in
      {
        packages.default = gitknown;
        packages.gitknown = gitknown;

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
