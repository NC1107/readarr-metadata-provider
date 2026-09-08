// Package bootstrap fetches a published dataset so a fresh install can
// serve without building one first, and keeps it current afterwards.
//
// Two rules hold throughout. A download never replaces the served file until
// it has been verified against the published checksum and proven to open,
// so a bad download degrades to serving what already works. And a file this
// package did not install is never replaced: an operator who built their own
// dataset keeps it.
package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// maxDatasetBytes caps what a download may expand to. The published
// dataset is ~6.6GB; this leaves headroom without letting a bad URL fill
// the disk.
const maxDatasetBytes = 32 << 30

// stallTimeout is how long a transfer may go without delivering a byte
// before it is cut and resumed. Without it a half-open connection holds the
// download until the caller gives up, which for first boot is never.
const stallTimeout = 2 * time.Minute

// downloadAttempts is how many times one part is (re)opened before the
// download is given up. A resume continues from the byte where the previous
// attempt stopped, so a retry costs nothing already transferred.
const downloadAttempts = 4

// client bounds the connection rather than the whole transfer: the artifact
// is over a gigabyte and a slow link must not fail on a total timeout.
var client = &http.Client{
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	},
}

// LocalMarker is what the dataset builder writes into the digest sidecar of
// a dataset it built, so the updater knows to leave it alone.
const LocalMarker = "local"

// Options tunes an install.
type Options struct {
	// Validate is run on the downloaded file before it replaces the served
	// one. It should open the file and prove it can serve; an error keeps the
	// current dataset. Nil skips the check.
	Validate func(path string) error
}

// digestPath is where the checksum of the installed artifact is recorded,
// so a later check can tell whether the published dataset has moved on
// without downloading gigabytes to find out.
func digestPath(dbPath string) string { return dbPath + ".installed-sha256" }

var (
	errLocal     = errors.New("dataset was built locally")
	errUnmanaged = errors.New("dataset was not installed by this server")
)

// InstalledDigest reports the checksum recorded for the dataset on disk, or
// an empty string when none is recorded.
func InstalledDigest(dbPath string) string {
	d, err := installedDigest(dbPath)
	if err != nil {
		return ""
	}
	return d
}

// installedDigest distinguishes the three states a dataset on disk can be
// in: installed by this package (a digest), built by the operator (the
// local marker), or of unknown origin (no sidecar at all).
func installedDigest(dbPath string) (string, error) {
	b, err := os.ReadFile(digestPath(dbPath))
	if err != nil {
		return "", errUnmanaged
	}
	d := strings.TrimSpace(string(b))
	if d == LocalMarker {
		return "", errLocal
	}
	if d == "" {
		return "", errUnmanaged
	}
	return d, nil
}

// RecordLocal marks the dataset at dbPath as built here, so the updater
// never replaces it. The dataset builder calls this on every build.
func RecordLocal(dbPath string) error {
	return os.WriteFile(digestPath(dbPath), []byte(LocalMarker+"\n"), 0o644)
}

func recordDigest(dbPath, digest string) {
	if err := os.WriteFile(digestPath(dbPath), []byte(digest+"\n"), 0o644); err != nil {
		log.Printf("could not record dataset checksum: %v", err)
	}
}

// warned remembers which skip reasons have been logged, so a dataset that is
// deliberately pinned does not produce the same line every six hours.
var warned sync.Map

func warnOnce(key, format string, args ...any) {
	if _, seen := warned.LoadOrStore(key, true); !seen {
		log.Printf(format, args...)
	}
}

