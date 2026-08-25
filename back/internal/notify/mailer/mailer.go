// Package mailer delivers the messages this product sends to a stranger: the
// magic-link code and the project invitation. It sits behind the interface
// auth.NewMagicLink already takes, so the sign-in door does not know or care
// which transport is configured.
package mailer

import (
	"fmt"
	"net/url"
)

// Config is the whole surface. A half-filled one is refused at construction:
// a mailer that accepts mail and drops it is worse than no mailer, because the
// operator has no way to tell the difference.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
}

// RenderCode builds the message. Pure, so the link format is testable without a
// network — and the format matters: SignIn redeems `?email=…&token=…` on mount.
//
// Both forms travel: the link for the common case, the bare code for the mail
// client that mangles it. A message with only a link has one way to fail.
func RenderCode(to, code, signInBase string) (subject, body string) {
	link := fmt.Sprintf("%s/sign-in?email=%s&token=%s",
		signInBase, url.QueryEscape(to), url.QueryEscape(code))
	subject = "Your UpControl sign-in link"
	body = fmt.Sprintf(`Sign in to UpControl:

%s

Or type this code on the sign-in page: %s

The link works once and expires shortly. If you did not ask for it, ignore
this message — nobody can sign in without it.
`, link, code)
	return subject, body
}

// RenderInvite builds the invitation message, the same shape RenderCode gives
// the sign-in door: link plus bare code, so a mail client that mangles the link
// still leaves a way in. Pure for the same reason, and the bytes are a
// contract: the email agent (email/src/templates.ts) pins the same text part
// in TypeScript, so a line that drifts here is a second invitation mail in the
// wild.
func RenderInvite(to, code, signInBase, project, invitedBy string) (subject, body string) {
	link := fmt.Sprintf("%s/sign-in?email=%s&token=%s",
		signInBase, url.QueryEscape(to), url.QueryEscape(code))
	subject = fmt.Sprintf("%s invited you to %s on UpControl", invitedBy, project)
	body = fmt.Sprintf(`%s invited you to %s on UpControl.

Accept the invitation and sign in:

%s

Or type this code on the sign-in page: %s

The link works once and expires shortly. If you were not expecting this, ignore
this message: nobody can sign in without it.
`, invitedBy, project, link, code)
	return subject, body
}
