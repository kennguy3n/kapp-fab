package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Cell provisioning — Phase G / multi-region automation.
//
// The autoscaler (autoscaler.go) decides WHEN a cell should grow or
// shrink. A CellProvisioner decides HOW that decision is actuated
// against real infrastructure: spinning up another docker-compose
// stack / bare-metal node (ScriptProvisioner), calling an
// operator-owned control plane (WebhookProvisioner), or doing nothing
// but logging (NoopProvisioner — the default, preserving the historic
// "observe only" behaviour).
//
// The interface is deliberately small and side-effect oriented. The
// autoscaler never blocks the rest of its tick on a slow provider:
// provisioning is best-effort and failures are logged, not fatal.

// CellProvisionState is the transient runtime health a provisioner
// observes for a cell and returns from Status(). It is a probe result
// (e.g. the script provisioner reports "ready" when containers are up,
// "failed" when they are not), NOT the persisted lifecycle of the cell.
//
// It is deliberately DISTINCT from the cells.status column added in
// migrations/000081_cell_region_metadata.sql (see CellStatus* below).
// These values must never be written into cells.status — only the
// CellStatus* lifecycle values satisfy that column's CHECK constraint.
type CellProvisionState string

const (
	// CellStatePending — provisioning has been requested but the cell
	// is not yet serving traffic.
	CellStatePending CellProvisionState = "pending"
	// CellStateReady — the cell is provisioned and accepting tenants.
	CellStateReady CellProvisionState = "ready"
	// CellStateDraining — the cell is being emptied ahead of teardown;
	// tenants are migrating off and no new tenants should be placed.
	CellStateDraining CellProvisionState = "draining"
	// CellStateFailed — provisioning or deprovisioning errored and the
	// cell needs operator attention.
	CellStateFailed CellProvisionState = "failed"
	// CellStateUnknown — the provisioner cannot determine the state
	// (e.g. the NoopProvisioner, which keeps no records).
	CellStateUnknown CellProvisionState = "unknown"
)

// CellStatus* are the canonical values of the persisted cells.status
// column. They mirror EXACTLY the CHECK constraint defined in
// migrations/000081_cell_region_metadata.sql, so any code that reads or
// writes cells.status should use these constants rather than string
// literals. Unlike CellProvisionState (a transient health probe), this
// is the durable lifecycle of a cell row in the control plane.
const (
	// CellStatusActive — the cell is serving tenants. Default for every
	// row and the only status the autoscaler evaluates.
	CellStatusActive = "active"
	// CellStatusProvisioning — a provisioner is standing the cell up; it
	// is not yet serving traffic and must not be a placement/drain target.
	CellStatusProvisioning = "provisioning"
	// CellStatusDraining — the cell is being emptied ahead of teardown.
	CellStatusDraining = "draining"
	// CellStatusDeprovisioned — the cell has been torn down and is
	// retained only for audit/history.
	CellStatusDeprovisioned = "deprovisioned"
)

