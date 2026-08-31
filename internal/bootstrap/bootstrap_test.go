package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// releaseServer stands in for the GitHub release: a zstd artifact plus the
// manifest published beside it.
func releaseServer(t *testing.T, payload []byte, corruptManifest bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	artifact := buf.Bytes()

	sum := sha256.Sum256(artifact)
	digest := hex.EncodeToString(sum[:])
	if corruptManifest {
		digest = hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
	}
	manifest := "# comment line\n" + digest + "  metadata.db.zst\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/metadata.db.zst", func(w http.ResponseWriter, r *http.Request) {
		w.Write(artifact)
	})
	mux.HandleFunc("/manifest.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(manifest))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestEnsureDownloadsAndDecompresses(t *testing.T) {
	payload := bytes.Repeat([]byte("sqlite-ish bytes "), 1000)
	srv := releaseServer(t, payload, false)
	dst := filepath.Join(t.TempDir(), "nested", "metadata.db")

	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decompressed content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}

func TestEnsureRejectsChecksumMismatch(t *testing.T) {
	srv := releaseServer(t, []byte("payload"), true)
	dir := t.TempDir()
	dst := filepath.Join(dir, "metadata.db")

	err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst")
	if err == nil {
		t.Fatal("expected a checksum error, got nil")
	}
	// A rejected download must leave nothing behind, or the next boot would
	// serve a corrupt dataset.
	for _, leftover := range []string{dst, dst + ".download"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should not exist after a failed download", leftover)
		}
	}
}

func TestEnsureKeepsExistingDataset(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(dst, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A URL that would fail if contacted proves the existing file wins.
	if err := Ensure(context.Background(), dst, "http://127.0.0.1:1/metadata.db.zst"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "original" {
		t.Fatalf("existing dataset was overwritten: %q", got)
	}
}

func TestEnsureWithoutURLFails(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, ""); err == nil {
		t.Fatal("expected an error when no dataset and no URL")
	}
}

func TestEnsureUncompressedArtifact(t *testing.T) {
	payload := []byte("plain database bytes")
	mux := http.NewServeMux()
	mux.HandleFunc("/metadata.db", func(w http.ResponseWriter, r *http.Request) {
		w.Write(payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db"); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

func TestUpdateSkipsWhenDigestMatches(t *testing.T) {
	payload := []byte("current dataset")
	srv := releaseServer(t, payload, false)
	dst := filepath.Join(t.TempDir(), "metadata.db")

	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := Update(context.Background(), dst, srv.URL+"/metadata.db.zst")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated {
		t.Fatal("re-downloaded a dataset that had not changed")
	}
	after, _ := os.Stat(dst)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged dataset was rewritten")
	}
}

func TestUpdateInstallsNewerDataset(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "metadata.db")

	old := releaseServer(t, []byte("version one"), false)
	if err := Ensure(context.Background(), dst, old.URL+"/metadata.db.zst"); err != nil {
		t.Fatal(err)
	}

	// A second release standing in for a later publish.
	next := releaseServer(t, []byte("version two, with more books"), false)
	updated, err := Update(context.Background(), dst, next.URL+"/metadata.db.zst")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated {
		t.Fatal("did not install the newer dataset")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "version two, with more books" {
		t.Fatalf("served content is %q", got)
	}
	if InstalledDigest(dst) == "" {
		t.Error("digest was not recorded, so the next check would download again")
	}
}

func TestUpdateKeepsGoodDatasetOnBadChecksum(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "metadata.db")

	good := releaseServer(t, []byte("known good"), false)
	if err := Ensure(context.Background(), dst, good.URL+"/metadata.db.zst"); err != nil {
		t.Fatal(err)
	}

	corrupt := releaseServer(t, []byte("corrupt"), true)
	updated, err := Update(context.Background(), dst, corrupt.URL+"/metadata.db.zst")
	if err == nil {
		t.Fatal("expected a checksum error")
	}
	if updated {
		t.Fatal("reported an update it did not make")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "known good" {
		t.Fatalf("a failed update replaced the working dataset: %q", got)
	}
	if _, err := os.Stat(dst + ".download"); !os.IsNotExist(err) {
		t.Error("partial download was left behind")
	}
}