// Update installs a newer published dataset when one exists, returning
// whether it replaced the file. The comparison is a single small request for
// the manifest, so calling this on a timer is cheap. A dataset this package
// did not install is left alone.
func Update(ctx context.Context, dbPath, rawURL string, opts Options) (bool, error) {
	if rawURL == "" {
		return false, nil
	}
	installed, err := installedDigest(dbPath)
	switch {
	case errors.Is(err, errLocal):
		warnOnce("local:"+dbPath, "dataset at %s was built locally; automatic updates leave it alone (delete it and %s to switch to the published dataset)",
			dbPath, digestPath(dbPath))
		return false, nil
	case errors.Is(err, errUnmanaged):
		if _, statErr := os.Stat(dbPath); statErr == nil {
			warnOnce("unmanaged:"+dbPath, "dataset at %s was not installed by this server, so automatic updates leave it alone (delete it to download the published dataset, or set the refresh interval to 0 to silence this)",
				dbPath)
			return false, nil
		}
		// No file at all: fall through and install one.
	}

	art, err := resolve(ctx, rawURL)
	if err != nil {
		return false, err
	}
	if art.published == installed {
		return false, nil
	}

	log.Printf("newer dataset published, downloading from %s", redact(rawURL))
	if err := install(ctx, dbPath, art, opts); err != nil {
		return false, err
	}
	return true, nil
}

// Ensure guarantees a dataset exists at dbPath, downloading it from url
// when it does not. An existing file is never touched, so a boot after the
// first costs nothing.
func Ensure(ctx context.Context, dbPath, rawURL string, opts Options) error {
	if _, err := os.Stat(dbPath); err == nil {
		return nil
	}
	if rawURL == "" {
		return fmt.Errorf("no dataset at %s and no dataset URL configured", dbPath)
	}
	log.Printf("no dataset at %s, downloading from %s", dbPath, redact(rawURL))
	art, err := resolve(ctx, rawURL)
	if err != nil {
		return err
	}
	return install(ctx, dbPath, art, opts)
}

// install downloads an artifact beside dbPath, verifies it, proves it opens,
// and only then renames it into place.
func install(ctx context.Context, dbPath string, art *artifact, opts Options) error {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return err
	}
	if err := checkFreeSpace(filepath.Dir(dbPath), art.uncompressed); err != nil {
		return err
	}
	start := time.Now()
	tmp := dbPath + ".download"
	os.Remove(tmp)
	written, err := download(ctx, art, tmp)
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if opts.Validate != nil {
		if err := opts.Validate(tmp); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("downloaded dataset failed validation, keeping the current one: %w", err)
		}
	}
	// The served database keeps the connections it already holds open on
	// the old inode (see the server's store), so renaming over it is safe;
	// the server opens the new file afterwards and swaps to it.
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return err
	}
	recordDigest(dbPath, art.published)
	log.Printf("dataset ready: %s (%s, %s)", dbPath, humanBytes(written), time.Since(start).Round(time.Second))
	return nil
}

// artifact is what a dataset URL resolves to once the manifest beside it
// has been read: one file, or an ordered run of parts, each with a digest.
type artifact struct {
	parts []part
	// published identifies this exact publication for the sidecar: the
	// file's digest, or for parts a digest over the parts' digests.
	published    string
	uncompressed int64
	zst          bool
}

type part struct {
	url    string
	digest string
}

// resolve reads the manifest published beside url and works out what to
// download. The single file named by url is preferred; when the manifest
// lists it only as parts, those are used in order. A URL without a manifest
// is refused: the manifest is the only thing that lets a download be
// verified, and an unverifiable dataset is not something to serve.
func resolve(ctx context.Context, rawURL string) (*artifact, error) {
	base := rawURL[:strings.LastIndex(rawURL, "/")+1]
	name := path.Base(rawURL)
	m, err := fetchManifest(ctx, base+"manifest.txt")
	if err != nil {
		return nil, fmt.Errorf("no published checksum for %s: %w", redact(rawURL), err)
	}
	art := &artifact{uncompressed: m.uncompressed, zst: strings.HasSuffix(name, ".zst")}
	if d, ok := m.digests[name]; ok {
		art.parts = []part{{url: rawURL, digest: d}}
		art.published = d
		return art, nil
	}
	var names []string
	for n := range m.digests {
		if strings.HasPrefix(n, name+".part") {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("manifest beside %s does not list it or any of its parts", redact(rawURL))
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		art.parts = append(art.parts, part{url: base + n, digest: m.digests[n]})
		io.WriteString(h, m.digests[n]+"\n")
	}
	art.published = hex.EncodeToString(h.Sum(nil))
	return art, nil
}

type manifest struct {
	digests      map[string]string
	uncompressed int64
}

// fetchManifest reads the sha256sum-style manifest the packaging script
// writes: comment lines, one of which records the uncompressed size, then
// "digest  name" lines for every asset.
func fetchManifest(ctx context.Context, manifestURL string) (*manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest.txt: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	m := &manifest{digests: map[string]string{}}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			if rest, ok := strings.CutPrefix(line, "# uncompressed size:"); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest), "bytes")), 10, 64)
				m.uncompressed = n
			}
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && len(fields[0]) == 64 {
			m.digests[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
		}
	}
	if len(m.digests) == 0 {
		return nil, errors.New("manifest.txt lists no checksums")
	}
	return m, nil
}

