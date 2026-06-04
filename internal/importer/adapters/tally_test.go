package adapters

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kennguy3n/kapp-fab/internal/importer"
)

const tallyXMLPayload = `<ENVELOPE>
 <BODY>
  <IMPORTDATA>
   <REQUESTDATA>
    <TALLYMESSAGE>
     <LEDGER NAME="Cash" RESERVEDNAME="">
      <PARENT>Cash-in-Hand</PARENT>
      <OPENINGBALANCE>1000.00</OPENINGBALANCE>
      <GUID>ledger-guid-1</GUID>
     </LEDGER>
    </TALLYMESSAGE>
    <TALLYMESSAGE>
     <STOCKITEM NAME="Widget">
      <PARENT>Primary</PARENT>
      <BASEUNITS>Nos</BASEUNITS>
     </STOCKITEM>
    </TALLYMESSAGE>
    <TALLYMESSAGE>
     <VOUCHER VCHTYPE="Sales" ACTION="Create">
      <DATE>20240401</DATE>
      <VOUCHERTYPENAME>Sales</VOUCHERTYPENAME>
      <VOUCHERNUMBER>S-1</VOUCHERNUMBER>
      <NARRATION>Sold widgets</NARRATION>
      <PARTYLEDGERNAME>Acme</PARTYLEDGERNAME>
      <ALLLEDGERENTRIES.LIST>
       <LEDGERNAME>Sales</LEDGERNAME>
       <AMOUNT>-500.00</AMOUNT>
       <ISDEEMEDPOSITIVE>No</ISDEEMEDPOSITIVE>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
       <LEDGERNAME>Acme</LEDGERNAME>
       <AMOUNT>500.00</AMOUNT>
       <ISDEEMEDPOSITIVE>Yes</ISDEEMEDPOSITIVE>
      </ALLLEDGERENTRIES.LIST>
     </VOUCHER>
    </TALLYMESSAGE>
   </REQUESTDATA>
  </IMPORTDATA>
 </BODY>
</ENVELOPE>`

func collect(t *testing.T, a *TallyAdapter, raw json.RawMessage) map[string][]importer.NormalizedRow {
	t.Helper()
	byEntity := map[string][]importer.NormalizedRow{}
	if err := a.Export(context.Background(), raw, func(row importer.NormalizedRow) error {
		byEntity[row.Entity] = append(byEntity[row.Entity], row)
		return nil
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return byEntity
}

func TestTallyXMLDiscoverAndExport(t *testing.T) {
	cfg, _ := json.Marshal(TallyConfig{Format: "xml", Payload: tallyXMLPayload})
	a := NewTallyAdapter()

	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if disco.TotalRows != 3 {
		t.Fatalf("TotalRows = %d, want 3", disco.TotalRows)
	}
	counts := map[string]int64{}
	for _, e := range disco.Entities {
		counts[e.Name] = e.RowCount
		if e.Checksum == "" {
			t.Errorf("entity %s missing checksum", e.Name)
		}
	}
	if counts["Ledger"] != 1 || counts["StockItem"] != 1 || counts["Voucher"] != 1 {
		t.Fatalf("unexpected counts: %+v", counts)
	}

	rows := collect(t, a, cfg)

	ledger := rows["Ledger"][0]
	if ledger.SourceID != "ledger-guid-1" {
		t.Errorf("ledger SourceID = %q, want guid", ledger.SourceID)
	}
	if ledger.Data["name"] != "Cash" || ledger.Data["group"] != "Cash-in-Hand" || ledger.Data["opening_balance"] != "1000.00" {
		t.Errorf("ledger mapping wrong: %+v", ledger.Data)
	}

	stock := rows["StockItem"][0]
	if stock.SourceID != "Widget" { // no GUID -> falls back to NAME
		t.Errorf("stock SourceID = %q, want Widget", stock.SourceID)
	}
	if stock.Data["uom"] != "Nos" {
		t.Errorf("stock uom mapping wrong: %+v", stock.Data)
	}

	voucher := rows["Voucher"][0]
	if voucher.SourceID != "S-1" {
		t.Errorf("voucher SourceID = %q, want S-1", voucher.SourceID)
	}
	if voucher.Data["number"] != "S-1" || voucher.Data["type"] != "Sales" || voucher.Data["party"] != "Acme" {
		t.Errorf("voucher mapping wrong: %+v", voucher.Data)
	}
	lines, ok := voucher.Data["lines"].([]map[string]any)
	if !ok || len(lines) != 2 {
		t.Fatalf("voucher lines wrong: %+v", voucher.Data["lines"])
	}
	if lines[0]["LEDGERNAME"] != "Sales" || lines[1]["LEDGERNAME"] != "Acme" {
		t.Errorf("ledger entries not parsed: %+v", lines)
	}
}

func TestTallyJSONExport(t *testing.T) {
	payload := `{
      "ledgers": [{"NAME": "Bank", "PARENT": "Bank Accounts", "GUID": "g-9"}],
      "stock_items": [],
      "vouchers": [{"VOUCHERNUMBER": "P-7", "VOUCHERTYPENAME": "Purchase"}]
    }`
	cfg, _ := json.Marshal(TallyConfig{Format: "json", Payload: payload})
	a := NewTallyAdapter()

	disco, err := a.Discover(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	// stock_items is empty, so only Ledger + Voucher are reported.
	if len(disco.Entities) != 2 || disco.TotalRows != 2 {
		t.Fatalf("unexpected discover: %+v", disco.Entities)
	}

	rows := collect(t, a, cfg)
	if rows["Ledger"][0].Data["name"] != "Bank" || rows["Ledger"][0].SourceID != "g-9" {
		t.Errorf("json ledger mapping wrong: %+v", rows["Ledger"][0])
	}
	if rows["Voucher"][0].Data["number"] != "P-7" {
		t.Errorf("json voucher mapping wrong: %+v", rows["Voucher"][0])
	}
}

func TestTallyEmptyAndInvalid(t *testing.T) {
	a := NewTallyAdapter()

	// Empty payload is rejected.
	raw, _ := json.Marshal(TallyConfig{Payload: "  "})
	if _, err := a.Discover(context.Background(), raw); err == nil {
		t.Error("expected error on empty payload")
	}

	// No recognised records -> a note, no entities, no error.
	raw, _ = json.Marshal(TallyConfig{Format: "xml", Payload: "<ENVELOPE></ENVELOPE>"})
	disco, err := a.Discover(context.Background(), raw)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(disco.Entities) != 0 || len(disco.Notes) == 0 {
		t.Errorf("expected no entities and a note, got %+v / %v", disco.Entities, disco.Notes)
	}

	// Malformed XML surfaces as an error.
	raw, _ = json.Marshal(TallyConfig{Format: "xml", Payload: "<ENVELOPE><LEDGER>"})
	if _, err := a.Discover(context.Background(), raw); err == nil {
		t.Error("expected parse error on malformed XML")
	}

	// Unknown format is rejected.
	raw, _ = json.Marshal(TallyConfig{Format: "yaml", Payload: "x: 1"})
	if _, err := a.Discover(context.Background(), raw); err == nil {
		t.Error("expected error on unsupported format")
	}
}