// Cell is the control-plane view of a provisioned cell returned by a
// provisioner. It is intentionally a superset of the columns the
// autoscaler reads (CellSnapshot) plus the region-placement metadata
// persisted by migration 000081.
type Cell struct {
	ID         string            `json:"id"`
	Region     string            `json:"region"`
	Provider   string            `json:"provider,omitempty"`
	Zone       string            `json:"zone,omitempty"`
	Endpoint   string            `json:"endpoint,omitempty"`
	MaxTenants int               `json:"max_tenants"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	CreatedAt  time.Time         `json:"created_at,omitempty"`
}

// CellSpec is the desired shape of a new cell handed to Provision. The
// autoscaler fills MaxTenants from its policy; an operator-driven call
// can additionally pin provider/zone for data-residency placement.
type CellSpec struct {
	MaxTenants int               `json:"max_tenants"`
	Provider   string            `json:"provider,omitempty"`
	Zone       string            `json:"zone,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// CellProvisionStatus is the result of a Status query.
type CellProvisionStatus struct {
	CellID  string             `json:"cell_id"`
	State   CellProvisionState `json:"state"`
	Message string             `json:"message,omitempty"`
}

// CellProvisioner actuates autoscaler decisions against real
// infrastructure. Implementations must be safe for concurrent use by
// the single autoscale loop goroutine and must honour ctx cancellation
// / deadlines.
type CellProvisioner interface {
	// Provision creates a new cell in the given region. The returned
	// Cell describes the newly created (or in-flight) cell; its ID is
	// the handle Deprovision and Status accept.
	Provision(ctx context.Context, region string, spec CellSpec) (*Cell, error)
	// Deprovision tears down the named cell. It MUST be a no-op-safe
	// idempotent call: deprovisioning an already-gone cell returns nil.
	Deprovision(ctx context.Context, cellID string) error
	// Status reports the current lifecycle state of the named cell.
	Status(ctx context.Context, cellID string) (CellProvisionStatus, error)
}

// AutoscaleProvisioningEnabled reports whether the autoscaler should
// actuate scale decisions against the configured provisioner. Defaults
// to false so the historic observe-only behaviour is preserved unless
// an operator explicitly opts in with KAPP_AUTOSCALE_PROVISION=true.
func AutoscaleProvisioningEnabled() bool {
	return getenvBool("KAPP_AUTOSCALE_PROVISION", false)
}

// ProvisionerFromEnv constructs the provisioner selected by
// KAPP_AUTOSCALE_PROVISIONER (script | webhook | noop). It is the
// single place env wiring for provisioners lives so config.go is left
// untouched (avoiding a merge conflict with concurrent work on that
// file). A nil/empty selector yields the NoopProvisioner.
func ProvisionerFromEnv(logger *slog.Logger) (CellProvisioner, error) {
	if logger == nil {
		logger = slog.Default()
	}
	kind := strings.ToLower(strings.TrimSpace(getenv("KAPP_AUTOSCALE_PROVISIONER", "noop")))
	switch kind {
	case "", "noop":
		return NewNoopProvisioner(logger), nil
	case "script":
		path := getenv("KAPP_AUTOSCALE_PROVISION_SCRIPT", "scripts/provision-cell.sh")
		timeout := getenvDuration("KAPP_AUTOSCALE_PROVISION_TIMEOUT", 5*time.Minute)
		return NewScriptProvisioner(path, timeout, logger), nil
	case "webhook":
		url := strings.TrimSpace(os.Getenv("KAPP_AUTOSCALE_WEBHOOK_URL"))
		if url == "" {
			return nil, errors.New("platform: KAPP_AUTOSCALE_PROVISIONER=webhook requires KAPP_AUTOSCALE_WEBHOOK_URL")
		}
		timeout := getenvDuration("KAPP_AUTOSCALE_WEBHOOK_TIMEOUT", 30*time.Second)
		return NewWebhookProvisioner(url, timeout, logger), nil
	default:
		return nil, fmt.Errorf("platform: unknown KAPP_AUTOSCALE_PROVISIONER %q (want script|webhook|noop)", kind)
	}
}

// ---------------------------------------------------------------------------
// NoopProvisioner — default. Logs the decision, mutates nothing.
// ---------------------------------------------------------------------------

// NoopProvisioner records what would have happened without touching any
// infrastructure. It is the default so enabling KAPP_AUTOSCALE_PROVISION
// without choosing a real provisioner is safe (the loop logs intentions
// rather than silently doing nothing).
type NoopProvisioner struct {
	logger *slog.Logger
}

// NewNoopProvisioner returns a NoopProvisioner. A nil logger falls back
// to slog.Default.
func NewNoopProvisioner(logger *slog.Logger) *NoopProvisioner {
	if logger == nil {
		logger = slog.Default()
	}
	return &NoopProvisioner{logger: logger}
}

// Provision logs the intent and returns a synthetic Cell so callers can
// treat the noop path uniformly. No infrastructure is created.
func (p *NoopProvisioner) Provision(_ context.Context, region string, spec CellSpec) (*Cell, error) {
	id := fmt.Sprintf("cell-%s-%s", sanitizeRegion(region), uuid.NewString()[:8])
	p.logger.Info("provision: noop scale_up",
		"cell_id", id, "region", region, "max_tenants", spec.MaxTenants)
	return &Cell{
		ID:         id,
		Region:     region,
		Provider:   spec.Provider,
		Zone:       spec.Zone,
		MaxTenants: spec.MaxTenants,
		Metadata:   spec.Metadata,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

// Deprovision logs the intent. No infrastructure is torn down.
func (p *NoopProvisioner) Deprovision(_ context.Context, cellID string) error {
	p.logger.Info("provision: noop scale_down", "cell_id", cellID)
	return nil
}

// Status always reports unknown: the noop provisioner keeps no records.
func (p *NoopProvisioner) Status(_ context.Context, cellID string) (CellProvisionStatus, error) {
	return CellProvisionStatus{
		CellID:  cellID,
		State:   CellStateUnknown,
		Message: "noop provisioner does not track cell state",
	}, nil
}

// ---------------------------------------------------------------------------
// ScriptProvisioner — shells out to scripts/provision-cell.sh.
// ---------------------------------------------------------------------------

// ScriptProvisioner actuates decisions by executing an operator-owned
// shell script (scripts/provision-cell.sh by default). This is the
// pragmatic choice for docker-compose and bare-metal fleets where
// "provision a cell" means running a deploy command on a host.
//
// Contract with the script (see scripts/provision-cell.sh):
//
//	provision <region> <max_tenants> <provider> <zone>
//	    → exit 0 and print a JSON Cell object on stdout.
//	deprovision <cell_id>
//	    → exit 0 (idempotent; absent cell is success).
//	status <cell_id>
//	    → exit 0 and print a JSON {state,message} object on stdout.
type ScriptProvisioner struct {
	scriptPath string
	timeout    time.Duration
	logger     *slog.Logger
	// run is the command executor seam. Production uses runScript
	// (os/exec); tests inject a fake so no real process is spawned.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewScriptProvisioner returns a ScriptProvisioner that invokes
// scriptPath. A non-positive timeout defaults to 5 minutes; a nil
// logger falls back to slog.Default.
func NewScriptProvisioner(scriptPath string, timeout time.Duration, logger *slog.Logger) *ScriptProvisioner {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &ScriptProvisioner{
		scriptPath: scriptPath,
		timeout:    timeout,
		logger:     logger,
		run:        runScript,
	}
}

// runScript executes name with args, returning combined stdout. stderr
// is folded into the error so a failing script surfaces its diagnostics.
func runScript(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg != "" {
			return stdout.Bytes(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.Bytes(), err
	}
	return stdout.Bytes(), nil
}

func (p *ScriptProvisioner) exec(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	return p.run(ctx, p.scriptPath, args...)
}

// Provision runs `<script> provision <region> <max_tenants> <provider> <zone>`
// and parses the JSON Cell printed on stdout.
func (p *ScriptProvisioner) Provision(ctx context.Context, region string, spec CellSpec) (*Cell, error) {
	out, err := p.exec(ctx, "provision", region,
		strconv.Itoa(spec.MaxTenants), spec.Provider, spec.Zone)
	if err != nil {
		return nil, fmt.Errorf("script provision: %w", err)
	}
	cell, err := parseCellJSON(out)
	if err != nil {
		return nil, fmt.Errorf("script provision: %w", err)
	}
	// Backfill fields the script may have omitted from its output so
	// the caller always sees a coherent Cell.
	if cell.Region == "" {
		cell.Region = region
	}
	if cell.MaxTenants == 0 {
		cell.MaxTenants = spec.MaxTenants
	}
	p.logger.Info("provision: script scale_up",
		"cell_id", cell.ID, "region", cell.Region, "max_tenants", cell.MaxTenants)
	return cell, nil
}

// Deprovision runs `<script> deprovision <cell_id>`.
func (p *ScriptProvisioner) Deprovision(ctx context.Context, cellID string) error {
	if _, err := p.exec(ctx, "deprovision", cellID); err != nil {
		return fmt.Errorf("script deprovision %q: %w", cellID, err)
	}
	p.logger.Info("provision: script scale_down", "cell_id", cellID)
	return nil
}

// Status runs `<script> status <cell_id>` and parses the JSON result.
func (p *ScriptProvisioner) Status(ctx context.Context, cellID string) (CellProvisionStatus, error) {
	out, err := p.exec(ctx, "status", cellID)
	if err != nil {
		return CellProvisionStatus{CellID: cellID, State: CellStateUnknown}, fmt.Errorf("script status %q: %w", cellID, err)
	}
	st, err := parseStatusJSON(out)
	if err != nil {
		return CellProvisionStatus{CellID: cellID, State: CellStateUnknown}, fmt.Errorf("script status %q: %w", cellID, err)
	}
	st.CellID = cellID
	return st, nil
}

// ---------------------------------------------------------------------------
// WebhookProvisioner — POSTs the decision to an operator-configured URL.
// ---------------------------------------------------------------------------

// WebhookProvisioner actuates decisions by POSTing JSON to an
// operator-configured endpoint. This suits custom infrastructure where
// a small adapter service translates the request into Terraform /
// Pulumi / cloud-API calls.
type WebhookProvisioner struct {
	url    string
	client *http.Client
	logger *slog.Logger
}

// NewWebhookProvisioner returns a WebhookProvisioner. A non-positive
// timeout defaults to 30s; a nil logger falls back to slog.Default.
func NewWebhookProvisioner(url string, timeout time.Duration, logger *slog.Logger) *WebhookProvisioner {
	if logger == nil {
		logger = slog.Default()
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &WebhookProvisioner{
		url:    url,
		client: &http.Client{Timeout: timeout},
		logger: logger,
	}
}

// webhookRequest is the JSON envelope POSTed for every action.
type webhookRequest struct {
	Action string    `json:"action"`
	Region string    `json:"region,omitempty"`
	CellID string    `json:"cell_id,omitempty"`
	Spec   *CellSpec `json:"spec,omitempty"`
}

// webhookResponse is the JSON envelope expected back. Provision returns
// Cell; Status returns Status; Deprovision may return an empty body.
type webhookResponse struct {
	Cell   *Cell                `json:"cell,omitempty"`
	Status *CellProvisionStatus `json:"status,omitempty"`
	Error  string               `json:"error,omitempty"`
}

func (p *WebhookProvisioner) post(ctx context.Context, req webhookRequest) (*webhookResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Bound the body read so a misbehaving endpoint cannot exhaust
	// memory; provisioner responses are tiny JSON objects.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("webhook %s: status %d: %s", req.Action, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out webhookResponse
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	if out.Error != "" {
		return nil, fmt.Errorf("webhook %s: %s", req.Action, out.Error)
	}
	return &out, nil
}

// Provision POSTs {action:"provision", region, spec} and expects a
// {cell:{...}} response.
func (p *WebhookProvisioner) Provision(ctx context.Context, region string, spec CellSpec) (*Cell, error) {
	specCopy := spec
	out, err := p.post(ctx, webhookRequest{Action: "provision", Region: region, Spec: &specCopy})
	if err != nil {
		return nil, err
	}
	if out.Cell == nil || strings.TrimSpace(out.Cell.ID) == "" {
		return nil, errors.New("webhook provision: response missing cell.id")
	}
	if out.Cell.Region == "" {
		out.Cell.Region = region
	}
	p.logger.Info("provision: webhook scale_up",
		"cell_id", out.Cell.ID, "region", out.Cell.Region, "max_tenants", out.Cell.MaxTenants)
	return out.Cell, nil
}

// Deprovision POSTs {action:"deprovision", cell_id}.
func (p *WebhookProvisioner) Deprovision(ctx context.Context, cellID string) error {
	if _, err := p.post(ctx, webhookRequest{Action: "deprovision", CellID: cellID}); err != nil {
		return err
	}
	p.logger.Info("provision: webhook scale_down", "cell_id", cellID)
	return nil
}

// Status POSTs {action:"status", cell_id} and expects a {status:{...}}
// response.
func (p *WebhookProvisioner) Status(ctx context.Context, cellID string) (CellProvisionStatus, error) {
	out, err := p.post(ctx, webhookRequest{Action: "status", CellID: cellID})
	if err != nil {
		return CellProvisionStatus{CellID: cellID, State: CellStateUnknown}, err
	}
	if out.Status == nil {
		return CellProvisionStatus{CellID: cellID, State: CellStateUnknown}, errors.New("webhook status: response missing status")
	}
	st := *out.Status
	if st.CellID == "" {
		st.CellID = cellID
	}
	if st.State == "" {
		st.State = CellStateUnknown
	}
	return st, nil
}

// ---------------------------------------------------------------------------
// shared parsing helpers
// ---------------------------------------------------------------------------

// parseCellJSON decodes a Cell from script stdout. It tolerates leading
// log noise by decoding the last non-empty line as JSON, which lets a
// provisioning script emit human-readable progress before the final
// machine-readable result.
func parseCellJSON(out []byte) (*Cell, error) {
	line := lastJSONLine(out)
	if line == "" {
		return nil, errors.New("empty output (expected JSON cell)")
	}
	var c Cell
	if err := json.Unmarshal([]byte(line), &c); err != nil {
		return nil, fmt.Errorf("parse cell json: %w", err)
	}
	if strings.TrimSpace(c.ID) == "" {
		return nil, errors.New("cell json missing id")
	}
	return &c, nil
}

// parseStatusJSON decodes a CellProvisionStatus from script stdout,
// applying the same last-JSON-line tolerance as parseCellJSON.
func parseStatusJSON(out []byte) (CellProvisionStatus, error) {
	line := lastJSONLine(out)
	if line == "" {
		return CellProvisionStatus{}, errors.New("empty output (expected JSON status)")
	}
	var s CellProvisionStatus
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		return CellProvisionStatus{}, fmt.Errorf("parse status json: %w", err)
	}
	if s.State == "" {
		s.State = CellStateUnknown
	}
	return s, nil
}

// lastJSONLine returns the last non-empty, trimmed line of out that
// looks like a JSON object ('{' … '}'). Returns "" when none match.
func lastJSONLine(out []byte) string {
	lines := strings.Split(string(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// sanitizeRegion reduces a region string to a slug safe for use inside
// a generated cell id (lowercase alphanumerics and dashes).
func sanitizeRegion(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if region == "" {
		return "default"
	}
	var b strings.Builder
	for _, r := range region {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "default"
	}
	return s
}
