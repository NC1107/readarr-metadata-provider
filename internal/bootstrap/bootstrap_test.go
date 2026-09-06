package bootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

// release describes a fake GitHub release for the tests to serve.
type release struct {
	payload         []byte // the uncompressed dataset
	corruptManifest bool   // publish a wrong digest
	parts           int    // >1 splits the artifact into that many parts
	plain           bool   // serve metadata.db uncompressed instead of .zst
	noManifest      bool
	noRange         bool // ignore Range headers, answering 200 from the start
}

func compress(t *testing.T, payload []byte) []byte {
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
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// releaseServer stands in for the GitHub release: the artifact (whole or in
// parts) plus the manifest published beside it, the way package-dataset.sh
// writes it.
func releaseServer(t *testing.T, r release) *httptest.Server {
	t.Helper()
	name := "metadata.db.zst"
	artifact := compress(t, r.payload)
	if r.plain {
		name = "metadata.db"
		artifact = r.payload
	}

	files := map[string][]byte{}
	var manifest strings.Builder
	manifest.WriteString("# readarr-metadata-provider dataset\n")
	fmt.Fprintf(&manifest, "# uncompressed size: %d bytes\n", len(r.payload))
	digest := func(b []byte) string {
		if r.corruptManifest {
			return hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32))
		}
		return sum(b)
	}
	if r.parts > 1 {
		size := (len(artifact) + r.parts - 1) / r.parts
		for i := 0; i < r.parts; i++ {
			start := i * size
			if start >= len(artifact) {
				break
			}
			end := min(start+size, len(artifact))
			pn := fmt.Sprintf("%s.part%02d", name, i)
			files[pn] = artifact[start:end]
			fmt.Fprintf(&manifest, "%s  %s\n", digest(files[pn]), pn)
		}
	} else {
		files[name] = artifact
		fmt.Fprintf(&manifest, "%s  %s\n", digest(artifact), name)
	}

	mux := http.NewServeMux()
	for fname, body := range files {
		mux.HandleFunc("/"+fname, func(w http.ResponseWriter, req *http.Request) {
			if r.noRange {
				w.Write(body)
				return
			}
			http.ServeContent(w, req, fname, modTime, bytes.NewReader(body))
		})
	}
	if !r.noManifest {
		m := manifest.String()
		mux.HandleFunc("/manifest.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(m)) })
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// modTime is zero, which ServeContent treats as "no Last-Modified".
var modTime time.Time

func TestEnsureDownloadsAndDecompresses(t *testing.T) {
	payload := bytes.Repeat([]byte("sqlite-ish bytes "), 1000)
	srv := releaseServer(t, release{payload: payload})
	dst := filepath.Join(t.TempDir(), "nested", "metadata.db")

	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("decompressed content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if InstalledDigest(dst) != sum(compress(t, payload)) {
		t.Error("the recorded digest is not the artifact's")
	}
}

func TestEnsureRejectsChecksumMismatch(t *testing.T) {
	srv := releaseServer(t, release{payload: []byte("payload"), corruptManifest: true})
	dst := filepath.Join(t.TempDir(), "metadata.db")

	err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{})
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

func TestEnsureRefusesWithoutManifest(t *testing.T) {
	srv := releaseServer(t, release{payload: []byte("payload"), noManifest: true})
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{}); err == nil {
		t.Fatal("a dataset with no published checksum must not be installed")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("an unverifiable download was installed")
	}
}

func TestEnsureKeepsExistingDataset(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(dst, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A URL that would fail if contacted proves the existing file wins.
	if err := Ensure(context.Background(), dst, "http://127.0.0.1:1/metadata.db.zst", Options{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "original" {
		t.Fatalf("existing dataset was overwritten: %q", got)
	}
}

func TestEnsureWithoutURLFails(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, "", Options{}); err == nil {
		t.Fatal("expected an error when no dataset and no URL")
	}
}

func TestEnsureUncompressedArtifact(t *testing.T) {
	payload := []byte("plain database bytes")
	srv := releaseServer(t, release{payload: payload, plain: true})
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db", Options{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Fatalf("got %q, want %q", got, payload)
	}
}

// TestEnsureReassemblesParts covers the day the compressed artifact outgrows
// one release asset: the packaging script then publishes numbered parts and
// the manifest lists them instead of the whole, and the download must join
// and verify them.
func TestEnsureReassemblesParts(t *testing.T) {
	payload := bytes.Repeat([]byte("many books, many parts "), 20000)
	srv := releaseServer(t, release{payload: payload, parts: 4})
	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled content differs: got %d bytes, want %d", len(got), len(payload))
	}
	if InstalledDigest(dst) == "" {
		t.Error("no digest recorded for a multipart install")
	}

	// A corrupt part is caught by its own checksum.
	bad := releaseServer(t, release{payload: payload, parts: 4, corruptManifest: true})
	dst2 := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst2, bad.URL+"/metadata.db.zst", Options{}); err == nil {
		t.Fatal("expected a checksum error for a corrupt part")
	}
}

