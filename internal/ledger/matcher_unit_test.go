package ledger

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"", "abc", 3},
		{"kitten", "sitting", 3},
		{"flaw", "lawn", 2},
		{"same", "same", 0},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q,%q) = %d; want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestNormalizeForMatch(t *testing.T) {
	cases := map[string]string{
		"TFR *Acme Corp 1234": "tfr acme corp",
		"  AWS   EMEA  ":      "aws emea",
		"12345":               "",
		"Uber*Eats":           "uber eats",
	}
	for in, want := range cases {
		if got := normalizeForMatch(in); got != want {
			t.Errorf("normalizeForMatch(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestDescriptionSimilarity(t *testing.T) {
	if s := DescriptionSimilarity("ACME CORP", "ACME CORP"); s != 1 {
		t.Errorf("identical => %v; want 1", s)
	}
	if s := DescriptionSimilarity("", "anything"); s != 0 {
		t.Errorf("empty => %v; want 0", s)
	}
	// Reordered tokens still score highly thanks to token overlap.
	reordered := DescriptionSimilarity("ACME CORP LTD", "LTD CORP ACME")
	if reordered < 0.6 {
		t.Errorf("reordered similarity = %v; want >= 0.6", reordered)
	}
	// Unrelated strings score low.
	if s := DescriptionSimilarity("grocery store", "airline ticket"); s > 0.4 {
		t.Errorf("unrelated similarity = %v; want < 0.4", s)
	}
	// Digit-only reference noise is ignored, so these collapse equal.
	if s := DescriptionSimilarity("TFR ACME 1111", "TFR ACME 9999"); s != 1 {
		t.Errorf("ref-noise similarity = %v; want 1", s)
	}
}

func TestLevenshteinRatioUsesRuneLength(t *testing.T) {
	// "café" vs "cafe" differ by a single rune substitution (é→e). The
	// rune length of both is 4, so the ratio must be 1 - 1/4 = 0.75. A
	// byte-length denominator would use 5 ("café" is 5 bytes) and wrongly
	// report 0.8, inflating similarity for accented EU/UK counterparties.
	if got := levenshteinRatio("café", "cafe"); got < 0.749 || got > 0.751 {
		t.Errorf("levenshteinRatio(café,cafe) = %v; want ~0.75 (rune-based)", got)
	}
	// Two identical multi-byte strings stay exactly 1.
	if got := levenshteinRatio("Société Générale", "Société Générale"); got != 1 {
		t.Errorf("identical accented => %v; want 1", got)
	}
}

func TestTokenOverlap(t *testing.T) {
	if o := tokenOverlap("aa bb cc", "aa bb cc"); o != 1 {
		t.Errorf("identical token sets => %v; want 1", o)
	}
	if o := tokenOverlap("aa bb", "cc dd"); o != 0 {
		t.Errorf("disjoint => %v; want 0", o)
	}
	// Half overlap: {aa,bb} vs {bb,cc} => inter 1, union 3.
	if o := tokenOverlap("aa bb", "bb cc"); o < 0.33 || o > 0.34 {
		t.Errorf("half overlap = %v; want ~0.333", o)
	}
}

func TestDescriptionKeyStableAndPrivate(t *testing.T) {
	k1 := DescriptionKey("TFR *Acme Corp 1234")
	k2 := DescriptionKey("tfr acme corp 5678")
	if k1 == "" || k1 != k2 {
		t.Fatalf("keys should match across ref noise: %q vs %q", k1, k2)
	}
	if len(k1) != 16 {
		t.Errorf("key length = %d; want 16 hex chars", len(k1))
	}
	if DescriptionKey("12345") != "" {
		t.Errorf("digit-only description should yield empty key")
	}
}

// scoreOne is the heart of the confidence model; exercise each component.
func TestScoreOneComponents(t *testing.T) {
	vd := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	opts := MatchOptions{Window: 7 * 24 * time.Hour}
	abs := decimal.RequireFromString("100.00")

	t.Run("exact amount same day", func(t *testing.T) {
		c := candidate{entryID: uuid.New(), postedAt: vd, memo: "", lineAmount: abs, accountCode: "6000"}
		score, reason := scoreOne(abs, vd, "", c, nil, opts)
		// 0.40 exact + 0.20 same-day = 0.60.
		if score < 0.59 || score > 0.61 {
			t.Errorf("score = %v; want ~0.60 (%s)", score, reason)
		}
	})

	t.Run("amount within tolerance", func(t *testing.T) {
		o := MatchOptions{Window: 7 * 24 * time.Hour, AmountTolerance: decimal.RequireFromString("0.05")}
		c := candidate{entryID: uuid.New(), postedAt: vd, lineAmount: decimal.RequireFromString("100.03"), accountCode: "6000"}
		score, _ := scoreOne(abs, vd, "", c, nil, o)
		// 0.25 tolerance + 0.20 same-day = 0.45.
		if score < 0.44 || score > 0.46 {
			t.Errorf("score = %v; want ~0.45", score)
		}
	})

	t.Run("date proximity decays", func(t *testing.T) {
		c := candidate{entryID: uuid.New(), postedAt: vd.AddDate(0, 0, 7), lineAmount: abs, accountCode: "6000"}
		score, _ := scoreOne(abs, vd, "", c, nil, opts)
		// At the window edge proximity contributes ~0, so just the 0.40 exact.
		if score < 0.39 || score > 0.42 {
			t.Errorf("score = %v; want ~0.40 at window edge", score)
		}
	})

	t.Run("learned counterparty adds", func(t *testing.T) {
		c := candidate{entryID: uuid.New(), postedAt: vd, lineAmount: abs, accountCode: "6000"}
		learned := map[string]int{"6000": 3}
		score, reason := scoreOne(abs, vd, "", c, learned, opts)
		// 0.40 + 0.20 same-day + 0.20 learned = 0.80.
		if score < 0.79 || score > 0.81 {
			t.Errorf("score = %v; want ~0.80 (%s)", score, reason)
		}
	})

	t.Run("capped at 1.0", func(t *testing.T) {
		c := candidate{entryID: uuid.New(), postedAt: vd, memo: "acme corp", lineAmount: abs, accountCode: "6000"}
		learned := map[string]int{"6000": 9}
		score, _ := scoreOne(abs, vd, "acme corp", c, learned, opts)
		if score > 1.0 {
			t.Errorf("score = %v; must be capped at 1.0", score)
		}
	})
}

func TestScoreCandidatesDedupesEntryAndSorts(t *testing.T) {
	vd := time.Date(2024, 3, 10, 0, 0, 0, 0, time.UTC)
	opts := MatchOptions{Window: 7 * 24 * time.Hour}
	abs := decimal.RequireFromString("50")
	entryA := uuid.New()
	entryB := uuid.New()
	cands := []candidate{
		// Two lines of entryA: the exact one should win.
		{entryID: entryA, postedAt: vd, lineAmount: decimal.RequireFromString("50"), accountCode: "6000"},
		{entryID: entryA, postedAt: vd.AddDate(0, 0, 6), lineAmount: decimal.RequireFromString("50"), accountCode: "6000"},
		// entryB weaker (further out).
		{entryID: entryB, postedAt: vd.AddDate(0, 0, 6), lineAmount: decimal.RequireFromString("50"), accountCode: "7000"},
	}
	scored := scoreCandidates(abs, vd, "", cands, nil, opts)
	if len(scored) != 2 {
		t.Fatalf("got %d scored; want 2 (entry-deduped)", len(scored))
	}
	if scored[0].entryID != entryA {
		t.Errorf("top entry = %v; want entryA (same-day exact)", scored[0].entryID)
	}
	if scored[0].Confidence < scored[1].Confidence {
		t.Errorf("results not sorted desc: %v < %v", scored[0].Confidence, scored[1].Confidence)
	}
}

func TestClassifyCadence(t *testing.T) {
	mk := func(days ...int) []time.Time {
		base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
		out := make([]time.Time, len(days))
		for i, d := range days {
			out[i] = base.AddDate(0, 0, d)
		}
		return out
	}
	cases := []struct {
		name  string
		dates []time.Time
		want  string
	}{
		{"weekly", mk(0, 7, 14, 21), "weekly"},
		{"monthly", mk(0, 30, 60, 90), "monthly"},
		{"biweekly", mk(0, 14, 28), "biweekly"},
		{"irregular", mk(0, 3, 40), "irregular"},
		{"too few", mk(0), "irregular"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyCadence(tc.dates); got != tc.want {
				t.Errorf("classifyCadence = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestMatchOptionsDefaults(t *testing.T) {
	var o MatchOptions
	if o.window() != DefaultMatchWindow {
		t.Errorf("default window = %v; want %v", o.window(), DefaultMatchWindow)
	}
	if o.minConfidence() != DefaultMinConfidence {
		t.Errorf("default minConfidence = %v; want %v", o.minConfidence(), DefaultMinConfidence)
	}
}

func TestSuggestMatchesValidatesInput(t *testing.T) {
	m := &SmartMatcher{}
	if _, err := m.SuggestMatches(context.TODO(), uuid.Nil, uuid.New(), MatchOptions{}); err == nil {
		t.Error("expected error on nil tenant")
	}
	if _, err := m.SuggestMatches(context.TODO(), uuid.New(), uuid.Nil, MatchOptions{}); err == nil {
		t.Error("expected error on nil txn")
	}
}

func TestDetectTransferValidatesInput(t *testing.T) {
	m := &SmartMatcher{}
	if _, err := m.DetectTransfer(context.TODO(), uuid.Nil, uuid.New()); err == nil {
		t.Error("expected error on nil tenant")
	}
	if _, err := m.DetectTransfer(context.TODO(), uuid.New(), uuid.Nil); err == nil {
		t.Error("expected error on nil txn")
	}
}

func TestDetectDuplicateValidatesInput(t *testing.T) {
	m := &SmartMatcher{}
	if _, err := m.DetectDuplicate(context.TODO(), uuid.Nil, uuid.New()); err == nil {
		t.Error("expected error on nil tenant")
	}
	if _, err := m.DetectDuplicate(context.TODO(), uuid.New(), uuid.Nil); err == nil {
		t.Error("expected error on nil txn")
	}
}

func TestMentionsTransfer(t *testing.T) {
	hits := []string{
		"TRANSFER TO SAVINGS", "Internal xfer", "TRF 12345",
		"Move to savings pot", "weekly INTERNAL sweep",
	}
	for _, s := range hits {
		if !mentionsTransfer(s) {
			t.Errorf("mentionsTransfer(%q) = false; want true", s)
		}
	}
	misses := []string{"AMAZON WEB SERVICES", "TFL TRAVEL", "Salary", ""}
	for _, s := range misses {
		if mentionsTransfer(s) {
			t.Errorf("mentionsTransfer(%q) = true; want false", s)
		}
	}
}

func TestTransferConfidence(t *testing.T) {
	base := time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC)

	// Same-day, no cue: base 0.6 + 0.3*1.0 = 0.9.
	if got := transferConfidence(base, base, "ACME", "ACME"); got < 0.899 || got > 0.901 {
		t.Errorf("same-day confidence = %v; want ~0.90", got)
	}
	// Same-day with a transfer cue: 0.9 + 0.1 ≈ 1.0 (never exceeds 1).
	if got := transferConfidence(base, base, "TRANSFER OUT", "deposit"); got < 0.999 || got > 1 {
		t.Errorf("same-day+cue confidence = %v; want ~1.0", got)
	}
	// Window edge (4 days): proximity 0 → base 0.6, no cue.
	edge := base.Add(DefaultTransferWindow)
	if got := transferConfidence(base, edge, "x", "y"); got < 0.599 || got > 0.601 {
		t.Errorf("window-edge confidence = %v; want ~0.60", got)
	}
	// Sign of the gap must not matter (symmetric).
	mid := base.Add(2 * 24 * time.Hour)
	if a, b := transferConfidence(base, mid, "x", "y"), transferConfidence(mid, base, "x", "y"); a != b {
		t.Errorf("confidence not symmetric: %v vs %v", a, b)
	}
	// Beyond the window proximity clamps at 0, never negative.
	far := base.Add(10 * 24 * time.Hour)
	if got := transferConfidence(base, far, "x", "y"); got < 0.599 || got > 0.601 {
		t.Errorf("beyond-window confidence = %v; want ~0.60 (clamped)", got)
	}
}
