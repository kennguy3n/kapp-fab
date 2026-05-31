// Command kapp-publish is the publisher-side CLI for the kapp
// marketplace (Phase B8). It packs a manifest directory into a
// tar.gz bundle, optionally signs it with an ed25519 private key,
// uploads the bytes to a marketplace deploy, and submits a new
// version row referencing the upload.
//
// Subcommands:
//
//	pack      <dir> -o bundle.tar.gz
//	  Pack a manifest directory into a deterministic tar.gz so
//	  the same source tree produces the same content_hash on
//	  every box. Used standalone when a publisher wants to host
//	  the bundle themselves (or sign offline).
//
//	validate  <bundle.tar.gz>
//	  Parse + validate the bundle without uploading. Same code
//	  path the upload endpoint runs server-side, so a clean
//	  exit here means the upload will pass validation.
//
//	sign      <bundle.tar.gz> --key <file.ed25519> [--key-id <id>] -o <bundle.sig>
//	  Detached ed25519 signature over the bundle bytes. The
//	  publisher passes --bundle-signature + --bundle-signature-
//	  key-id when calling publish so the marketplace's
//	  SignatureCheck recognises the signature.
//
//	upload    <bundle.tar.gz>
//	          --api <base-url> --token <bearer> --publisher-id <uuid>
//	  Upload the tar.gz to POST /api/v1/publisher/{publisher_id}/bundles.
//	  Prints the JSON response (bundle_url + bundle_hash + bundle_size).
//
//	publish   <bundle.tar.gz>
//	          --api <base-url> --token <bearer>
//	          --publisher-id <uuid> --extension-id <uuid>
//	          [--bundle-signature <b64> --bundle-signature-key-id <id>]
//	  One-shot upload + version submit. Equivalent to
//	  `upload` followed by POST /api/v1/publisher/{publisher_id}/
//	  extensions/{extension_id}/versions with the returned
//	  bundle_url / bundle_hash / bundle_size.
//
// All HTTP requests carry the supplied --token as a Bearer
// Authorization header. Authentication failures surface as
// non-zero exit codes with the server's JSON body printed to
// stderr.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/bundle"
)

const (
	// defaultManifestFile is the canonical name the packer
	// expects in the source tree. Mirrors what bundle.Extract
	// looks for inside the tar.gz (the resolver expects this
	// filename at the archive root per spec §2).
	defaultManifestFile = "kapp-extension.yaml"

	// defaultArchiveRoot is the single-directory prefix every
	// entry is packed under. Spec §2 of the bundle layout
	// requires "archive root MUST contain a single directory"
	// — using a fixed name here keeps the produced tar.gz
	// deterministic regardless of the on-disk source path
	// (publisher A on /home/alice/myext and publisher B on
	// C:\Users\Bob\myext both produce the same hash).
	defaultArchiveRoot = "bundle"

	// httpUploadTimeout caps the time spent on a single
	// upload + publish request. The server enforces its own
	// body cap (10 MiB) and parsing budget, so a hostile network
	// shouldn't be able to stall us indefinitely.
	httpUploadTimeout = 120 * time.Second
)

func main() {
	if err := realMain(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kapp-publish:", err)
		os.Exit(1)
	}
}

func realMain(args []string) error {
	if len(args) == 0 {
		printUsage()
		return errors.New("subcommand required")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "pack":
		return runPack(rest)
	case "validate":
		return runValidate(rest)
	case "sign":
		return runSign(rest)
	case "upload":
		return runUpload(rest)
	case "publish":
		return runPublish(rest)
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		printUsage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `kapp-publish — publisher CLI for the kapp marketplace

Subcommands:
  pack     <dir>            -o bundle.tar.gz
  validate <bundle.tar.gz>
  sign     <bundle.tar.gz>  --key <file.ed25519> [--key-id <id>] -o <sig.b64>
  upload   <bundle.tar.gz>  --api <url> --token <jwt> --publisher-id <uuid>
  publish  <bundle.tar.gz>  --api <url> --token <jwt> --publisher-id <uuid>
                            --extension-id <uuid>
                            [--bundle-signature <b64> --bundle-signature-key-id <id>]
`)
}

// ---------------------------------------------------------------------------
// pack
// ---------------------------------------------------------------------------