// download streams the artifact's parts, in order, through one hash per
// part and (for .zst) one decoder, into dest. Each part is verified as it
// completes, so a corrupt part is caught before the next is fetched.
func download(ctx context.Context, art *artifact, dest string) (int64, error) {
	f, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	body := &partsReader{ctx: ctx, parts: art.parts, lastLogged: time.Now()}
	defer body.Close()

	var src io.Reader = body
	if art.zst {
		dec, err := zstd.NewReader(body)
		if err != nil {
			return 0, err
		}
		defer dec.Close()
		src = dec
	}

	written, err := io.Copy(f, io.LimitReader(src, maxDatasetBytes+1))
	if err != nil {
		return 0, fmt.Errorf("downloading dataset: %w", err)
	}
	if written > maxDatasetBytes {
		return 0, fmt.Errorf("dataset exceeds %s limit", humanBytes(maxDatasetBytes))
	}
	if err := body.finished(); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	return written, nil
}

// partsReader presents an ordered run of HTTP bodies as one stream. A part
// that stalls or drops is reopened with a Range request from the byte it
// reached, so a retry never re-transfers what already arrived, and every
// part's digest is checked the moment its last byte is read.
type partsReader struct {
	ctx   context.Context
	parts []part

	i        int           // index of the part being read
	body     io.ReadCloser // current part's body, nil between parts
	cancel   context.CancelFunc
	stall    *time.Timer
	stalled  atomic.Bool
	hash     hash.Hash // sha256 of the current part
	received int64     // bytes of the current part received so far
	attempts int
	done     bool

	total, read int64 // across all parts, for progress lines
	lastLogged  time.Time
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.done {
			return 0, io.EOF
		}
		if p.body == nil {
			if err := p.open(); err != nil {
				return 0, err
			}
		}
		n, err := p.body.Read(b)
		if n > 0 {
			p.hash.Write(b[:n])
			p.received += int64(n)
			p.read += int64(n)
			p.stall.Reset(stallTimeout)
			p.progress()
		}
		if err == nil {
			return n, nil
		}
		if errors.Is(err, io.EOF) {
			if verr := p.finishPart(); verr != nil {
				return n, verr
			}
			if n > 0 {
				return n, nil
			}
			continue
		}
		// A read error mid-part: give up if the caller is gone, otherwise
		// reopen from where this attempt reached.
		p.closeBody()
		if p.ctx.Err() != nil {
			return n, p.ctx.Err()
		}
		p.attempts++
		if p.attempts >= downloadAttempts {
			if p.stalled.Load() {
				return n, fmt.Errorf("part %s: no data for %s, transfer stalled: %w", path.Base(p.parts[p.i].url), stallTimeout, err)
			}
			return n, fmt.Errorf("part %s: %w", path.Base(p.parts[p.i].url), err)
		}
		log.Printf("dataset download interrupted at %s of %s, resuming (attempt %d): %v",
			humanBytes(p.received), path.Base(p.parts[p.i].url), p.attempts+1, err)
		select {
		case <-p.ctx.Done():
			return n, p.ctx.Err()
		case <-time.After(time.Duration(p.attempts) * 3 * time.Second):
		}
		if n > 0 {
			return n, nil
		}
	}
}

