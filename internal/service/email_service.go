package service

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"syscall"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
)

// EmailSender 发送邮件的接口（方便测试时替换）
type EmailSender interface {
	SendEmail(to, subject, body string) error
}

// SMTPEmailSender 通过 SMTP 发送邮件
type SMTPEmailSender struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	UseTLS   bool
}

func NewSMTPEmailSender(host string, port int, username, password, from string, useTLS bool) *SMTPEmailSender {
	return &SMTPEmailSender{Host: host, Port: port, Username: username, Password: password, From: from, UseTLS: useTLS}
}

// isRetryableEmailError 判断是否值得重试：网络不可达/被拒等永久性错误不重试
func isRetryableEmailError(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.ENOEXEC) ||
		errors.Is(err, syscall.EHOSTUNREACH) {
		return false
	}
	return true
}

func (s *SMTPEmailSender) SendEmail(to, subject, body string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
		lastErr = s.sendOnce(to, subject, body)
		if lastErr == nil {
			return nil
		}
		logger.Errorf("SendEmail attempt %d failed: %v", attempt+1, lastErr)
		if !isRetryableEmailError(lastErr) {
			break // 永久性错误，不再重试
		}
	}
	return fmt.Errorf("send email failed: %w", lastErr)
}

func (s *SMTPEmailSender) sendOnce(to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", s.Host, s.Port)
	msg := strings.Join([]string{
		fmt.Sprintf("From: %s", s.From),
		fmt.Sprintf("To: %s", to),
		fmt.Sprintf("Subject: %s", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"",
		body,
	}, "\r\n")

	var auth smtp.Auth
	if s.Username != "" {
		auth = smtp.PlainAuth("", s.Username, s.Password, s.Host)
	}

	if s.UseTLS {
		tlsCfg := &tls.Config{ServerName: s.Host} //nolint:gosec
		conn, err := tls.Dial("tcp", addr, tlsCfg)
		if err != nil {
			return fmt.Errorf("smtp tls dial: %w", err)
		}
		defer conn.Close()
		client, err := smtp.NewClient(conn, s.Host)
		if err != nil {
			return fmt.Errorf("smtp client: %w", err)
		}
		defer client.Close()
		if auth != nil {
			if err := client.Auth(auth); err != nil {
				return fmt.Errorf("smtp auth: %w", err)
			}
		}
		if err := client.Mail(s.From); err != nil {
			return err
		}
		if err := client.Rcpt(to); err != nil {
			return err
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		defer w.Close()
		_, err = fmt.Fprint(w, msg)
		return err
	}
	return smtp.SendMail(addr, auth, s.From, []string{to}, []byte(msg))
}

// WebhookEmailSender 通过 HTTP POST 发送邮件（SMTP 不可用时的替代方案）
// Payload: {"to":"...","subject":"...","body":"..."}
type WebhookEmailSender struct {
	URL   string
	Token string // 非空时附加 Authorization: Bearer <token>
}

func NewWebhookEmailSender(url, token string) *WebhookEmailSender {
	return &WebhookEmailSender{URL: url, Token: token}
}

func (w *WebhookEmailSender) SendEmail(to, subject, body string) error {
	payload := fmt.Sprintf(`{"to":%q,"subject":%q,"body":%q}`, to, subject, body)
	req, err := http.NewRequest(http.MethodPost, w.URL, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("webhook email: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if w.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.Token)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook email: send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook email: server returned %d", resp.StatusCode)
	}
	return nil
}

// NoopEmailSender 用于未配置邮件时记录日志（不报错）
type NoopEmailSender struct{}

func (n *NoopEmailSender) SendEmail(to, subject, body string) error {
	logger.Printf("[Email NOOP] To=%s Subject=%s", to, subject)
	return nil
}