func runPack(args []string) error {
	flags := flag.NewFlagSet("pack", flag.ContinueOnError)
	out := flags.String("o", "", "output tar.gz path (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *out == "" || flags.NArg() != 1 {
		return errors.New("usage: pack <dir> -o bundle.tar.gz")
	}
	dir := flags.Arg(0)
	if !looksLikeBundleSource(dir) {
		return fmt.Errorf("source dir %q does not contain %s", dir, defaultManifestFile)
	}
	body, err := packDir(dir)
	if err != nil {
		return fmt.Errorf("pack: %w", err)
	}
	// 0o600 — the packed tar.gz contains the publisher's signed
	// bundle bytes; tighten the default mask to prevent other
	// shell users on a shared dev box from reading it. The
	// publisher can chmod it back if they want world-readable.
	if err := os.WriteFile(*out, body, 0o600); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	sum := sha256.Sum256(body)
	fmt.Printf("packed %d bytes\nsha256: %s\nout: %s\n", len(body), hex.EncodeToString(sum[:]), *out)
	return nil
}

func looksLikeBundleSource(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, defaultManifestFile))
	return err == nil && !info.IsDir()
}

// packDir walks `dir` and writes a deterministic tar.gz to memory.
// Determinism: entries are sorted lexicographically, mtimes are
// zeroed, owners are zeroed, and the gzip header skips the OS /
// timestamp fields. The same source tree on any box produces the
// same bytes (and therefore the same SHA-256), which is what the
// content-addressed marketplace bundle store needs.
func packDir(dir string) ([]byte, error) {
	var entries []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		// Skip macOS / editor / VCS noise that should never
		// ship inside a bundle.
		name := d.Name()
		switch name {
		case ".DS_Store", "Thumbs.db", ".git", ".gitignore":
			return nil
		}
		if strings.HasSuffix(name, "~") || strings.HasSuffix(name, ".swp") {
			return nil
		}
		entries = append(entries, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(entries)

	var buf bytes.Buffer
	// gzip.NewWriter sets ModTime/Name/OS internally; clearing
	// after Write keeps headers deterministic across boxes.
	gz := gzip.NewWriter(&buf)
	gz.Name = ""
	gz.Comment = ""
	gz.ModTime = time.Time{}
	tw := tar.NewWriter(gz)

	for _, p := range entries {
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return nil, err
		}
		// tar headers must use forward slashes regardless of
		// the host OS, otherwise the server-side extractor
		// rejects the entry. Prefix the fixed archive root so
		// every entry sits under a single directory (spec §2).
		rel = defaultArchiveRoot + "/" + filepath.ToSlash(rel)
		// gosec G304 — p is rooted under the publisher-supplied
		// source dir; the WalkDir guarantees it stays inside.
		// The publisher is packing their own files by design.
		body, err := os.ReadFile(p) // #nosec G304
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		hdr := &tar.Header{
			Name:     rel,
			Mode:     0o644,
			Size:     int64(len(body)),
			ModTime:  time.Time{},
			Typeflag: tar.TypeReg,
			Format:   tar.FormatPAX,
		}
		// Zero ownership for byte-stability across boxes.
		hdr.Uid = 0
		hdr.Gid = 0
		hdr.Uname = ""
		hdr.Gname = ""
		// PAX records would otherwise embed atime/ctime; clear
		// so the header is byte-stable.
		hdr.PAXRecords = nil
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(body); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------------------------------------------------------------------------
// validate
// ---------------------------------------------------------------------------

func runValidate(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: validate <bundle.tar.gz>")
	}
	body, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if int64(len(body)) > marketplace.MaxBundleSizeBytes {
		return fmt.Errorf("bundle %d bytes exceeds %d cap",
			len(body), marketplace.MaxBundleSizeBytes)
	}
	rb, err := bundle.Extract(body)
	if err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	sum := sha256.Sum256(body)
	fmt.Printf("manifest:         %s@%s\n", rb.Manifest.Name, rb.Manifest.Version)
	fmt.Printf("publisher:        %s\n", rb.Manifest.Publisher)
	fmt.Printf("description:      %s\n", trim(rb.Manifest.Description, 80))
	fmt.Printf("kapp constraint:  %s..%s\n", rb.Manifest.MinKappVersion, rb.Manifest.MaxKappVersion)
	fmt.Printf("ktypes:           %d\n", len(rb.Manifest.KTypes))
	fmt.Printf("workflows:        %d\n", len(rb.Manifest.Workflows))
	fmt.Printf("agent_tools:      %d\n", len(rb.Manifest.AgentTools))
	fmt.Printf("webhooks:         %d\n", len(rb.Manifest.WebhooksConsumed))
	fmt.Printf("ui_extensions:    %d\n", len(rb.Manifest.UIExtensions))
	fmt.Printf("size:             %d bytes\n", len(body))
	fmt.Printf("sha256:           %s\n", hex.EncodeToString(sum[:]))
	return nil
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---------------------------------------------------------------------------
// sign
// ---------------------------------------------------------------------------

func runSign(args []string) error {
	flags := flag.NewFlagSet("sign", flag.ContinueOnError)
	keyPath := flags.String("key", "", "ed25519 private key file (32 or 64 raw bytes, OR base64)")
	keyID := flags.String("key-id", "", "optional key id to print alongside the signature")
	out := flags.String("o", "", "output path for the base64 detached signature (stdout if empty)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *keyPath == "" || flags.NArg() != 1 {
		return errors.New("usage: sign <bundle.tar.gz> --key <file> [--key-id <id>] [-o <sig.b64>]")
	}
	// gosec G304 — the bundle path is a CLI argument by
	// design; the publisher is signing a file they themselves
	// pointed at. There is no untrusted-input vector here.
	body, err := os.ReadFile(flags.Arg(0)) // #nosec G304
	if err != nil {
		return err
	}
	priv, err := loadEd25519Key(*keyPath)
	if err != nil {
		return fmt.Errorf("load key: %w", err)
	}
	sig := ed25519.Sign(priv, body)
	encoded := base64.StdEncoding.EncodeToString(sig)
	if *out == "" {
		fmt.Println(encoded)
	} else {
		if err := os.WriteFile(*out, []byte(encoded+"\n"), 0o600); err != nil {
			return err
		}
	}
	sum := sha256.Sum256(body)
	fmt.Fprintf(os.Stderr, "signed %d bytes (sha256 %s)\n", len(body), hex.EncodeToString(sum[:]))
	if *keyID != "" {
		fmt.Fprintf(os.Stderr, "key-id: %s\n", *keyID)
	}
	return nil
}

// loadEd25519Key parses an ed25519 private key from disk. Accepts:
//   - 64 raw bytes (the full Ed25519 private key including the
//     public-key tail produced by ed25519.GenerateKey).
//   - 32 raw bytes (the seed; we expand via ed25519.NewKeyFromSeed).
//   - Base64 (standard or URL-safe) of either of the above.
//
// PEM is intentionally not supported in v1 — adding x509-PEM
// parsing pulls in heavier dependencies and the file-level
// shape is unstable enough that the bytes-then-base64 union is
// less error-prone for the publisher.
func loadEd25519Key(path string) (ed25519.PrivateKey, error) {
	// gosec G304 — publisher-provided CLI argument; the path is
	// the publisher's own ed25519 key on their own machine.
	raw, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	// Try base64 first; if the decode round-trips, use it.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, derr := enc.DecodeString(string(trimmed)); derr == nil {
			if len(decoded) == ed25519.PrivateKeySize || len(decoded) == ed25519.SeedSize {
				trimmed = decoded
				break
			}
		}
	}
	switch len(trimmed) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(append([]byte(nil), trimmed...)), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(trimmed), nil
	default:
		return nil, fmt.Errorf("ed25519 key: unexpected length %d (want %d raw, %d seed, or base64 of either)",
			len(trimmed), ed25519.PrivateKeySize, ed25519.SeedSize)
	}
}