// open starts (or resumes) the current part.
func (p *partsReader) open() error {
	cur := p.parts[p.i]
	ctx, cancel := context.WithCancel(p.ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur.url, nil)
	if err != nil {
		cancel()
		return err
	}
	if p.received > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", p.received))
	}
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return err
	}
	switch {
	case p.received == 0 && resp.StatusCode == http.StatusOK:
		p.hash = sha256.New()
		if p.i == 0 && resp.ContentLength > 0 {
			// Only the first part's length is known up front; progress is
			// reported against it and grows as later parts start.
			p.total = resp.ContentLength
		} else if resp.ContentLength > 0 {
			p.total += resp.ContentLength
		}
	case p.received > 0 && resp.StatusCode == http.StatusPartialContent:
		// Resumed where the previous attempt stopped; the hash carries on.
	case p.received > 0 && resp.StatusCode == http.StatusOK:
		resp.Body.Close()
		cancel()
		return fmt.Errorf("%s does not support resuming a download", redact(cur.url))
	default:
		resp.Body.Close()
		cancel()
		return fmt.Errorf("downloading %s: HTTP %s", redact(cur.url), resp.Status)
	}
	p.body, p.cancel = resp.Body, cancel
	p.stalled.Store(false)
	p.stall = time.AfterFunc(stallTimeout, func() {
		p.stalled.Store(true)
		cancel()
	})
	return nil
}

func (p *partsReader) closeBody() {
	if p.stall != nil {
		p.stall.Stop()
	}
	if p.body != nil {
		p.body.Close()
		p.body = nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

// finishPart verifies the part that just ended and moves to the next.
func (p *partsReader) finishPart() error {
	p.closeBody()
	cur := p.parts[p.i]
	if got := hex.EncodeToString(p.hash.Sum(nil)); cur.digest != "" && got != cur.digest {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", path.Base(cur.url), cur.digest, got)
	}
	p.i++
	p.received, p.attempts = 0, 0
	if p.i >= len(p.parts) {
		p.done = true
	}
	return nil
}

// finished reports whether every part was read to its end and verified. A
// zstd stream can end before its container does, so this is checked after
// the copy rather than inferred from it.
func (p *partsReader) finished() error {
	if !p.done {
		return fmt.Errorf("download ended early: %d of %d parts complete", p.i, len(p.parts))
	}
	return nil
}

func (p *partsReader) Close() error {
	p.closeBody()
	return nil
}

func (p *partsReader) progress() {
	if time.Since(p.lastLogged) < 15*time.Second {
		return
	}
	p.lastLogged = time.Now()
	if p.total > 0 && len(p.parts) == 1 {
		log.Printf("  %s of %s (%.0f%%)", humanBytes(p.read), humanBytes(p.total), 100*float64(p.read)/float64(p.total))
	} else {
		log.Printf("  %s (part %d of %d)", humanBytes(p.read), p.i+1, len(p.parts))
	}
}

// checkFreeSpace refuses a download that cannot fit, with the reason, rather
// than filling the disk and failing at the end of a long transfer. The
// margin covers SQLite's own scratch needs when the file is first opened.
func checkFreeSpace(dir string, need int64) error {
	if need <= 0 {
		return nil
	}
	avail, ok := freeBytes(dir)
	if !ok {
		return nil
	}
	want := need + need/20
	if avail < want {
		return fmt.Errorf("not enough free space in %s: the dataset needs %s and %s is available (a refresh keeps the current dataset while the new one downloads, so it needs room for both)",
			dir, humanBytes(want), humanBytes(avail))
	}
	return nil
}

// redact strips credentials and query strings from a URL before it is
// logged, so a private mirror's token never lands in the container log.
func redact(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
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
