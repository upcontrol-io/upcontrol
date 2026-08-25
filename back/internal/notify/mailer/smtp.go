package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"net/smtp"
)

// SMTP sends through any relay, stdlib only: back/ carries no provider SDK.
type SMTP struct {
	cfg Config
	log *slog.Logger
}

// Send delivers one plain-text message through the relay.
func (s *SMTP) Send(_ context.Context, to, subject, text string) error {
	msg := buildMessage(s.cfg.From, s.cfg.FromName, to, subject, text)
	addr := fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port)
	var auth smtp.Auth
	if s.cfg.Username != "" {
		auth = smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)
	}
	if err := smtp.SendMail(addr, auth, s.cfg.From, []string{to}, msg); err != nil {
		s.log.Warn("mailer: send failed", "to", to, "err", err)
		return err
	}
	return nil
}

// buildMessage assembles the RFC 822 bytes. Pure, so the header contract is
// testable without a relay.
func buildMessage(from, fromName, to, subject, text string) []byte {
	sender := from
	if fromName != "" {
		sender = fmt.Sprintf("%s <%s>", fromName, from)
	}
	return []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n%s", sender, to, subject, text))
}
