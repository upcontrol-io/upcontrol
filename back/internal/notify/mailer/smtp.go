package mailer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/smtp"
	"strings"
)

// SMTP sends through any relay. stdlib only: this product's rule is zero runtime
// dependencies it does not need, and a provider SDK would tie back/ to a vendor
// for one message.
type SMTP struct {
	cfg  Config
	log  *slog.Logger
	base string
}

// NewSMTP refuses an incomplete config rather than failing per-message later.
func NewSMTP(cfg Config, log *slog.Logger) (*SMTP, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, errors.New("mailer: UC_SMTP_HOST is empty")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return nil, errors.New("mailer: UC_SMTP_FROM is empty")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &SMTP{cfg: cfg, log: log}, nil
}

// WithSignInBase sets the origin the magic link points at (e.g. https://upcontrol.io).
func (s *SMTP) WithSignInBase(base string) *SMTP { s.base = base; return s }

// SendCode delivers one magic-link code. The code itself is never logged: a log
// line carrying it is a second place to steal a session from.
func (s *SMTP) SendCode(ctx context.Context, to, code string) error {
	subject, body := RenderCode(to, code, s.base)
	if err := s.Send(ctx, to, subject, body); err != nil {
		return err
	}
	s.log.Info("mailer: code sent", "to", to)
	return nil
}

// Send delivers one plain-text message through the relay. SendCode rides it;
// the alert channel (deliver.SMTPChannel) calls it directly.
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
