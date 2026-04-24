package finance

// KTypePayment is the canonical identifier for finance.payment.
const KTypePayment = "finance.payment"

// WorkflowPayment is the workflow the payment schema registers.
const WorkflowPayment = "finance.payment.lifecycle"

// paymentSchema — ERPNext-style Payment Entry. `payment_type` selects
// receive (customer → us) vs pay (us → supplier). `allocations` is an
// array of `{invoice_id, allocated_amount}` so a single payment can
// settle multiple invoices; the ledger poster validates the sum does
// not exceed the total amount and that each allocation does not
// exceed the target invoice's outstanding balance.
var paymentSchema = []byte(`{
  "name": "finance.payment",
  "version": 1,
  "fields": [
    {"name": "payment_type", "type": "enum", "values": ["receive", "pay"], "required": true},
    {"name": "party_type", "type": "enum", "values": ["customer", "supplier"], "required": true},
    {"name": "party_id", "type": "string", "required": true},
    {"name": "amount", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "payment_date", "type": "date"},
    {"name": "reference", "type": "string", "max_length": 120},
    {"name": "allocations", "type": "array"},
    {"name": "status", "type": "enum", "values": ["draft", "submitted", "cancelled"], "default": "draft"},
    {"name": "bank_account", "type": "string", "max_length": 32},
    {"name": "ar_account_code", "type": "string", "max_length": 32},
    {"name": "ap_account_code", "type": "string", "max_length": 32},
    {"name": "journal_entry_id", "type": "string"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["payment_date", "payment_type", "party_type", "party_id", "amount", "currency", "status"]},
    "form": {"sections": [
      {"title": "Payment", "fields": ["payment_type", "party_type", "party_id", "payment_date", "reference", "owner"]},
      {"title": "Amount", "fields": ["amount", "currency", "bank_account"]},
      {"title": "Allocations", "fields": ["allocations"]},
      {"title": "Accounts", "fields": ["ar_account_code", "ap_account_code"]},
      {"title": "Posting", "fields": ["status", "journal_entry_id"]}
    ]}
  },
  "cards": {"summary": "Payment {{reference}} — {{amount}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["finance.admin", "tenant.admin"]},
  "workflow": {
    "name": "finance.payment.lifecycle",
    "initial_state": "draft",
    "states": ["draft", "submitted", "cancelled"],
    "transitions": [
      {"from": ["draft"], "to": "submitted", "action": "submit"},
      {"from": ["draft", "submitted"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["finance.record_payment"]
}`)
