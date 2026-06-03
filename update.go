package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Autoupdate keeps a self-managed install current with GitHub releases, the way
// `claude` updates itself: on by default, checked in the background, applied on
// the next start (never a mid-run restart). It is deliberately inert unless the
// running binary lives under the self-managed dir, so a `nix run` / dev build,
// or the immutable /nix/store copy, is never touched: those are updated by their
// own toolchain. Set GITKNOWN_NO_AUTOUPDATE to disable it entirely.
const (
	updateSlug     = "denisraison/gitknown"
	updateInterval = 6 * time.Hour // releases are infrequent; a slow poll is plenty
	maxBinarySize  = 200 << 20     // ceiling on the extracted binary, a decompression-bomb guard
)

var updateClient = &http.Client{Timeout: 2 * time.Minute} // the packed binary is a few MB

// updateConfig configures the self-updater. apiBase/dlBase point at GitHub and
// are overridden only in tests.
type updateConfig struct {
	dir     string // self-managed install root; binaries + the `current` symlink live in dir/bin
	current string // this binary's version, e.g. "v0.3.3"
	apiBase string
	dlBase  string
}

func (c updateConfig) api() string {
	if c.apiBase != "" {
		return c.apiBase
	}
	return "https://api.github.com"
}

func (c updateConfig) dl() string {
	if c.dlBase != "" {
		return c.dlBase
	}
	return "https://github.com"
}

// defaultUpdateDir is where staged updates live: $XDG_DATA_HOME/gitknown, else
// ~/.local/share/gitknown. Empty when no home dir resolves (autoupdate then off).
func defaultUpdateDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "gitknown")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "gitknown")
}

// selfManaged reports whether this binary is the one autoupdate owns: it lives
// under dir/bin (reached via the `current` symlink). A /nix/store or dev-tree
// binary is not, so autoupdate leaves it alone.
func selfManaged(dir string) bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	binDir := filepath.Join(dir, "bin")
	if resolved, rerr := filepath.EvalSymlinks(binDir); rerr == nil {
		binDir = resolved
	}
	return strings.HasPrefix(exe, binDir+string(os.PathSeparator))
}

// autoUpdate polls for a newer release and stages it; it never exits the process
// (the new binary goes live on the next start). It returns immediately, doing
// nothing, when the install isn't self-managed, the platform has no published
// release, or this isn't a clean release build.
func autoUpdate(ctx context.Context, cfg updateConfig) {
	if cfg.dir == "" || !selfManaged(cfg.dir) {
		return // nix store / dev build: not ours to update
	}
	asset, ok := releaseAsset()
	if !ok {
		log.Printf("gitknown: autoupdate off: no release build for %s/%s", runtime.GOOS, runtime.GOARCH)
		return
	}
	if _, ok := parseSemver(cfg.current); !ok {
		log.Printf("gitknown: autoupdate off: %q is not a release version", cfg.current)
		return
	}
	log.Printf("gitknown: autoupdate on: tracking %s releases (current %s)", updateSlug, cfg.current)

	check := func() {
		tag, err := stageUpdate(ctx, cfg, asset)
		if err != nil {
			log.Printf("gitknown: autoupdate: %v", err)
			return
		}
		if tag != "" {
			log.Printf("gitknown: autoupdate: staged %s, active on next restart", tag)
		}
	}
	check()
	t := time.NewTicker(updateInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			check()
		}
	}
}

// stageUpdate fetches the latest release tag and, when it is newer than the
// running version, downloads + verifies the platform tarball and points the
// `current` symlink at it. Returns the staged tag, or "" when nothing changed.
func stageUpdate(ctx context.Context, cfg updateConfig, asset string) (string, error) {
	latest, err := latestTag(ctx, cfg)
	if err != nil {
		return "", err
	}
	if !newer(cfg.current, latest) {
		return "", nil
	}
	binDir := filepath.Join(cfg.dir, "bin")
	name := "gitknown-" + latest
	if currentPointsAt(binDir, name) {
		return "", nil // already staged on a previous check
	}
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		return "", err
	}
	target := filepath.Join(binDir, name)
	if _, err := os.Stat(target); err != nil {
		if err := downloadRelease(ctx, cfg, latest, asset, target); err != nil {
			return "", err
		}
		if err := sanityCheck(ctx, target, latest); err != nil {
			if rmErr := os.Remove(target); rmErr != nil {
				log.Printf("gitknown: autoupdate: remove bad download %s: %v", target, rmErr)
			}
			return "", err
		}
	}
	if err := swapCurrent(binDir, name); err != nil {
		return "", err
	}
	return latest, nil
}

