package mailer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

// Dynamic resolves the relay per message: Settings values win over the UC_SMTP_* env, so mail
// flows the moment they are saved; a missing piece is a per-send error, never a boot refusal.
type Dynamic struct {
	settings func(context.Context) Config
	log      *slog.Logger
	base     string
}

func NewDynamic(settings func(context.Context) Config, log *slog.Logger) *Dynamic {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Dynamic{settings: settings, log: log}
}

// WithSignInBase sets the origin the magic link points at.
func (d *Dynamic) WithSignInBase(base string) *Dynamic { d.base = base; return d }

// Configured reports whether a relay exists right now. The auth handler reads
// it through an optional interface assertion (the ai.Configured idiom).
func (d *Dynamic) Configured(ctx context.Context) bool {
	return strings.TrimSpace(d.settings(ctx).Host) != ""
}

// SendCode delivers one magic-link code; the code itself is never logged.
func (d *Dynamic) SendCode(ctx context.Context, to, code string) error {
	subject, body := RenderCode(to, code, d.base)
	if err := d.Send(ctx, to, subject, body); err != nil {
		return err
	}
	d.log.Info("mailer: code sent", "to", to)
	return nil
}

// SendInvite delivers one project invitation; the code is never logged either.
func (d *Dynamic) SendInvite(ctx context.Context, to, code, project, invitedBy string) error {
	subject, body := RenderInvite(to, code, d.base, project, invitedBy)
	if err := d.Send(ctx, to, subject, body); err != nil {
		return err
	}
	d.log.Info("mailer: invite sent", "to", to)
	return nil
}

// Send resolves the relay and delivers one message through it.
func (d *Dynamic) Send(ctx context.Context, to, subject, text string) error {
	cfg := d.settings(ctx)
	if strings.TrimSpace(cfg.Host) == "" {
		return errors.New("smtp: no relay configured (Settings, or UC_SMTP_HOST)")
	}
	if strings.TrimSpace(cfg.From) == "" {
		return errors.New("smtp: no From address configured (Settings, or UC_SMTP_FROM)")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	return (&SMTP{cfg: cfg, log: d.log}).Send(ctx, to, subject, text)
}
