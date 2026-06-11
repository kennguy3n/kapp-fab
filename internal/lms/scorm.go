package lms

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/audit"
	"github.com/kennguy3n/kapp-fab/internal/files"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// SCORM content_type values (also added to the lesson schema enum).
const (
	ContentTypeScorm12   = "scorm_12"
	ContentTypeScorm2004 = "scorm_2004"
)

// SCORM 1.2 lesson_status / SCORM 2004 completion+success vocab.
const (
	scormStatusPassed     = "passed"
	scormStatusCompleted  = "completed"
	scormStatusFailed     = "failed"
	scormStatusIncomplete = "incomplete"
	scormStatusBrowsed    = "browsed"
	scormStatusNotAttempt = "not attempted"
	scormStatusUnknown    = "unknown"
)

var (
	// ErrInvalidScormPackage is returned when an uploaded SCORM archive
	// is not a readable ZIP or is otherwise structurally invalid.
	ErrInvalidScormPackage = errors.New("lms: invalid scorm package")
	// ErrScormManifestMissing is returned when a SCORM archive does not
	// contain the mandatory imsmanifest.xml at its root.
	ErrScormManifestMissing = errors.New("lms: imsmanifest.xml not found in package")
)

// CMIData is the subset of the SCORM runtime data model we persist. The
// runtime endpoints (initialize/commit/terminate) marshal the SCORM RTE
// key/value pairs into this struct before mapping to lms.progress.
type CMIData struct {
	// Version selects the interpretation of the status fields and the
	// session-time format. One of ContentTypeScorm12 / ContentTypeScorm2004.
	Version string `json:"version"`
	// SCORM 1.2: cmi.core.lesson_status.
	LessonStatus string `json:"lesson_status"`
	// SCORM 2004: cmi.completion_status + cmi.success_status.
	CompletionStatus string `json:"completion_status"`
	SuccessStatus    string `json:"success_status"`
	// cmi.core.score.raw (0..100). Pointer so "unset" is distinct from 0.
	ScoreRaw *float64 `json:"score_raw,omitempty"`
	// cmi.score.scaled (-1..1) — SCORM 2004 only.
	ScoreScaled *float64 `json:"score_scaled,omitempty"`
	// cmi.core.session_time (1.2: HHHH:MM:SS.ss) or cmi.session_time
	// (2004: ISO-8601 duration PT…). Added to accumulated time on commit.
	SessionTime string `json:"session_time"`
	// cmi.suspend_data — opaque resume blob (≤ 4096 chars in 1.2,
	// 64000 in 2004); persisted verbatim into progress metadata.
	SuspendData string `json:"suspend_data"`
}

// ProgressMapping is the lms.progress projection of a CMI commit.
type ProgressMapping struct {
	Status         string
	Score          *decimal.Decimal
	SessionSeconds int64
	SuspendData    string
	Completed      bool
}

// MapCMIToProgress translates a SCORM CMI commit into the LMS progress
// model, per the data-model mapping:
//
//	cmi.core.lesson_status / completion+success → lms.progress.status
//	cmi.core.score.raw                          → lms.progress.score
//	cmi.core.session_time                       → time_spent (accumulated)
//	cmi.suspend_data                            → metadata.suspend_data
//
// Pure (no I/O) so the mapping table is exhaustively unit-tested. An
// unknown/blank status maps to in_progress (the learner has launched
// the SCO but not reached a terminal state).
func MapCMIToProgress(cmi CMIData) (ProgressMapping, error) {
	out := ProgressMapping{SuspendData: cmi.SuspendData}

	out.Status = scormStatusToProgress(cmi)
	out.Completed = out.Status == ProgressCompleted

	if cmi.ScoreRaw != nil {
		d := decimal.NewFromFloat(*cmi.ScoreRaw)
		out.Score = &d
	} else if cmi.ScoreScaled != nil {
		// SCORM 2004 cmi.score.scaled is normalized to [-1,1]; express it
		// as a percentage so the single numeric score column is comparable
		// across versions. Clamp to [0,100]: the lesson_progress score is a
		// percentage (the lms.lesson KType defines score with min 0), and a
		// rare negative scaled score has no meaningful sub-zero percentage.
		pct := *cmi.ScoreScaled * 100
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		d := decimal.NewFromFloat(pct)
		out.Score = &d
	}

	secs, err := parseScormDuration(cmi.Version, cmi.SessionTime)
	if err != nil {
		return ProgressMapping{}, err
	}
	out.SessionSeconds = secs
	return out, nil
}