// latestTag returns the tag_name of the repo's latest GitHub release.
func latestTag(ctx context.Context, cfg updateConfig) (string, error) {
	body, err := fetchBytes(ctx, cfg.api()+"/repos/"+updateSlug+"/releases/latest")
	if err != nil {
		return "", err
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", fmt.Errorf("autoupdate: decode release: %w", err)
	}
	if rel.TagName == "" {
		return "", errors.New("autoupdate: release has no tag_name")
	}
	return rel.TagName, nil
}

// downloadRelease fetches the release tarball, checks it against the published
// sha256, and extracts the gitknown binary to target.
func downloadRelease(ctx context.Context, cfg updateConfig, tag, asset, target string) error {
	base := cfg.dl() + "/" + updateSlug + "/releases/download/" + tag + "/gitknown-" + tag + "-" + asset + ".tar.gz"
	sumBody, err := fetchBytes(ctx, base+".sha256")
	if err != nil {
		return err
	}
	fields := strings.Fields(string(sumBody))
	if len(fields) == 0 {
		return fmt.Errorf("autoupdate: empty checksum for %s", tag)
	}
	want := fields[0]
	tarball, err := fetchBytes(ctx, base)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(tarball)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("autoupdate: checksum mismatch for %s: got %s want %s", tag, got, want)
	}
	return extractBinary(tarball, target)
}

// extractBinary pulls the `gitknown` entry out of the gzipped tarball and writes
// it to target (via a temp file + rename, so target is never half-written).
func extractBinary(targz []byte, target string) error {
	gz, err := gzip.NewReader(bytes.NewReader(targz))
	if err != nil {
		return fmt.Errorf("autoupdate: gunzip: %w", err)
	}
	defer func() {
		if cerr := gz.Close(); cerr != nil {
			log.Printf("gitknown: autoupdate: close gzip reader: %v", cerr)
		}
	}()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("autoupdate: read tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "gitknown" {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".bin-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		_, copyErr := io.Copy(tmp, io.LimitReader(tr, maxBinarySize)) // LimitReader: decompression-bomb guard
		closeErr := tmp.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			removeTemp(tmpName)
			return fmt.Errorf("autoupdate: write binary: %w", err)
		}
		if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // G302: it's an executable, it must be runnable
			removeTemp(tmpName)
			return err
		}
		if err := os.Rename(tmpName, target); err != nil {
			removeTemp(tmpName)
			return err
		}
		return nil
	}
	return errors.New("autoupdate: gitknown binary not found in release tarball")
}

// sanityCheck refuses to stage a binary that won't run or reports the wrong
// version, so a corrupt download can never become the `current` target.
func sanityCheck(ctx context.Context, bin, want string) error {
	out, err := exec.CommandContext(ctx, bin, "-version").Output() //nolint:gosec // G204: bin is the path we just staged, not user input
	if err != nil {
		return fmt.Errorf("autoupdate: staged binary failed to run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != want {
		return fmt.Errorf("autoupdate: staged binary reports %q, want %q", got, want)
	}
	return nil
}

// currentPointsAt reports whether dir/current already resolves to name.
func currentPointsAt(binDir, name string) bool {
	dest, err := os.Readlink(filepath.Join(binDir, "current"))
	if err != nil {
		return false
	}
	return filepath.Base(dest) == name
}

// swapCurrent atomically points binDir/current at name (a sibling filename, so
// the link stays valid if the dir moves).
func swapCurrent(binDir, name string) error {
	tmp := filepath.Join(binDir, ".current.tmp")
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(name, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(binDir, "current")); err != nil {
		removeTemp(tmp)
		return err
	}
	return nil
}

func removeTemp(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("gitknown: autoupdate: clean temp %s: %v", path, err)
	}
}

// fetchBytes GETs url and returns its body, bounded so a hostile response can't
// exhaust memory.
func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gitknown-autoupdate") // GitHub rejects requests without one
	resp, err := updateClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			log.Printf("gitknown: autoupdate: close response body: %v", cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("autoupdate: GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxBinarySize))
}

// releaseAsset maps the running platform to the release's per-platform suffix,
// or false when this platform isn't a published build.
func releaseAsset() (string, bool) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "darwin-arm64", true
	case "linux/amd64":
		return "linux-amd64", true
	}
	return "", false
}

// newer reports whether latest is a strictly higher release than current. A
// non-release version on either side (dev build, describe suffix) is never newer,
// so a source/dev build is never auto-replaced by a release.
func newer(current, latest string) bool {
	c, ok := parseSemver(current)
	if !ok {
		return false
	}
	l, ok := parseSemver(latest)
	if !ok {
		return false
	}
	for i := range 3 {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// parseSemver parses a clean "vX.Y.Z" (or "X.Y.Z") release tag. Anything with a
// pre-release/build suffix (e.g. git-describe's "v0.3.3-2-gabc") is rejected.
func parseSemver(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	if v == "" || strings.ContainsAny(v, "-+") {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
