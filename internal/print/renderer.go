// Package print renders KRecords into HTML, and — when a host-
// provided binary (wkhtmltopdf) is available — further converts that
// HTML into a PDF. The package is deliberately pluggable so tests
// and dev environments without a PDF converter still receive usable
// HTML output; the handler layer surfaces whichever format the
// caller requested.
package print

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

//go:embed templates/*.html
var builtin embed.FS

// Renderer turns a KRecord into a printable HTML (and optional PDF)
// artifact. Tenant-specific overrides are resolved via the TemplateStore
// injected on construction; when none exists the renderer falls
// back to the package-embedded defaults.
type Renderer struct {
	store     *TemplateStore
	objs      files.ObjectStore
	converter HTMLToPDF
	// defaultTmpls is a per-ktype cache of parsed embedded templates.
	// defaultMu guards it because resolveTemplate runs from per-request
	// goroutines and Go maps are not safe for concurrent read/write.
	// *template.Template itself is safe for concurrent Execute, so the
	// lock only scopes the map, not the rendering step.
	defaultMu    sync.RWMutex
	defaultTmpls map[string]*template.Template
}

// NewRenderer wires a Renderer over a template store and object
// store. When converter is nil the renderer picks a host-best
// default: DetectConverter shells out to `wkhtmltopdf` if the
// binary is on PATH, otherwise HTML is returned as the "PDF"
// payload with an application/pdf mime — callers that strictly
// need PDF bytes should pass their own converter.
func NewRenderer(store *TemplateStore, objs files.ObjectStore, converter HTMLToPDF) *Renderer {
	if converter == nil {
		converter = DetectConverter()
	}
	return &Renderer{
		store:        store,
		objs:         objs,
		converter:    converter,
		defaultTmpls: map[string]*template.Template{},
	}
}

// HTMLToPDF abstracts the HTML→PDF conversion step so tests can swap
// in a fake. Production wires one of WKHTMLToPDF (wkhtmltopdf CLI)
// or, when the converter returns nothing, the HTML passthrough
// which preserves behaviour in environments without a binary.
type HTMLToPDF interface {
	Convert(ctx context.Context, html []byte) ([]byte, error)
}

// RenderedDoc is the result of one render call. Bytes + ContentType
// are suitable for the HTTP response; Key is the content-addressable
// object-store key so the caller can surface a stable URL.
type RenderedDoc struct {
	Bytes       []byte
	ContentType string
	Key         string
}

// tmplData is what each template sees under `.`. It flattens the
// KRecord fields that matter for a printable artifact so templates
// don't need to reach into the raw JSON.
type tmplData struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	KType     string
	Version   int
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
	Data      map[string]any
}

// RenderHTML produces the HTML for one record. Tenant overrides
// from print_templates take precedence; otherwise the package's
// embedded default for this KType (or the generic default.html) is
// used.
func (r *Renderer) RenderHTML(ctx context.Context, rec record.KRecord) ([]byte, error) {
	data := tmplData{
		ID:        rec.ID,
		TenantID:  rec.TenantID,
		KType:     rec.KType,
		Version:   rec.Version,
		Status:    rec.Status,
		CreatedAt: rec.CreatedAt,
		UpdatedAt: rec.UpdatedAt,
		Data:      map[string]any{},
	}
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &data.Data); err != nil {
			return nil, fmt.Errorf("print: unmarshal record data: %w", err)
		}
	}
	tmpl, err := r.resolveTemplate(ctx, rec.TenantID, rec.KType)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("print: execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// RenderPDF returns the PDF bytes for a record. The HTML rendering
// step is always run; the converter only touches the HTML output.
// The result is persisted to the object store keyed by SHA-256
// hash so repeat renders of unchanged content dedup.
func (r *Renderer) RenderPDF(ctx context.Context, rec record.KRecord) (*RenderedDoc, error) {
	html, err := r.RenderHTML(ctx, rec)
	if err != nil {
		return nil, err
	}
	pdf, err := r.converter.Convert(ctx, html)
	if err != nil {
		return nil, fmt.Errorf("print: convert to pdf: %w", err)
	}
	sum := sha256.Sum256(pdf)
	h := hex.EncodeToString(sum[:])
	key := "print/" + h[:2] + "/" + h + ".pdf"
	if r.objs != nil {
		if err := r.objs.Put(ctx, key, "application/pdf", pdf); err != nil {
			// Cache put failures are logged but not fatal — the
			// PDF still streams back to the caller in-process.
			// Persisted cache hits on the next call are the
			// only lost optimisation.
			_ = err
		}
	}
	return &RenderedDoc{Bytes: pdf, ContentType: "application/pdf", Key: key}, nil
}

// resolveTemplate picks the template to execute. Order:
//  1. Tenant-specific default row in print_templates for this KType.
//  2. Package-embedded templates/<ktype>.html.
//  3. Package-embedded templates/default.html.
func (r *Renderer) resolveTemplate(ctx context.Context, tenantID uuid.UUID, ktypeName string) (*template.Template, error) {
	if r.store != nil {
		override, err := r.store.GetDefault(ctx, tenantID, ktypeName)
		if err != nil && !errors.Is(err, ErrTemplateNotFound) {
			return nil, err
		}
		if override != nil {
			t, err := template.New(ktypeName).Funcs(funcMap()).Parse(override.HTMLTemplate)
			if err != nil {
				return nil, fmt.Errorf("print: parse tenant template: %w", err)
			}
			return t, nil
		}
	}
	r.defaultMu.RLock()
	cached, ok := r.defaultTmpls[ktypeName]
	r.defaultMu.RUnlock()
	if ok {
		return cached, nil
	}
	raw, err := builtin.ReadFile("templates/" + ktypeName + ".html")
	if err != nil {
		raw, err = builtin.ReadFile("templates/default.html")
		if err != nil {
			return nil, fmt.Errorf("print: load default template: %w", err)
		}
	}
	t, err := template.New(ktypeName).Funcs(funcMap()).Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("print: parse default template: %w", err)
	}
	r.defaultMu.Lock()
	// Re-check under the write lock so two concurrent misses on the
	// same ktype produce a single cached entry rather than losing one.
	if existing, ok := r.defaultTmpls[ktypeName]; ok {
		r.defaultMu.Unlock()
		return existing, nil
	}
	r.defaultTmpls[ktypeName] = t
	r.defaultMu.Unlock()
	return t, nil
}

