{
  description = "gitknown - multi-repo/worktree git review dashboard";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }: flake-utils.lib.eachSystem
    [ "aarch64-darwin" "x86_64-darwin" "x86_64-linux" ]
    (
      system:
      let
        pkgs = import nixpkgs { inherit system; };

        # buildGoModule needs a version, but a hand-maintained literal would go
        # stale after the next tag. The released artifacts already take their
        # version from the git tag (GITHUB_REF_NAME in release.yml); here we just
        # track the source revision so the nix package name reflects what was
        # actually checked out, no maintenance required.
        version = self.shortRev or self.dirtyShortRev or "dev";

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
          # Stamp the same version var `just build` sets. A flake build can't see
          # git tags (no .git in the sandbox), only the rev, so a `nix run` binary
          # reports the source revision rather than a release tag.
          ldflags = [ "-X main.version=${version}" ];
          subPackages = [ "." ];
          # The watcher tests need git + real filesystem events, which don't
          # belong in a sealed build sandbox; CI (`just verify`) runs them.
          doCheck = false;
          meta = {
            description = "Multi-repo/worktree git WIP review dashboard";
            mainProgram = "gitknown";
          };
        };

        # Binary distribution: the prebuilt, tagged release pulled from GitHub
        # Releases instead of compiled from source. This is the goreleaser /
        # go-bin-flake pattern: the release workflow already builds, packs, and
        # publishes a per-platform tarball with the real git-tag version stamped
        # in, so `nix run github:denisraison/gitknown` fetches that exact binary
        # with no source rebuild (and no Linux build-from-source autoPatchelf
        # concerns). release.yml auto-commits this bump to main after each
        # release; `just release-hashes v<X.Y.Z>` regenerates the values for a
        # manual fix. Only the platforms the release targets appear; others fall
        # back to the source build above.
        binVersion = "0.3.4";
        binAssets = {
          x86_64-linux = {
            arch = "linux-amd64";
            sha256 = "a75991cd1999ba5919c1f4eb4a8cd163f6b4ebbfebc297f5bdd5316b2f6e8fd0";
          };
          aarch64-darwin = {
            arch = "darwin-arm64";
            sha256 = "dbf26a91413f892facc6659c8a7af422d88c2ec8bfaaf974922d3997d3cf56d5";
          };
        };
        binAsset = binAssets.${system} or null;
        gitknown-bin =
          if binAsset == null then null
          else pkgs.stdenvNoCC.mkDerivation {
            pname = "gitknown";
            version = binVersion;
            src = pkgs.fetchurl {
              url = "https://github.com/denisraison/gitknown/releases/download/v${binVersion}/gitknown-v${binVersion}-${binAsset.arch}.tar.gz";
              sha256 = binAsset.sha256;
            };
            sourceRoot = "."; # the tarball is the bare `gitknown` binary, no wrapping dir
            # The Linux binary is dynamically linked against glibc (Go's cgo net
            # resolver), so patch its interpreter to run on NixOS. The darwin
            # binary links system frameworks at fixed paths and needs no patching.
            nativeBuildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.autoPatchelfHook ];
            buildInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.stdenv.cc.cc.lib ];
            dontConfigure = true;
            dontBuild = true;
            installPhase = ''
              runHook preInstall
              install -Dm755 gitknown $out/bin/gitknown
              runHook postInstall
            '';
            meta = {
              description = "Multi-repo/worktree git WIP review dashboard (prebuilt release)";
              mainProgram = "gitknown";
            };
          };
      in
      {
        # default is the prebuilt tagged release where we publish one, so
        # `nix run github:denisraison/gitknown` just works for users; `.#gitknown`
        # always builds from source (what a local checkout actually has).
        packages = {
          inherit gitknown;
          default = if gitknown-bin != null then gitknown-bin else gitknown;
        } // pkgs.lib.optionalAttrs (gitknown-bin != null) {
          inherit gitknown-bin;
        };

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
