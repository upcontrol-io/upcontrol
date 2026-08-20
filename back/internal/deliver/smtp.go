// SMTPChannel is the self-host alert-email path: no email agent, the alert
// leaves through the deployment's own SMTP relay (UC_SMTP_*) — the same relay
// the magic-link mailer uses. ucworker registers it only when UC_EMAIL_URL is
// unset and UC_SMTP_HOST is set; with the agent present, EmailChannel wins
// (docs/plans/public-first-split.md, Decision 9).

package deliver

import "context"

// mailSender is the one method SMTPChannel needs from mailer.SMTP; an
// interface so the test can capture the message instead of running a relay.
type mailSender interface {
	Send(ctx context.Context, to, subject, text string) error
}

// SMTPChannel sends alert emails over SMTP directly.
type SMTPChannel struct{ Mailer mailSender }

func (c *SMTPChannel) Kind() string { return "email" }

func (c *SMTPChannel) Send(ctx context.Context, target string, p AlertPayload) (int, error) {
	// SMTP has no HTTP status; 200 keeps ClassifyError's success contract, and
	// a transport failure is the (0, err) retryable case, same as doPost.
	if err := c.Mailer.Send(ctx, target, "["+p.Status+"] "+p.Title, formatEmail(p)); err != nil {
		return 0, err
	}
	return 200, nil
}