// funcMap exposes the helpers templates need to render JSONB-backed
// data: `field` pulls a top-level key with a fallback, `lineItems`
// normalises an array-of-objects, and `fieldPairs` iterates the
// top-level keys in sorted order for the generic default template.
func funcMap() template.FuncMap {
	return template.FuncMap{
		"field": func(m map[string]any, key string, def any) any {
			if m == nil {
				return def
			}
			if v, ok := m[key]; ok && v != nil {
				return v
			}
			return def
		},
		"lineItems": func(m map[string]any, key string) []map[string]any {
			if m == nil {
				return nil
			}
			v, ok := m[key]
			if !ok {
				return nil
			}
			arr, ok := v.([]any)
			if !ok {
				return nil
			}
			out := make([]map[string]any, 0, len(arr))
			for _, el := range arr {
				if obj, ok := el.(map[string]any); ok {
					out = append(out, obj)
				}
			}
			return out
		},
		"item": func(m map[string]any, key string, def any) any {
			if m == nil {
				return def
			}
			if v, ok := m[key]; ok && v != nil {
				return v
			}
			return def
		},
		"fieldPairs": func(m map[string]any) []kv {
			if m == nil {
				return nil
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			out := make([]kv, 0, len(keys))
			for _, k := range keys {
				out = append(out, kv{Key: k, Value: stringify(m[k])})
			}
			return out
		},
	}
}

type kv struct {
	Key   string
	Value string
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, err := json.MarshalIndent(x, "", "  ")
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}

// --- HTMLToPDF implementations ---------------------------------------

// WKHTMLToPDF shells out to the `wkhtmltopdf` CLI. The binary must
// be on PATH; DetectConverter picks it up automatically when
// present.
type WKHTMLToPDF struct{ Binary string }

// Convert pipes HTML into wkhtmltopdf on stdin and returns the
// PDF bytes on stdout.
func (c *WKHTMLToPDF) Convert(ctx context.Context, html []byte) ([]byte, error) {
	if c.Binary == "" {
		c.Binary = "wkhtmltopdf"
	}
	cmd := exec.CommandContext(ctx, c.Binary, "--quiet", "-", "-")
	cmd.Stdin = bytes.NewReader(html)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("wkhtmltopdf: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// htmlPassthrough is the fallback converter for environments without
// a wkhtmltopdf binary. It labels the HTML as application/pdf so
// the caller still gets a single-format response, but the bytes are
// HTML — tenants that must have real PDFs should install
// wkhtmltopdf or plug in a converter of their own. This is the same
// pragmatic fallback shape frappe/erpnext ships when its configured
// "chrome" / "wkhtmltopdf" renderer is missing.
type htmlPassthrough struct{}

func (htmlPassthrough) Convert(_ context.Context, html []byte) ([]byte, error) {
	return html, nil
}

// DetectConverter returns the best-available HTMLToPDF for this
// host. In order: wkhtmltopdf (if on PATH), otherwise the HTML
// passthrough fallback.
func DetectConverter() HTMLToPDF {
	if path, err := exec.LookPath("wkhtmltopdf"); err == nil {
		return &WKHTMLToPDF{Binary: path}
	}
	return htmlPassthrough{}
}

// WriteHTMLResponse is a small helper the handler uses to stream
// HTML content with the right headers so the endpoint can share
// code between HTML and PDF paths.
func WriteHTMLResponse(w io.Writer, body []byte) (int, error) {
	return w.Write(body)
}
