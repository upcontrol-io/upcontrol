// Package mailer delivers the one message this product sends to a stranger: the
// magic-link code. It sits behind the interface auth.NewMagicLink already takes,
// so the sign-in door does not know or care which transport is configured.
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
	subject = "Your Upcontrol sign-in link"
	body = fmt.Sprintf(`Sign in to Upcontrol:

%s

Or type this code on the sign-in page: %s

The link works once and expires shortly. If you did not ask for it, ignore
this message — nobody can sign in without it.
`, link, code)
	return subject, body
}
