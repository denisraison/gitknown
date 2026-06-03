package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNewer covers the gate that decides when an update fires: strictly-higher
// release tags only, and never from/to a non-release version (a dev or
// git-describe build), so a source build is never clobbered by a release.
func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.3.3", "v0.3.4", true},
		{"v0.3.3", "v0.4.0", true},
		{"v0.3.3", "v1.0.0", true},
		{"0.3.3", "v0.3.4", true}, // mixed prefix
		{"v0.3.3", "v0.3.3", false},
		{"v0.3.4", "v0.3.3", false}, // downgrade
		{"v0.10.0", "v0.9.0", false},
		{"v0.9.0", "v0.10.0", true},
		{"dev", "v0.3.4", false},              // dev build: never auto-replaced
		{"v0.3.3-2-gabc123", "v0.3.4", false}, // ahead-of-tag build: not a clean release
		{"v0.3.3", "v0.3.4-rc1", false},       // pre-release latest: skip
	}
	for _, c := range cases {
		if got := newer(c.current, c.latest); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

// TestStageUpdate runs the real download -> verify -> extract -> swap path
// against a local server standing in for GitHub: a newer tag is staged and the
// `current` symlink resolves to a runnable binary, and a second pass is a no-op.
func TestStageUpdate(t *testing.T) {
	asset, ok := releaseAsset()
	if !ok {
		t.Skipf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const tag = "v9.9.9"
	// The "binary" is a script that prints the tag, so sanityCheck's `-version`
	// probe passes exactly as the real binary's would.
	targz := makeTarGz(t, "gitknown", "#!/bin/sh\necho "+tag+"\n")
	srv := releaseServer(t, tag, asset, targz, sha256hex(targz))
	defer srv.Close()

	dir := t.TempDir()
	cfg := updateConfig{dir: dir, current: "v0.0.1", apiBase: srv.URL, dlBase: srv.URL}

	staged, err := stageUpdate(context.Background(), cfg, asset)
	if err != nil {
		t.Fatalf("stageUpdate: %v", err)
	}
	if staged != tag {
		t.Fatalf("staged %q, want %q", staged, tag)
	}

	cur := filepath.Join(dir, "bin", "current")
	out, err := exec.CommandContext(context.Background(), cur, "-version").Output()
	if err != nil {
		t.Fatalf("running staged current: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != tag {
		t.Fatalf("staged binary reports %q, want %q", got, tag)
	}

	// Already staged: a second check must not re-stage.
	if staged2, err := stageUpdate(context.Background(), cfg, asset); err != nil || staged2 != "" {
		t.Fatalf("second stageUpdate = (%q, %v), want no-op", staged2, err)
	}
}

// TestStageUpdateRejectsBadChecksum: a tarball whose bytes don't match the
// published sha256 is refused, and nothing is staged.
func TestStageUpdateRejectsBadChecksum(t *testing.T) {
	asset, ok := releaseAsset()
	if !ok {
		t.Skipf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	const tag = "v9.9.9"
	targz := makeTarGz(t, "gitknown", "#!/bin/sh\necho "+tag+"\n")
	srv := releaseServer(t, tag, asset, targz, sha256hex([]byte("not the tarball")))
	defer srv.Close()

	dir := t.TempDir()
	cfg := updateConfig{dir: dir, current: "v0.0.1", apiBase: srv.URL, dlBase: srv.URL}

	_, err := stageUpdate(context.Background(), cfg, asset)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("stageUpdate err = %v, want checksum mismatch", err)
	}
	if _, err := exec.CommandContext(context.Background(), filepath.Join(dir, "bin", "current"), "-version").Output(); err == nil {
		t.Fatal("a `current` symlink was created despite the bad checksum")
	}
}

// releaseServer serves the three endpoints stageUpdate hits: the latest-release
// JSON, the tarball, and its .sha256 sidecar.
func releaseServer(t *testing.T, tag, asset string, targz []byte, sum string) *httptest.Server {
	t.Helper()
	tarName := "gitknown-" + tag + "-" + asset + ".tar.gz"
	dlPath := "/" + updateSlug + "/releases/download/" + tag + "/" + tarName
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+updateSlug+"/releases/latest", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"tag_name":"` + tag + `"}`)); err != nil {
			t.Errorf("write latest: %v", err)
		}
	})
	mux.HandleFunc(dlPath, func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(targz); err != nil {
			t.Errorf("write tarball: %v", err)
		}
	})
	mux.HandleFunc(dlPath+".sha256", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(sum + "  " + tarName + "\n")); err != nil {
			t.Errorf("write sha256: %v", err)
		}
	})
	return httptest.NewServer(mux)
}

func makeTarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
