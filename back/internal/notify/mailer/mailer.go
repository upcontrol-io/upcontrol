// Package mailer delivers the magic-link code and the project invitation
// behind the interface auth.NewMagicLink takes.
package mailer

import (
	"fmt"
	"net/url"
)

// Config is the whole surface; a half-filled one is refused at construction:
// a mailer that accepts and drops mail is worse than no mailer.
type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	FromName string
}

// renderCode builds the message, pure so the link format is testable. Both
// forms travel: the link, and the bare code for the client that mangles it.
func renderCode(to, code, signInBase string) (subject, body string) {
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

// renderInvite builds the invitation, same shape as renderCode. The bytes
// are a contract: email/src/templates.ts pins the same text part.
func renderInvite(to, code, signInBase, project, invitedBy string) (subject, body string) {
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