// scormStatusToProgress collapses the SCORM 1.2 / 2004 status vocab onto
// the three-valued lms.progress status.
func scormStatusToProgress(cmi CMIData) string {
	switch normalizeScormVersion(cmi.Version) {
	case ContentTypeScorm2004:
		// 2004 splits completion (did they finish?) from success
		// (did they pass?). Completed XOR passed both count as done.
		completion := strings.ToLower(strings.TrimSpace(cmi.CompletionStatus))
		success := strings.ToLower(strings.TrimSpace(cmi.SuccessStatus))
		if completion == scormStatusCompleted || success == scormStatusPassed {
			return ProgressCompleted
		}
		if completion == scormStatusNotAttempt && success == "" {
			return ProgressNotStarted
		}
		return ProgressInProgress
	default: // SCORM 1.2
		status := strings.ToLower(strings.TrimSpace(cmi.LessonStatus))
		switch status {
		case scormStatusPassed, scormStatusCompleted:
			return ProgressCompleted
		case scormStatusNotAttempt, "":
			// A blank/whitespace-only lesson_status means the SCO has
			// launched but not yet reported a state → in_progress. Test
			// the trimmed value (not the raw field) so "   " behaves like
			// "". The explicit "not attempted" token is the only value
			// that maps to not_started.
			if status == "" {
				return ProgressInProgress
			}
			return ProgressNotStarted
		default: // failed / incomplete / browsed
			return ProgressInProgress
		}
	}
}

func normalizeScormVersion(v string) string {
	if strings.Contains(v, "2004") {
		return ContentTypeScorm2004
	}
	return ContentTypeScorm12
}

// parseScormDuration converts a SCORM session-time token to whole
// seconds. SCORM 1.2 uses CMITimespan "HHHH:MM:SS.ss"; SCORM 2004 uses
// an ISO-8601 duration "P[n]DT[n]H[n]M[n]S". An empty token is 0.
func parseScormDuration(version, token string) (int64, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, nil
	}
	if normalizeScormVersion(version) == ContentTypeScorm2004 || strings.HasPrefix(token, "P") {
		return parseISO8601Duration(token)
	}
	return parseCMITimespan(token)
}

// parseCMITimespan parses "HHHH:MM:SS.ss" (SCORM 1.2). Hours may exceed
// two digits; fractional seconds are truncated.
func parseCMITimespan(token string) (int64, error) {
	parts := strings.Split(token, ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("%w: bad session_time %q", ErrInvalidScormPackage, token)
	}
	h, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: hours in %q", ErrInvalidScormPackage, token)
	}
	m, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%w: minutes in %q", ErrInvalidScormPackage, token)
	}
	secFloat, err := strconv.ParseFloat(parts[2], 64)
	if err != nil || secFloat < 0 || secFloat >= 60 {
		return 0, fmt.Errorf("%w: seconds in %q", ErrInvalidScormPackage, token)
	}
	if h < 0 {
		return 0, fmt.Errorf("%w: negative hours in %q", ErrInvalidScormPackage, token)
	}
	return h*3600 + m*60 + int64(secFloat), nil
}

