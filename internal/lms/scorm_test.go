package lms

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func f64(v float64) *float64 { return &v }

func TestParseScormDuration(t *testing.T) {
	tests := []struct {
		name    string
		version string
		token   string
		want    int64
		wantErr bool
	}{
		{"empty", ContentTypeScorm12, "", 0, false},
		{"1.2 basic", ContentTypeScorm12, "01:30:15", 5415, false},
		{"1.2 fractional seconds truncated", ContentTypeScorm12, "00:00:05.75", 5, false},
		{"1.2 long hours", ContentTypeScorm12, "1000:00:00", 3600000, false},
		{"1.2 bad parts", ContentTypeScorm12, "1:2", 0, true},
		{"1.2 bad minutes", ContentTypeScorm12, "00:99:00", 0, true},
		{"2004 hours+minutes+seconds", ContentTypeScorm2004, "PT1H30M5S", 5405, false},
		{"2004 minutes only", ContentTypeScorm2004, "PT45M", 2700, false},
		{"2004 with days", ContentTypeScorm2004, "P1DT2H", 93600, false},
		{"2004 fractional seconds", ContentTypeScorm2004, "PT0.5S", 0, false},
		{"prefix P inferred even on 1.2", ContentTypeScorm12, "PT10S", 10, false},
		{"2004 bad", ContentTypeScorm2004, "1H", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseScormDuration(tt.version, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.token)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("parseScormDuration(%q,%q) = %d, want %d", tt.version, tt.token, got, tt.want)
			}
		})
	}
}

func TestMapCMIToProgress(t *testing.T) {
	t.Run("1.2 passed completes with score", func(t *testing.T) {
		got, err := MapCMIToProgress(CMIData{
			Version: ContentTypeScorm12, LessonStatus: "passed",
			ScoreRaw: f64(87), SessionTime: "00:10:00", SuspendData: "abc",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != ProgressCompleted || !got.Completed {
			t.Fatalf("want completed, got %+v", got)
		}
		if got.Score == nil || !got.Score.Equal(decimal.NewFromInt(87)) {
			t.Fatalf("want score 87, got %v", got.Score)
		}
		if got.SessionSeconds != 600 {
			t.Fatalf("want 600s, got %d", got.SessionSeconds)
		}
		if got.SuspendData != "abc" {
			t.Fatalf("suspend data lost: %q", got.SuspendData)
		}
	})
	t.Run("1.2 incomplete is in_progress", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm12, LessonStatus: "incomplete"})
		if got.Status != ProgressInProgress || got.Completed {
			t.Fatalf("want in_progress, got %+v", got)
		}
	})
	t.Run("1.2 not attempted is not_started", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm12, LessonStatus: "not attempted"})
		if got.Status != ProgressNotStarted {
			t.Fatalf("want not_started, got %+v", got)
		}
	})
	t.Run("1.2 empty status is in_progress (launched)", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm12, LessonStatus: ""})
		if got.Status != ProgressInProgress {
			t.Fatalf("want in_progress, got %+v", got)
		}
	})
	t.Run("1.2 whitespace-only status behaves like empty (in_progress)", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm12, LessonStatus: "   "})
		if got.Status != ProgressInProgress {
			t.Fatalf("want in_progress, got %+v", got)
		}
	})
	t.Run("2004 completion completed => complete", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm2004, CompletionStatus: "completed", SuccessStatus: "unknown"})
		if got.Status != ProgressCompleted {
			t.Fatalf("want completed, got %+v", got)
		}
	})
	t.Run("2004 passed via success", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm2004, CompletionStatus: "incomplete", SuccessStatus: "passed"})
		if got.Status != ProgressCompleted {
			t.Fatalf("want completed, got %+v", got)
		}
	})
	t.Run("2004 scaled score scaled to 0..100", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm2004, CompletionStatus: "completed", ScoreScaled: f64(0.9)})
		if got.Score == nil || !got.Score.Equal(decimal.NewFromInt(90)) {
			t.Fatalf("want 90, got %v", got.Score)
		}
	})
	t.Run("2004 negative scaled score clamps to 0", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm2004, CompletionStatus: "incomplete", ScoreScaled: f64(-0.5)})
		if got.Score == nil || !got.Score.Equal(decimal.NewFromInt(0)) {
			t.Fatalf("want 0 (clamped), got %v", got.Score)
		}
	})
	t.Run("2004 scaled score above 1 clamps to 100", func(t *testing.T) {
		got, _ := MapCMIToProgress(CMIData{Version: ContentTypeScorm2004, CompletionStatus: "completed", ScoreScaled: f64(1.5)})
		if got.Score == nil || !got.Score.Equal(decimal.NewFromInt(100)) {
			t.Fatalf("want 100 (clamped), got %v", got.Score)
		}
	})
	t.Run("bad session time errors", func(t *testing.T) {
		_, err := MapCMIToProgress(CMIData{Version: ContentTypeScorm12, LessonStatus: "passed", SessionTime: "bogus"})
		if err == nil {
			t.Fatal("expected error")
		}
	})
}

