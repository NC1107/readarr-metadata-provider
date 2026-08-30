// Package bootstrap fetches a published dataset so a fresh install can
// serve without building one first.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// maxDatasetBytes caps what a download may expand to. The published
// dataset is ~6.6GB; this leaves headroom without letting a bad URL fill
// the disk.
const maxDatasetBytes = 32 << 30

// client bounds the transfer so a stalled server cannot hang first boot
// forever. The timeout covers the whole body, which is why it is generous.
var client = &http.Client{Timeout: 2 * time.Hour}

// Ensure guarantees a dataset exists at dbPath, downloading it from url
// when it does not. An existing file is never touched, so a boot after the
// first costs nothing.
func Ensure(ctx context.Context, dbPath, url string) error {
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	}
	if url == "" {
		return fmt.Errorf("no dataset at %s and no dataset URL configured", dbPath)
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}

	log.Printf("no dataset at %s, downloading from %s", dbPath, url)
	start := time.Now()

	// The checksum covers the compressed artifact, so hash the download as
	// it streams and verify once it completes. A mismatch discards the
	// output rather than leaving a corrupt dataset in place.
	want := fetchChecksum(ctx, url)

	tmp := dbPath + ".download"
	os.Remove(tmp)
	written, sum, err := download(ctx, url, tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if want != "" && sum != want {
		os.Remove(tmp)
		return fmt.Errorf("checksum mismatch: expected %s, got %s", want, sum)
	}
	if want == "" {
		log.Print("no published checksum found; skipping verification")
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return err
	}
	log.Printf("dataset ready: %s (%s, %s)", dbPath, humanBytes(written), time.Since(start).Round(time.Second))
	return nil
}

// download streams url into dest, decompressing zstd on the way, and
// returns the bytes written plus the checksum of the transferred (still
// compressed) body.
func download(ctx context.Context, url, dest string) (int64, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("downloading dataset: HTTP %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()

	hash := sha256.New()
	body := &progressReader{
		r:          io.TeeReader(resp.Body, hash),
		total:      resp.ContentLength,
		lastLogged: time.Now(),
	}

	var src io.Reader = body
	if strings.HasSuffix(url, ".zst") {
		dec, err := zstd.NewReader(body)
		if err != nil {
			return 0, "", err
		}
		defer dec.Close()
		src = dec
	}

	written, err := io.Copy(f, io.LimitReader(src, maxDatasetBytes+1))
	if err != nil {
		return 0, "", fmt.Errorf("downloading dataset: %w", err)
	}
	if written > maxDatasetBytes {
		return 0, "", fmt.Errorf("dataset exceeds %s limit", humanBytes(maxDatasetBytes))
	}
	if err := f.Sync(); err != nil {
		return 0, "", err
	}
	return written, hex.EncodeToString(hash.Sum(nil)), nil
}

// fetchChecksum looks for the artifact's line in the manifest published
// beside it. Verification is best effort: a missing manifest is reported by
// the caller, not fatal, since the transport is already authenticated.
func fetchChecksum(ctx context.Context, url string) string {
	manifestURL := url[:strings.LastIndex(url, "/")+1] + "manifest.txt"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ""
	}
	name := path.Base(url)
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0]
		}
	}
	return ""
}

// progressReader logs download progress, which matters when the artifact is
// a gigabyte and the alternative is a silent process.
type progressReader struct {
	r          io.Reader
	total      int64
	read       int64
	lastLogged time.Time
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if time.Since(p.lastLogged) > 15*time.Second {
		p.lastLogged = time.Now()
		if p.total > 0 {
			log.Printf("  %s of %s (%.0f%%)", humanBytes(p.read), humanBytes(p.total),
				100*float64(p.read)/float64(p.total))
		} else {
			log.Printf("  %s", humanBytes(p.read))
		}
	}
	return n, err
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
