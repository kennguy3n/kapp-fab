// Session 16 — Recruitment email templates.
//
// These helpers build the `notification` envelope that the worker's
// notificationRouter (services/worker/notifications.go) extracts from an
// event payload and dispatches over SMTP. Emitting the envelope on the
// outbox event keeps email delivery fully decoupled from the request
// path: the store commits the row + event in one transaction, and the
// worker turns the envelope into an email best-effort. A missing
// applicant email yields a nil envelope (no email channel), which the
// router simply ignores.

package hr

import "fmt"

// emailEnvelope is the notification shape the worker understands for the
// email channel. Returned as a map so it slots directly into the event
// payload under the "notification" key.
func emailEnvelope(to, title, body string) map[string]any {
	if to == "" {
		return nil
	}
	return map[string]any{
		"channel": "email",
		"email":   to,
		"title":   title,
		"body":    body,
	}
}

// applicationReceivedEmail acknowledges a freshly submitted application.
func applicationReceivedEmail(app JobApplication, opening JobOpening) map[string]any {
	title := fmt.Sprintf("Application received: %s", opening.Title)
	body := fmt.Sprintf(
		"Hi %s,\n\nThank you for applying for the %s position%s. "+
			"We have received your application and our team will review it shortly.\n\n"+
			"Best regards,\nThe Hiring Team",
		app.ApplicantName, opening.Title, departmentSuffix(opening.Department))
	return emailEnvelope(app.ApplicantEmail, title, body)
}

// interviewScheduledEmail notifies the applicant of a scheduled interview.
func interviewScheduledEmail(iv Interview, app JobApplication) map[string]any {
	when := "soon"
	if iv.ScheduledAt != nil {
		when = "on " + iv.ScheduledAt.Format("Mon, 02 Jan 2006 15:04 MST")
	}
	where := ""
	switch {
	case iv.MeetingLink != "":
		where = fmt.Sprintf("\n\nJoin link: %s", iv.MeetingLink)
	case iv.Location != "":
		where = fmt.Sprintf("\n\nLocation: %s", iv.Location)
	}
	title := "Interview scheduled"
	body := fmt.Sprintf(
		"Hi %s,\n\nYour %s interview has been scheduled %s (%d minutes).%s\n\n"+
			"Best regards,\nThe Hiring Team",
		app.ApplicantName, humanInterviewType(iv.InterviewType), when, iv.DurationMinutes, where)
	return emailEnvelope(app.ApplicantEmail, title, body)
}

// offerSentEmail notifies the applicant that an offer has been extended.
func offerSentEmail(offer OfferLetter, app JobApplication) map[string]any {
	role := offer.Designation
	if role == "" {
		role = "the role"
	}
	validity := ""
	if offer.ValidUntil != nil {
		validity = fmt.Sprintf(" This offer is valid until %s.", offer.ValidUntil.Format("02 Jan 2006"))
	}
	title := "Your offer letter"
	body := fmt.Sprintf(
		"Hi %s,\n\nWe are pleased to extend an offer for %s.%s "+
			"Please review the attached offer and respond at your earliest convenience.\n\n"+
			"Best regards,\nThe Hiring Team",
		app.ApplicantName, role, validity)
	return emailEnvelope(app.ApplicantEmail, title, body)
}

// offerAcceptedEmail confirms to the candidate that their acceptance was
// recorded.
func offerAcceptedEmail(offer OfferLetter, app JobApplication) map[string]any {
	title := "Offer accepted — welcome aboard!"
	body := fmt.Sprintf(
		"Hi %s,\n\nThank you for accepting our offer! We are thrilled to have you join us. "+
			"Our HR team will be in touch shortly with onboarding details.\n\n"+
			"Best regards,\nThe Hiring Team",
		app.ApplicantName)
	_ = offer
	return emailEnvelope(app.ApplicantEmail, title, body)
}

func departmentSuffix(dept string) string {
	if dept == "" {
		return ""
	}
	return " in " + dept
}

func humanInterviewType(t string) string {
	switch t {
	case "in_person":
		return "in-person"
	case "":
		return "scheduled"
	default:
		return t
	}
}