// ---------------------------------------------------------------------------
// upload
// ---------------------------------------------------------------------------

type uploadResult struct {
	UploadID    string `json:"upload_id"`
	BundleURL   string `json:"bundle_url"`
	BundleHash  string `json:"bundle_hash"`
	BundleSize  int64  `json:"bundle_size"`
	ContentType string `json:"content_type"`
}

func runUpload(args []string) error {
	flags := flag.NewFlagSet("upload", flag.ContinueOnError)
	api := flags.String("api", "", "marketplace API base URL (e.g. https://kapp.example.com)")
	token := flags.String("token", "", "bearer token for Authorization header")
	pubID := flags.String("publisher-id", "", "publisher UUID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *api == "" || *token == "" || *pubID == "" || flags.NArg() != 1 {
		return errors.New("usage: upload <bundle.tar.gz> --api <url> --token <jwt> --publisher-id <uuid>")
	}
	// gosec G304 — publisher-provided CLI argument; see
	// runSign for the same rationale.
	body, err := os.ReadFile(flags.Arg(0)) // #nosec G304
	if err != nil {
		return err
	}
	res, err := uploadBundle(context.Background(), *api, *token, *pubID, body)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func uploadBundle(ctx context.Context, api, token, pubID string, body []byte) (*uploadResult, error) {
	// Multipart body must declare a "bundle" file part — that's
	// the field name the server-side uploadPublisherBundle handler
	// pulls out with FormFile("bundle").
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="bundle"; filename="bundle.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	w, err := mw.CreatePart(h)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(body); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	url := strings.TrimRight(api, "/") + "/api/v1/publisher/" + pubID + "/bundles"
	ctx, cancel := context.WithTimeout(ctx, httpUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("upload: status=%d body=%s", resp.StatusCode, string(rb))
	}
	var out uploadResult
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, fmt.Errorf("upload: parse response: %w (body %q)", err, string(rb))
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// publish
// ---------------------------------------------------------------------------

type publishVersionBody struct {
	Manifest             json.RawMessage `json:"manifest"`
	BundleURL            string          `json:"bundle_url,omitempty"`
	BundleHash           string          `json:"bundle_hash"`
	BundleSize           int64           `json:"bundle_size"`
	BundleSignature      string          `json:"bundle_signature,omitempty"`
	BundleSignatureKeyID string          `json:"bundle_signature_key_id,omitempty"`
}

func runPublish(args []string) error {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	api := flags.String("api", "", "marketplace API base URL")
	token := flags.String("token", "", "bearer token")
	pubID := flags.String("publisher-id", "", "publisher UUID")
	extID := flags.String("extension-id", "", "extension UUID")
	sig := flags.String("bundle-signature", "", "optional ed25519 signature (base64) over the bundle bytes")
	sigKeyID := flags.String("bundle-signature-key-id", "", "optional key id matching --bundle-signature")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *api == "" || *token == "" || *pubID == "" || *extID == "" || flags.NArg() != 1 {
		return errors.New("usage: publish <bundle.tar.gz> --api <url> --token <jwt> --publisher-id <uuid> --extension-id <uuid> [--bundle-signature <b64> --bundle-signature-key-id <id>]")
	}
	// gosec G304 — publisher-provided CLI argument; see
	// runSign for the same rationale.
	body, err := os.ReadFile(flags.Arg(0)) // #nosec G304
	if err != nil {
		return err
	}
	rb, err := bundle.Extract(body)
	if err != nil {
		return fmt.Errorf("validate before publish: %w", err)
	}

	ctx := context.Background()
	up, err := uploadBundle(ctx, *api, *token, *pubID, body)
	if err != nil {
		return err
	}

	// Re-encode the manifest as JSON for the wire — bundle.Extract
	// already validated + canonicalised the parsed shape.
	manJSON, err := json.Marshal(rb.Manifest)
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}

	wire := publishVersionBody{
		Manifest:             manJSON,
		BundleURL:            up.BundleURL,
		BundleHash:           up.BundleHash,
		BundleSize:           up.BundleSize,
		BundleSignature:      *sig,
		BundleSignatureKeyID: *sigKeyID,
	}
	reqBody, err := json.Marshal(wire)
	if err != nil {
		return err
	}

	url := strings.TrimRight(*api, "/") + "/api/v1/publisher/" + *pubID + "/extensions/" + *extID + "/versions"
	ctx, cancel := context.WithTimeout(ctx, httpUploadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("publish: status=%d body=%s", resp.StatusCode, string(respBody))
	}
	fmt.Println(string(respBody))
	return nil
}