const sampleManifest12 = `<?xml version="1.0"?>
<manifest identifier="M1" xmlns="http://www.imsproject.org/xsd/imscp_rootv1p1p2">
  <metadata><schema>ADL SCORM</schema><schemaversion>1.2</schemaversion></metadata>
  <organizations default="ORG"><organization identifier="ORG"><title>Intro Safety</title></organization></organizations>
  <resources>
    <resource identifier="R1" type="webcontent" href="index.html"></resource>
  </resources>
</manifest>`

const sampleManifest2004 = `<?xml version="1.0"?>
<manifest identifier="M2">
  <metadata><schema>ADL SCORM</schema><schemaversion>2004 3rd Edition</schemaversion></metadata>
  <organizations default="ORG"><organization identifier="ORG"><title>Advanced</title></organization></organizations>
  <resources><resource identifier="R1" type="webcontent" href="start/launch.html"></resource></resources>
</manifest>`

func TestParseScormManifest(t *testing.T) {
	t.Run("1.2", func(t *testing.T) {
		m, err := ParseScormManifest([]byte(sampleManifest12))
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != ContentTypeScorm12 {
			t.Fatalf("want scorm_12, got %s", m.Version)
		}
		if m.LaunchHref != "index.html" {
			t.Fatalf("want index.html, got %s", m.LaunchHref)
		}
		if m.Title != "Intro Safety" {
			t.Fatalf("want title, got %q", m.Title)
		}
	})
	t.Run("2004", func(t *testing.T) {
		m, err := ParseScormManifest([]byte(sampleManifest2004))
		if err != nil {
			t.Fatal(err)
		}
		if m.Version != ContentTypeScorm2004 {
			t.Fatalf("want scorm_2004, got %s", m.Version)
		}
		if m.LaunchHref != "start/launch.html" {
			t.Fatalf("want start/launch.html, got %s", m.LaunchHref)
		}
	})
	t.Run("no resource href", func(t *testing.T) {
		_, err := ParseScormManifest([]byte(`<manifest><resources></resources></manifest>`))
		if !errors.Is(err, ErrInvalidScormPackage) {
			t.Fatalf("want ErrInvalidScormPackage, got %v", err)
		}
	})
	t.Run("malformed xml", func(t *testing.T) {
		_, err := ParseScormManifest([]byte(`<manifest`))
		if !errors.Is(err, ErrInvalidScormPackage) {
			t.Fatalf("want ErrInvalidScormPackage, got %v", err)
		}
	})
}

func TestContentTypeFor(t *testing.T) {
	cases := map[string]string{
		"index.html": "text/html",
		"app.js":     "application/javascript",
		"a/b.css":    "text/css",
		"x.png":      "image/png",
		"movie.mp4":  "video/mp4",
		"blob.bin":   "application/octet-stream",
	}
	for in, want := range cases {
		if got := contentTypeFor(in); got != want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", in, got, want)
		}
	}
}