// TestEnsureRefusesUnreadableDataset is the guarantee behind "a failed or
// corrupt download leaves the working dataset alone": a download that
// matches its checksum but fails validation is never installed.
func TestEnsureRefusesUnreadableDataset(t *testing.T) {
	srv := releaseServer(t, release{payload: []byte("not really a database")})
	dst := filepath.Join(t.TempDir(), "metadata.db")
	reject := Options{Validate: func(string) error { return errors.New("no works table") }}
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", reject); err == nil {
		t.Fatal("expected validation to refuse the download")
	}
	for _, leftover := range []string{dst, dst + ".download"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Errorf("%s should not exist after a refused download", leftover)
		}
	}
}

func TestUpdateSkipsWhenDigestMatches(t *testing.T) {
	payload := []byte("current dataset")
	srv := releaseServer(t, release{payload: payload})
	dst := filepath.Join(t.TempDir(), "metadata.db")

	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}

	updated, err := Update(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{})
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
	dst := filepath.Join(t.TempDir(), "metadata.db")

	old := releaseServer(t, release{payload: []byte("version one")})
	if err := Ensure(context.Background(), dst, old.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatal(err)
	}

	// A second release standing in for a later publish. Validation sees the
	// downloaded file, not the served one.
	var validated string
	opts := Options{Validate: func(p string) error {
		b, err := os.ReadFile(p)
		validated = string(b)
		return err
	}}
	next := releaseServer(t, release{payload: []byte("version two, with more books")})
	updated, err := Update(context.Background(), dst, next.URL+"/metadata.db.zst", opts)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated {
		t.Fatal("did not install the newer dataset")
	}
	if validated != "version two, with more books" {
		t.Errorf("validation ran on %q, want the new download", validated)
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
	dst := filepath.Join(t.TempDir(), "metadata.db")

	good := releaseServer(t, release{payload: []byte("known good")})
	if err := Ensure(context.Background(), dst, good.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatal(err)
	}

	corrupt := releaseServer(t, release{payload: []byte("corrupt"), corruptManifest: true})
	updated, err := Update(context.Background(), dst, corrupt.URL+"/metadata.db.zst", Options{})
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

// TestUpdateLeavesLocalAndUnmanagedDatasetsAlone is the fix for the worst
// first-run surprise: a dataset the operator built themselves, or placed by
// hand, must never be replaced by the published one.
func TestUpdateLeavesLocalAndUnmanagedDatasetsAlone(t *testing.T) {
	published := releaseServer(t, release{payload: []byte("published")})

	local := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(local, []byte("built here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RecordLocal(local); err != nil {
		t.Fatal(err)
	}
	updated, err := Update(context.Background(), local, published.URL+"/metadata.db.zst", Options{})
	if err != nil || updated {
		t.Fatalf("local build: updated=%v err=%v, want untouched", updated, err)
	}
	if got, _ := os.ReadFile(local); string(got) != "built here" {
		t.Fatalf("local build was replaced: %q", got)
	}

	unmanaged := filepath.Join(t.TempDir(), "metadata.db")
	if err := os.WriteFile(unmanaged, []byte("hand placed"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err = Update(context.Background(), unmanaged, published.URL+"/metadata.db.zst", Options{})
	if err != nil || updated {
		t.Fatalf("unmanaged: updated=%v err=%v, want untouched", updated, err)
	}
	if got, _ := os.ReadFile(unmanaged); string(got) != "hand placed" {
		t.Fatalf("hand-placed dataset was replaced: %q", got)
	}

	// With no file at all, Update installs one, like Ensure would.
	missing := filepath.Join(t.TempDir(), "metadata.db")
	updated, err = Update(context.Background(), missing, published.URL+"/metadata.db.zst", Options{})
	if err != nil || !updated {
		t.Fatalf("missing: updated=%v err=%v, want an install", updated, err)
	}
}

func TestRedactHidesCredentials(t *testing.T) {
	got := redact("https://user:secret@mirror.example/ds/metadata.db.zst?token=abc")
	if strings.Contains(got, "secret") || strings.Contains(got, "abc") {
		t.Errorf("redact leaked credentials: %s", got)
	}
	if got != "https://mirror.example/ds/metadata.db.zst" {
		t.Errorf("redact = %q", got)
	}
}

// TestDownloadResumesAfterDrop covers a connection that dies mid-body: the
// next attempt asks for the rest with a Range header and the hash carries
// on, so the transfer completes without re-fetching what already arrived
// and still verifies end to end.
func TestDownloadResumesAfterDrop(t *testing.T) {
	payload := bytes.Repeat([]byte("resume me "), 50000)
	artifact := compress(t, payload)
	manifest := fmt.Sprintf("# uncompressed size: %d bytes\n%s  metadata.db.zst\n", len(payload), sum(artifact))

	var requests []string
	dropped := false
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(manifest)) })
	mux.HandleFunc("/metadata.db.zst", func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("Range"))
		if !dropped {
			// Send the first half, then hang up without finishing.
			dropped = true
			w.Header().Set("Content-Length", fmt.Sprint(len(artifact)))
			w.WriteHeader(http.StatusOK)
			w.Write(artifact[:len(artifact)/2])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, err := hj.Hijack()
				if err == nil {
					conn.Close()
				}
			}
			return
		}
		http.ServeContent(w, r, "metadata.db.zst", modTime, bytes.NewReader(artifact))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "metadata.db")
	if err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{}); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, payload) {
		t.Fatalf("resumed content differs: got %d bytes, want %d", len(got), len(payload))
	}
	if len(requests) != 2 || requests[0] != "" || !strings.HasPrefix(requests[1], "bytes=") {
		t.Errorf("expected one full request then one Range request, got %q", requests)
	}
	if requests[1] != fmt.Sprintf("bytes=%d-", len(artifact)/2) {
		t.Errorf("resume asked for %q, want from byte %d", requests[1], len(artifact)/2)
	}
}

// TestInstallRefusesWhenDiskIsFull checks the size line in the manifest is
// honoured before any bytes move, with a message that says how much is
// needed.
func TestInstallRefusesWhenDiskIsFull(t *testing.T) {
	if _, ok := freeBytes(t.TempDir()); !ok {
		t.Skip("free space is not measurable on this platform")
	}
	artifact := compress(t, []byte("tiny"))
	manifest := fmt.Sprintf("# uncompressed size: %d bytes\n%s  metadata.db.zst\n", int64(1)<<60, sum(artifact))
	mux := http.NewServeMux()
	mux.HandleFunc("/manifest.txt", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(manifest)) })
	mux.HandleFunc("/metadata.db.zst", func(w http.ResponseWriter, _ *http.Request) { w.Write(artifact) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "metadata.db")
	err := Ensure(context.Background(), dst, srv.URL+"/metadata.db.zst", Options{})
	if err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("expected a free-space refusal, got %v", err)
	}
	if _, statErr := os.Stat(dst + ".download"); !os.IsNotExist(statErr) {
		t.Error("a refused download should not have started")
	}
}