// parseISO8601Duration parses the time-oriented subset of ISO-8601
// durations SCORM 2004 emits, e.g. "PT1H30M5S", "P1DT2H". Year/month
// are rejected (SCORM session time never uses them — they're ambiguous
// in seconds).
func parseISO8601Duration(token string) (int64, error) {
	if !strings.HasPrefix(token, "P") {
		return 0, fmt.Errorf("%w: bad ISO duration %q", ErrInvalidScormPackage, token)
	}
	body := token[1:]
	var datePart, timePart string
	if i := strings.Index(body, "T"); i >= 0 {
		datePart, timePart = body[:i], body[i+1:]
	} else {
		datePart = body
	}
	var total int64
	// Date part: only days (D) supported; weeks (W) handled standalone.
	if datePart != "" {
		num := strings.Builder{}
		for _, r := range datePart {
			switch {
			case r >= '0' && r <= '9' || r == '.':
				num.WriteRune(r)
			case r == 'D':
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return 0, fmt.Errorf("%w: days in %q", ErrInvalidScormPackage, token)
				}
				total += int64(v * 86400)
				num.Reset()
			case r == 'W':
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return 0, fmt.Errorf("%w: weeks in %q", ErrInvalidScormPackage, token)
				}
				total += int64(v * 7 * 86400)
				num.Reset()
			default:
				return 0, fmt.Errorf("%w: unsupported date unit %q in %q", ErrInvalidScormPackage, string(r), token)
			}
		}
	}
	if timePart != "" {
		num := strings.Builder{}
		for _, r := range timePart {
			switch {
			case r >= '0' && r <= '9' || r == '.':
				num.WriteRune(r)
			case r == 'H':
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return 0, fmt.Errorf("%w: hours in %q", ErrInvalidScormPackage, token)
				}
				total += int64(v * 3600)
				num.Reset()
			case r == 'M':
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return 0, fmt.Errorf("%w: minutes in %q", ErrInvalidScormPackage, token)
				}
				total += int64(v * 60)
				num.Reset()
			case r == 'S':
				v, err := strconv.ParseFloat(num.String(), 64)
				if err != nil {
					return 0, fmt.Errorf("%w: seconds in %q", ErrInvalidScormPackage, token)
				}
				total += int64(v)
				num.Reset()
			default:
				return 0, fmt.Errorf("%w: unsupported time unit %q in %q", ErrInvalidScormPackage, string(r), token)
			}
		}
	}
	return total, nil
}

// ---------------------------------------------------------------------------
// Package manifest parsing.
// ---------------------------------------------------------------------------

// ScormManifest is the parsed view of imsmanifest.xml we need to launch
// a package: the SCORM version and the launch href of the first
// (default) resource.
type ScormManifest struct {
	Version    string `json:"version"`
	LaunchHref string `json:"launch_href"`
	Title      string `json:"title"`
}

// manifestXML mirrors the imsmanifest.xml elements we read. Namespaces
// are ignored via the local-name match encoding/xml performs when the
// struct tag omits the namespace.
type manifestXML struct {
	XMLName       xml.Name `xml:"manifest"`
	SchemaVersion string   `xml:"metadata>schemaversion"`
	Schema        string   `xml:"metadata>schema"`
	Organizations struct {
		Default string `xml:"default,attr"`
		Org     []struct {
			ID    string `xml:"identifier,attr"`
			Title string `xml:"title"`
		} `xml:"organization"`
	} `xml:"organizations"`
	Resources struct {
		Resource []struct {
			ID        string `xml:"identifier,attr"`
			Type      string `xml:"type,attr"`
			Href      string `xml:"href,attr"`
			ScormType string `xml:"scormType,attr"`
		} `xml:"resource"`
	} `xml:"resources"`
}

// ParseScormManifest reads imsmanifest.xml bytes and extracts the launch
// href + SCORM version. The version is inferred from the schemaversion
// element ("1.2" → scorm_12; "2004"/"CAM 1.3" → scorm_2004).
func ParseScormManifest(data []byte) (*ScormManifest, error) {
	var m manifestXML
	if err := xml.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidScormPackage, err)
	}
	var launch string
	for _, r := range m.Resources.Resource {
		if r.Href != "" {
			launch = r.Href
			break
		}
	}
	if launch == "" {
		return nil, fmt.Errorf("%w: no resource with a launch href", ErrInvalidScormPackage)
	}
	title := ""
	if len(m.Organizations.Org) > 0 {
		title = m.Organizations.Org[0].Title
	}
	return &ScormManifest{
		Version:    scormVersionFromSchema(m.SchemaVersion),
		LaunchHref: launch,
		Title:      title,
	}, nil
}

func scormVersionFromSchema(schemaVersion string) string {
	if strings.Contains(schemaVersion, "1.2") {
		return ContentTypeScorm12
	}
	// "2004 3rd Edition", "CAM 1.3", etc. all denote SCORM 2004.
	return ContentTypeScorm2004
}

// ---------------------------------------------------------------------------
// Package storage + runtime persistence.
// ---------------------------------------------------------------------------

// ScormStore extracts uploaded packages into the tenant object store
// and persists SCORM runtime commits into lesson_progress.
type ScormStore struct {
	pool    *pgxpool.Pool
	objects files.ObjectStore
	auditor audit.Logger
	now     func() time.Time
}

