package service

import (
	"github.com/inkframe/inkframe-backend/internal/config"
	"github.com/inkframe/inkframe-backend/internal/logger"
)

// EmailNotificationService sends simple notification emails via the configured
// email backend (SMTP or webhook). It is opt-in via config email.enabled.
// All methods are non-fatal — errors are logged but never propagated to callers.
type EmailNotificationService struct {
	cfg    config.EmailConfig
	sender EmailSender
}

// NewEmailNotificationService creates an EmailNotificationService.
// When cfg.Enabled is false or no SMTP/webhook is configured, all sends are no-ops.
func NewEmailNotificationService(cfg config.EmailConfig) *EmailNotificationService {
	var sender EmailSender
	switch {
	case !cfg.Enabled:
		sender = &NoopEmailSender{}
	case cfg.WebhookURL != "":
		sender = NewWebhookEmailSender(cfg.WebhookURL, cfg.WebhookToken)
	case cfg.Host != "":
		sender = NewSMTPEmailSender(cfg.Host, cfg.Port, cfg.Username, cfg.Password, cfg.From, cfg.UseTLS)
	default:
		sender = &NoopEmailSender{}
	}
	return &EmailNotificationService{cfg: cfg, sender: sender}
}

// SendNotification sends a plain-text notification email asynchronously; non-fatal.
func (s *EmailNotificationService) SendNotification(to, subject, body string) {
	if !s.cfg.Enabled || s.cfg.Host == "" && s.cfg.WebhookURL == "" {
		return
	}
	go func() {
		if err := s.sender.SendEmail(to, subject, body); err != nil {
			logger.Errorf("[EmailNotificationService] SendNotification to=%s: %v", to, err)
		}
	}()
}
