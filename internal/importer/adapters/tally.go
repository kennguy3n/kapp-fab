package adapters

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

// TallyConfig is the JSON shape expected in ImportJob.Config for the
// Tally adapter. Unlike the cloud adapters, Tally Prime has no stable
// remote API for SMEs, so the integration is file-based: the operator
// exports masters and/or vouchers from Tally (XML or JSON) and pastes
// the export contents into Payload. This mirrors the inline-payload
// approach the CSV/JSON adapter already uses.
type TallyConfig struct {
	// Format selects the payload parser: "xml" (Tally's native export,
	// the default) or "json" (Tally Prime's JSON export / a
	// hand-assembled equivalent).
	Format string `json:"format,omitempty"`
	// Payload holds the raw Tally export file contents.
	Payload string `json:"payload"`
	// ConceptMap layers per-entity field renames on top of the
	// adapter's built-in mapping table, keyed by Tally entity name
	// ("Ledger", "StockItem", "Voucher").
	ConceptMap map[string]map[string]string `json:"concept_map,omitempty"`
}

// Tally entity names. They double as the keys into the field-map and
// default-target-KType tables.
const (
	tallyEntityLedger    = "Ledger"
	tallyEntityStockItem = "StockItem"
	tallyEntityVoucher   = "Voucher"
)

// TallyAdapter imports Tally Prime master + transaction exports from an
// inline payload.
type TallyAdapter struct{}

// NewTallyAdapter returns the Tally file-import adapter.
func NewTallyAdapter() *TallyAdapter { return &TallyAdapter{} }

// SourceType discriminates the adapter for registry lookup.
func (*TallyAdapter) SourceType() string { return SourceTypeTally }

// Discover parses the payload, counts rows per entity, and stamps a
// SHA-256 checksum over the payload bytes so the reconciler can detect
// truncation. Only entities actually present in the export are
// reported.
func (a *TallyAdapter) Discover(_ context.Context, raw json.RawMessage) (importer.DiscoverResult, error) {
	cfg, err := a.load(raw)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	parsed, err := parseTally(cfg)
	if err != nil {
		return importer.DiscoverResult{}, err
	}
	checksum := checksumBytes([]byte(cfg.Payload))
	result := importer.DiscoverResult{}
	for _, entity := range tallyEntityOrder {
		rows := parsed[entity]
		if len(rows) == 0 {
			continue
		}
		result.Entities = append(result.Entities, importer.DiscoveredEntity{
			Name:     entity,
			RowCount: int64(len(rows)),
			TargetKT: defaultTallyTargetKType[entity],
			Checksum: checksum,
		})
		result.TotalRows += int64(len(rows))
	}
	if len(result.Entities) == 0 {
		result.Notes = append(result.Notes, "tally: no Ledger/StockItem/Voucher records found in payload")
	}
	return result, nil
}