// NewScormStore wires a SCORM store. objects is the per-tenant object
// store (ZK Object Fabric) packages are extracted into.
func NewScormStore(pool *pgxpool.Pool, objects files.ObjectStore, auditor audit.Logger) *ScormStore {
	return &ScormStore{
		pool:    pool,
		objects: objects,
		auditor: auditor,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// WithClock substitutes the time source (tests).
func (s *ScormStore) WithClock(now func() time.Time) *ScormStore {
	if now != nil {
		s.now = now
	}
	return s
}

// RuntimeState is the persisted SCORM runtime for one (enrollment,
// lesson), shaped for the initialize handshake. The SCORM player
// rehydrates cmi.core.* from these fields so a learner resumes exactly
// where they left off (suspend_data + accumulated time).
type RuntimeState struct {
	Status           string           `json:"status"`
	Score            *decimal.Decimal `json:"score,omitempty"`
	TimeSpentSeconds int64            `json:"time_spent_seconds"`
	SuspendData      string           `json:"suspend_data"`
	Exists           bool             `json:"exists"`
}

// RuntimeState reads the current lesson_progress row for a SCORM
// initialize call. Returns Exists=false (not an error) when the learner
// has never launched the SCO, so the player starts a fresh attempt.
func (s *ScormStore) RuntimeState(ctx context.Context, tenantID, enrollmentID, lessonID uuid.UUID) (*RuntimeState, error) {
	if tenantID == uuid.Nil || enrollmentID == uuid.Nil || lessonID == uuid.Nil {
		return nil, errors.New("lms: tenant_id, enrollment_id, lesson_id required")
	}
	out := &RuntimeState{}
	err := platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var meta []byte
		row := tx.QueryRow(ctx,
			`SELECT status, score, time_spent_seconds, metadata
			   FROM lesson_progress
			  WHERE tenant_id = $1 AND enrollment_id = $2 AND lesson_id = $3`,
			tenantID, enrollmentID, lessonID,
		)
		if err := row.Scan(&out.Status, &out.Score, &out.TimeSpentSeconds, &meta); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		out.Exists = true
		if len(meta) > 0 {
			var m map[string]string
			if err := json.Unmarshal(meta, &m); err == nil {
				out.SuspendData = m["suspend_data"]
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read scorm runtime: %w", err)
	}
	return out, nil
}

// ScormPackage is the result of extracting an uploaded ZIP: the storage
// key prefix the files live under and the parsed manifest.
type ScormPackage struct {
	KeyPrefix string        `json:"key_prefix"`
	Manifest  ScormManifest `json:"manifest"`
	FileCount int           `json:"file_count"`
}

// ExtractPackage unzips a SCORM package, validates it carries an
// imsmanifest.xml, and stores every entry under
// "scorm/{tenantID}/{lessonID}/..." in the tenant object store. The
// tenant id is part of the key (in addition to any per-tenant routing
// the object store performs) so packages stay isolated even on a shared
// fallback bucket. Returns the parsed
// manifest with the launch href rewritten to the stored prefix. The
// object store is content-addressed/idempotent so re-uploading the same
// package is safe.
func (s *ScormStore) ExtractPackage(ctx context.Context, tenantID, lessonID uuid.UUID, zipBytes []byte) (*ScormPackage, error) {
	if s.objects == nil {
		return nil, fmt.Errorf("%w: object store not configured", ErrInvalidScormPackage)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidScormPackage, err)
	}
	// Decompression-bomb defence: bound the entry count up front (the zip
	// central directory is cheap to inspect) and the cumulative extracted
	// size as we read, in addition to the per-file cap below. Together
	// these stop a small archive from exhausting object-store capacity.
	if len(zr.File) > maxScormFiles {
		return nil, fmt.Errorf("%w: %d entries exceeds limit of %d", ErrInvalidScormPackage, len(zr.File), maxScormFiles)
	}
	keyPrefix := fmt.Sprintf("scorm/%s/%s", tenantID, lessonID)
	var manifest *ScormManifest
	fileCount := 0
	var totalBytes int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		// Reject path traversal — a malicious package must never write
		// outside its prefix.
		clean := path.Clean(f.Name)
		if strings.HasPrefix(clean, "..") || path.IsAbs(clean) || strings.Contains(clean, "../") {
			return nil, fmt.Errorf("%w: unsafe path %q", ErrInvalidScormPackage, f.Name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%w: open %s: %w", ErrInvalidScormPackage, f.Name, err)
		}
		content, err := io.ReadAll(io.LimitReader(rc, maxScormFileBytes+1))
		// The entry is fully read above; a read-only zip entry closer has
		// no buffered writes to flush, so a Close error is not actionable.
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("%w: read %s: %w", ErrInvalidScormPackage, f.Name, err)
		}
		if int64(len(content)) > maxScormFileBytes {
			return nil, fmt.Errorf("%w: %s exceeds %d bytes", ErrInvalidScormPackage, f.Name, maxScormFileBytes)
		}
		totalBytes += int64(len(content))
		if totalBytes > maxScormTotalBytes {
			return nil, fmt.Errorf("%w: total extracted size exceeds %d bytes", ErrInvalidScormPackage, maxScormTotalBytes)
		}
		if path.Base(clean) == "imsmanifest.xml" && path.Dir(clean) == "." {
			parsed, perr := ParseScormManifest(content)
			if perr != nil {
				return nil, perr
			}
			manifest = parsed
		}
		key := path.Join(keyPrefix, clean)
		if err := s.objects.Put(ctx, key, contentTypeFor(clean), content); err != nil {
			return nil, fmt.Errorf("store %s: %w", key, err)
		}
		fileCount++
	}
	if manifest == nil {
		return nil, ErrScormManifestMissing
	}
	// Rewrite launch href to the stored prefix so the player can load
	// it directly from the object store.
	manifest.LaunchHref = path.Join(keyPrefix, manifest.LaunchHref)
	return &ScormPackage{KeyPrefix: keyPrefix, Manifest: *manifest, FileCount: fileCount}, nil
}

// SCORM package extraction limits — defence-in-depth against
// decompression bombs and accidental oversized uploads.
const (
	// maxScormFileBytes caps a single entry in a SCORM package (50 MiB),
	// bounding memory + storage from a hostile or accidental huge upload.
	maxScormFileBytes = 50 << 20
	// maxScormFiles caps the number of entries in a package, stopping an
	// archive of millions of tiny files from exhausting write capacity.
	maxScormFiles = 10000
	// maxScormTotalBytes caps the cumulative extracted size across all
	// entries (512 MiB), independent of the per-file and upload caps.
	maxScormTotalBytes = 512 << 20
)

func contentTypeFor(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return "text/html"
	case ".js":
		return "application/javascript"
	case ".css":
		return "text/css"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".mp4":
		return "video/mp4"
	default:
		return "application/octet-stream"
	}
}

