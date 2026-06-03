package billing

import (
	"encoding/json"
	"time"
)

// stripeSubscriptionObject is the subset of a Stripe Subscription we
// read from customer.subscription.* webhook payloads. Unknown fields
// are ignored so Stripe can extend the object freely.
type stripeSubscriptionObject struct {
	ID                string `json:"id"`
	Customer          string `json:"customer"`
	Status            string `json:"status"`
	CancelAtPeriodEnd bool   `json:"cancel_at_period_end"`
	CurrentPeriodEnd  int64  `json:"current_period_end"`
	TrialEnd          int64  `json:"trial_end"`
	Items             struct {
		Data []struct {
			ID    string `json:"id"`
			Price struct {
				ID string `json:"id"`
			} `json:"price"`
		} `json:"data"`
	} `json:"items"`
	Metadata map[string]string `json:"metadata"`
}

// firstItem returns the (subscriptionItemID, priceID) of the first
// line item, or ("","") when the subscription carries none.
func (o stripeSubscriptionObject) firstItem() (itemID, priceID string) {
	if len(o.Items.Data) == 0 {
		return "", ""
	}
	return o.Items.Data[0].ID, o.Items.Data[0].Price.ID
}

// stripeInvoiceObject is the subset of a Stripe Invoice we read from
// invoice.* webhook payloads.
type stripeInvoiceObject struct {
	ID               string            `json:"id"`
	Customer         string            `json:"customer"`
	Subscription     string            `json:"subscription"`
	Status           string            `json:"status"`
	AmountDue        int64             `json:"amount_due"`
	AmountPaid       int64             `json:"amount_paid"`
	Currency         string            `json:"currency"`
	HostedInvoiceURL string            `json:"hosted_invoice_url"`
	PeriodStart      int64             `json:"period_start"`
	PeriodEnd        int64             `json:"period_end"`
	Metadata         map[string]string `json:"metadata"`
}

func parseSubscriptionObject(raw json.RawMessage) (stripeSubscriptionObject, error) {
	var o stripeSubscriptionObject
	err := json.Unmarshal(raw, &o)
	return o, err
}

func parseInvoiceObject(raw json.RawMessage) (stripeInvoiceObject, error) {
	var o stripeInvoiceObject
	err := json.Unmarshal(raw, &o)
	return o, err
}

// epochPtr converts a Unix-seconds timestamp into a *time.Time, or
// nil when the value is zero (Stripe omits/zeroes optional time
// fields such as trial_end on a non-trial subscription).
func epochPtr(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0).UTC()
	return &t
}