// Export emits one NormalizedRow per parsed record, applying the
// per-entity field map. The Tally GUID (or name/number fallback) is
// recorded as the SourceID.
func (a *TallyAdapter) Export(_ context.Context, raw json.RawMessage, emit func(importer.NormalizedRow) error) error {
	cfg, err := a.load(raw)
	if err != nil {
		return err
	}
	parsed, err := parseTally(cfg)
	if err != nil {
		return err
	}
	for _, entity := range tallyEntityOrder {
		mapping := mergeFieldMaps(defaultTallyFieldMap[entity], cfg.ConceptMap[entity])
		for _, row := range parsed[entity] {
			if err := emit(importer.NormalizedRow{
				Entity:   entity,
				SourceID: tallySourceID(entity, row),
				Data:     applyFieldMap(row, mapping),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *TallyAdapter) load(raw json.RawMessage) (TallyConfig, error) {
	var cfg TallyConfig
	if len(raw) == 0 {
		return cfg, fmt.Errorf("tally: config required")
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("tally: parse config: %w", err)
	}
	if strings.TrimSpace(cfg.Payload) == "" {
		return cfg, fmt.Errorf("tally: payload required")
	}
	if cfg.Format == "" {
		cfg.Format = "xml"
	}
	return cfg, nil
}

// tallyEntityOrder fixes the iteration order so Discover and Export
// agree and output is deterministic.
var tallyEntityOrder = []string{tallyEntityLedger, tallyEntityStockItem, tallyEntityVoucher}

// parseTally decodes the payload into per-entity rows keyed by entity
// name.
func parseTally(cfg TallyConfig) (map[string][]map[string]any, error) {
	switch strings.ToLower(cfg.Format) {
	case "xml", "":
		return parseTallyXML(cfg.Payload)
	case "json":
		return parseTallyJSON(cfg.Payload)
	default:
		return nil, fmt.Errorf("tally: unsupported format %q", cfg.Format)
	}
}

// tallyLedgerEntry is one debit/credit line inside a voucher.
type tallyLedgerEntry struct {
	LedgerName       string `xml:"LEDGERNAME"`
	Amount           string `xml:"AMOUNT"`
	IsDeemedPositive string `xml:"ISDEEMEDPOSITIVE"`
}

// tallyLedger / tallyStockItem / tallyVoucher mirror the subset of
// Tally's export schema the adapter maps. NAME appears as either an
// attribute (import format) or a child element (some export formats),
// so both are captured and the non-empty one wins.
type tallyLedger struct {
	NameAttr       string `xml:"NAME,attr"`
	Name           string `xml:"NAME"`
	Parent         string `xml:"PARENT"`
	OpeningBalance string `xml:"OPENINGBALANCE"`
	GUID           string `xml:"GUID"`
}

type tallyStockItem struct {
	NameAttr       string `xml:"NAME,attr"`
	Name           string `xml:"NAME"`
	Parent         string `xml:"PARENT"`
	BaseUnits      string `xml:"BASEUNITS"`
	OpeningBalance string `xml:"OPENINGBALANCE"`
	GUID           string `xml:"GUID"`
}

type tallyVoucher struct {
	VchType          string             `xml:"VCHTYPE,attr"`
	Date             string             `xml:"DATE"`
	VoucherTypeName  string             `xml:"VOUCHERTYPENAME"`
	VoucherNumber    string             `xml:"VOUCHERNUMBER"`
	Narration        string             `xml:"NARRATION"`
	PartyLedgerName  string             `xml:"PARTYLEDGERNAME"`
	GUID             string             `xml:"GUID"`
	AllLedgerEntries []tallyLedgerEntry `xml:"ALLLEDGERENTRIES.LIST"`
	LedgerEntries    []tallyLedgerEntry `xml:"LEDGERENTRIES.LIST"`
}

// parseTallyXML streams the export and collects LEDGER / STOCKITEM /
// VOUCHER elements wherever they appear in the envelope. Streaming via
// the token decoder keeps the parse robust to the various wrapper
// shapes Tally emits (IMPORTDATA>REQUESTDATA vs DATA>TALLYMESSAGE vs a
// COLLECTION export) without hard-coding a single path.
func parseTallyXML(payload string) (map[string][]map[string]any, error) {
	out := map[string][]map[string]any{}
	dec := xml.NewDecoder(strings.NewReader(payload))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tally: parse xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch se.Name.Local {
		case "LEDGER":
			var l tallyLedger
			if err := dec.DecodeElement(&l, &se); err != nil {
				return nil, fmt.Errorf("tally: decode ledger: %w", err)
			}
			out[tallyEntityLedger] = append(out[tallyEntityLedger], ledgerToRow(l))
		case "STOCKITEM":
			var s tallyStockItem
			if err := dec.DecodeElement(&s, &se); err != nil {
				return nil, fmt.Errorf("tally: decode stock item: %w", err)
			}
			out[tallyEntityStockItem] = append(out[tallyEntityStockItem], stockItemToRow(s))
		case "VOUCHER":
			var v tallyVoucher
			if err := dec.DecodeElement(&v, &se); err != nil {
				return nil, fmt.Errorf("tally: decode voucher: %w", err)
			}
			out[tallyEntityVoucher] = append(out[tallyEntityVoucher], voucherToRow(v))
		}
	}
	return out, nil
}

// parseTallyJSON decodes the JSON export shape:
//
//	{"ledgers": [...], "stock_items": [...], "vouchers": [...]}
//
// Each array element is a raw object whose keys are passed through to
// the field map unchanged, so a JSON export that already uses Tally's
// uppercase field names maps identically to the XML path.
func parseTallyJSON(payload string) (map[string][]map[string]any, error) {
	var doc struct {
		Ledgers    []map[string]any `json:"ledgers"`
		StockItems []map[string]any `json:"stock_items"`
		Vouchers   []map[string]any `json:"vouchers"`
	}
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		return nil, fmt.Errorf("tally: parse json: %w", err)
	}
	return map[string][]map[string]any{
		tallyEntityLedger:    doc.Ledgers,
		tallyEntityStockItem: doc.StockItems,
		tallyEntityVoucher:   doc.Vouchers,
	}, nil
}

func ledgerToRow(l tallyLedger) map[string]any {
	return map[string]any{
		"NAME":           firstNonEmpty(l.NameAttr, l.Name),
		"PARENT":         l.Parent,
		"OPENINGBALANCE": l.OpeningBalance,
		"GUID":           l.GUID,
	}
}

func stockItemToRow(s tallyStockItem) map[string]any {
	return map[string]any{
		"NAME":           firstNonEmpty(s.NameAttr, s.Name),
		"PARENT":         s.Parent,
		"BASEUNITS":      s.BaseUnits,
		"OPENINGBALANCE": s.OpeningBalance,
		"GUID":           s.GUID,
	}
}

func voucherToRow(v tallyVoucher) map[string]any {
	entries := v.AllLedgerEntries
	if len(entries) == 0 {
		entries = v.LedgerEntries
	}
	lines := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		lines = append(lines, map[string]any{
			"LEDGERNAME":       e.LedgerName,
			"AMOUNT":           e.Amount,
			"ISDEEMEDPOSITIVE": e.IsDeemedPositive,
		})
	}
	return map[string]any{
		"VOUCHERNUMBER":   v.VoucherNumber,
		"DATE":            v.Date,
		"VOUCHERTYPENAME": firstNonEmpty(v.VoucherTypeName, v.VchType),
		"NARRATION":       v.Narration,
		"PARTYLEDGERNAME": v.PartyLedgerName,
		"GUID":            v.GUID,
		"LEDGERENTRIES":   lines,
	}
}

// tallySourceID picks the most stable identifier available for a row:
// the Tally GUID when present, else the entity's natural key.
func tallySourceID(entity string, row map[string]any) string {
	if guid, _ := row["GUID"].(string); guid != "" {
		return guid
	}
	switch entity {
	case tallyEntityVoucher:
		if n, _ := row["VOUCHERNUMBER"].(string); n != "" {
			return n
		}
	default:
		if n, _ := row["NAME"].(string); n != "" {
			return n
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// defaultTallyTargetKType maps each Tally entity to its default target
// KType, pre-filled in the wizard's mapping step.
var defaultTallyTargetKType = map[string]string{
	tallyEntityLedger:    "finance.account",
	tallyEntityStockItem: "inventory.item",
	tallyEntityVoucher:   "finance.journal_entry",
}

// defaultTallyFieldMap maps Tally source fields onto KType field names
// per entity. Unmapped keys pass through verbatim.
var defaultTallyFieldMap = map[string]map[string]string{
	tallyEntityLedger: {
		"NAME": "name", "PARENT": "group", "OPENINGBALANCE": "opening_balance", "GUID": "source_guid",
	},
	tallyEntityStockItem: {
		"NAME": "name", "PARENT": "group", "BASEUNITS": "uom", "OPENINGBALANCE": "opening_stock", "GUID": "source_guid",
	},
	tallyEntityVoucher: {
		"VOUCHERNUMBER": "number", "DATE": "date", "VOUCHERTYPENAME": "type",
		"NARRATION": "memo", "PARTYLEDGERNAME": "party", "LEDGERENTRIES": "lines", "GUID": "source_guid",
	},
}

// SuggestTallyFieldMapping surfaces a best-effort source→target field
// map for fields the built-in table does not cover.
func SuggestTallyFieldMapping(sourceFields, targetFields []string) map[string]string {
	return SuggestFieldMapping(sourceFields, targetFields, 0.5)
}