// CommitRuntime applies a SCORM CMI commit to the lesson's progress row.
//
// Session time is accumulated as a DELTA, not a raw add. Per the SCORM
// spec, cmi.core.session_time / cmi.session_time is the cumulative time
// the learner has spent in the SCO during the *current* session, which a
// SCO typically re-reports (unchanged) on both LMSCommit and LMSFinish.
// Naively adding it on every commit double-counts (a 10-minute session
// committed then finished would record 20 minutes). Instead we persist
// the last cumulative value in metadata.last_session_seconds and add only
// the increment since the previous commit; a value lower than the stored
// one means the SCO re-launched (a new session reset its timer), so the
// full new value is added. suspend_data replaces the stored blob, and
// status/score are projected via MapCMIToProgress. Returns the resulting
// progress projection.
func (s *ScormStore) CommitRuntime(ctx context.Context, tenantID, enrollmentID, lessonID uuid.UUID, cmi CMIData, actor *uuid.UUID) (*Progress, error) {
	if tenantID == uuid.Nil || enrollmentID == uuid.Nil || lessonID == uuid.Nil {
		return nil, errors.New("lms: tenant_id, enrollment_id, lesson_id required")
	}
	mapping, err := MapCMIToProgress(cmi)
	if err != nil {
		return nil, err
	}
	var out Progress
	err = platform.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		now := s.now()
		var completedAt *time.Time
		if mapping.Completed {
			completedAt = &now
		}
		// last_session_seconds records the cumulative session_time this
		// commit reported so the next commit can add only the increment
		// (see the DELTA accumulation in the SQL below).
		meta, _ := json.Marshal(map[string]string{
			"suspend_data":         mapping.SuspendData,
			"last_session_seconds": strconv.FormatInt(mapping.SessionSeconds, 10),
		})
		row := tx.QueryRow(ctx,
			`INSERT INTO lesson_progress
			    (tenant_id, enrollment_id, lesson_id, status, score, attempts,
			     started_at, completed_at, updated_at, time_spent_seconds, metadata)
			 VALUES ($1,$2,$3,$4,$5,1,$6,$7,$6,$8,$9)
			 ON CONFLICT (tenant_id, enrollment_id, lesson_id) DO UPDATE
			    -- Completion is terminal: a learner revisiting a finished
			    -- lesson (SCO re-commits 'in_progress'/'incomplete') must
			    -- not regress a 'completed' row, which would leave status
			    -- inconsistent with the preserved completed_at below.
			    SET status             = CASE
			                                 WHEN lesson_progress.status = 'completed'
			                                 THEN lesson_progress.status
			                                 ELSE EXCLUDED.status
			                             END,
			        -- Score is a high-water mark once a lesson is completed: a
			        -- post-completion revisit that reports a lower score must not
			        -- regress the recorded grade (gradebook semantics). While the
			        -- lesson is still in progress the latest score wins, so a live
			        -- SCO can revise its score downward mid-attempt. Gating on the
			        -- existing row's 'completed' status mirrors the status guard
			        -- above; GREATEST(COALESCE(...), COALESCE(...)) is null-safe
			        -- (picks the non-null side when only one is present).
			        score              = CASE
			                                 WHEN lesson_progress.status = 'completed'
			                                 THEN GREATEST(
			                                          COALESCE(lesson_progress.score, EXCLUDED.score),
			                                          COALESCE(EXCLUDED.score, lesson_progress.score)
			                                      )
			                                 ELSE COALESCE(EXCLUDED.score, lesson_progress.score)
			                             END,
			        started_at         = COALESCE(lesson_progress.started_at, EXCLUDED.started_at),
			        completed_at       = COALESCE(lesson_progress.completed_at, EXCLUDED.completed_at),
			        updated_at         = EXCLUDED.updated_at,
			        -- attempts is deliberately NOT incremented here (unlike
			        -- xapi.upsertProgressTx / Store.UpsertProgress, which bump
			        -- it per statement/upsert). A SCORM SCO calls LMSCommit
			        -- repeatedly as an auto-save *within a single attempt*, so
			        -- incrementing per commit would inflate the counter into a
			        -- commit count rather than an attempt count. A new attempt
			        -- is the SCO re-launch (LMSInitialize), tracked separately.
			        -- Accumulate the DELTA of the cumulative session_time,
			        -- not the raw value: a SCO re-reports the same session
			        -- total on LMSCommit then LMSFinish, so adding $8 each
			        -- time double-counts. Add ($8 - last_session_seconds)
			        -- when the timer advanced; if $8 is lower the SCO
			        -- re-launched (new session reset its timer) so add $8.
			        time_spent_seconds = lesson_progress.time_spent_seconds +
			            CASE
			                WHEN $8 >= COALESCE((lesson_progress.metadata->>'last_session_seconds')::bigint, 0)
			                THEN $8 - COALESCE((lesson_progress.metadata->>'last_session_seconds')::bigint, 0)
			                ELSE $8
			            END,
			        metadata           = lesson_progress.metadata || EXCLUDED.metadata
			 RETURNING tenant_id, enrollment_id, lesson_id, status, score,
			           attempts, started_at, completed_at, updated_at`,
			tenantID, enrollmentID, lessonID, mapping.Status, mapping.Score,
			now, completedAt, mapping.SessionSeconds, meta,
		)
		if err := row.Scan(
			&out.TenantID, &out.EnrollmentID, &out.LessonID, &out.Status, &out.Score,
			&out.Attempts, &out.StartedAt, &out.CompletedAt, &out.UpdatedAt,
		); err != nil {
			return err
		}
		if s.auditor == nil {
			return nil
		}
		kind := audit.ActorUser
		if actor == nil {
			kind = audit.ActorSystem
		}
		tid := lessonID
		return s.auditor.LogTx(ctx, tx, audit.Entry{
			TenantID:    tenantID,
			ActorID:     actor,
			ActorKind:   kind,
			Action:      "lms.scorm.commit",
			TargetKType: KTypeLesson,
			TargetID:    &tid,
			After:       mustJSON(out),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("commit scorm runtime: %w", err)
	}
	return &out, nil
}
