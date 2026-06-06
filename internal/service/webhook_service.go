package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/inkframe/inkframe-backend/internal/logger"
	"github.com/inkframe/inkframe-backend/internal/model"
	"gorm.io/gorm"
)

// Webhook event type constants
const (
	WebhookEventChapterGenerated  = "chapter.generated"
	WebhookEventChapterReviewed   = "chapter.reviewed"
	WebhookEventVideoReady        = "video.ready"
	WebhookEventAnalysisComplete  = "analysis.complete"
	WebhookEventNovelCreated      = "novel.created"
)

// WebhookService manages webhook subscriptions and dispatches events.
type WebhookService struct {
	db   *gorm.DB
	http *http.Client
}

// NewWebhookService creates a new WebhookService.
func NewWebhookService(db *gorm.DB) *WebhookService {
	return &WebhookService{
		db:   db,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// CreateSubscription creates a new webhook subscription.
func (s *WebhookService) CreateSubscription(tenantID uint, url string, secret string, events []string) (*model.WebhookSubscription, error) {
	eventsJSON, err := json.Marshal(events)
	if err != nil {
		return nil, fmt.Errorf("marshal events: %w", err)
	}
	sub := &model.WebhookSubscription{
		TenantID: tenantID,
		URL:      url,
		Secret:   secret,
		Events:   string(eventsJSON),
		IsActive: true,
	}
	if err := s.db.Create(sub).Error; err != nil {
		return nil, fmt.Errorf("create webhook subscription: %w", err)
	}
	return sub, nil
}

// ListSubscriptions lists all webhook subscriptions for a tenant.
func (s *WebhookService) ListSubscriptions(tenantID uint) ([]*model.WebhookSubscription, error) {
	var subs []*model.WebhookSubscription
	if err := s.db.Where("tenant_id = ?", tenantID).Find(&subs).Error; err != nil {
		return nil, fmt.Errorf("list webhook subscriptions: %w", err)
	}
	return subs, nil
}

// DeleteSubscription deletes a subscription (must belong to tenant).
func (s *WebhookService) DeleteSubscription(id, tenantID uint) error {
	result := s.db.Where("id = ? AND tenant_id = ?", id, tenantID).Delete(&model.WebhookSubscription{})
	if result.Error != nil {
		return fmt.Errorf("delete webhook subscription: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("subscription not found or access denied")
	}
	return nil
}

// TestWebhook sends a test delivery to the specified subscription.
func (s *WebhookService) TestWebhook(id, tenantID uint) error {
	var sub model.WebhookSubscription
	if err := s.db.Where("id = ? AND tenant_id = ?", id, tenantID).First(&sub).Error; err != nil {
		return fmt.Errorf("webhook not found")
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"event":     "test",
		"tenant_id": tenantID,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"data":      map[string]string{"message": "This is a test delivery"},
	})
	req, err := http.NewRequest("POST", sub.URL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if sub.Secret != "" {
		req.Header.Set("X-Inkframe-Signature", webhookSign(sub.Secret, payload))
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("delivery failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("endpoint returned status %d", resp.StatusCode)
	}
	return nil
}

// Dispatch sends an event to all active subscriptions for the tenant that match the event type.
// Called asynchronously (goroutine) — non-blocking for callers.
func (s *WebhookService) Dispatch(tenantID uint, eventType string, payload interface{}) {
	body, err := json.Marshal(map[string]interface{}{
		"event":      eventType,
		"tenant_id":  tenantID,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
		"data":       payload,
	})
	if err != nil {
		logger.Errorf("[WebhookService] Dispatch marshal error: %v", err)
		return
	}

	var subs []*model.WebhookSubscription
	if err := s.db.Where("tenant_id = ? AND is_active = ?", tenantID, true).Find(&subs).Error; err != nil {
		logger.Errorf("[WebhookService] Dispatch query error: %v", err)
		return
	}

	for _, sub := range subs {
		// Check if subscription listens to this event type
		var events []string
		if err := json.Unmarshal([]byte(sub.Events), &events); err != nil {
			continue
		}
		matched := false
		for _, e := range events {
			if e == eventType {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		go s.deliverWithRetry(sub, eventType, body)
	}
}

// deliverWithRetry delivers to one subscription with up to 3 retries (1s, 5s, 30s backoff).
// Signs payload with HMAC-SHA256 using subscription.Secret if non-empty.
// Signature header: X-Inkframe-Signature: sha256=<hex>
func (s *WebhookService) deliverWithRetry(sub *model.WebhookSubscription, eventType string, body []byte) {
	backoffs := []time.Duration{1 * time.Second, 5 * time.Second, 30 * time.Second}
	var lastErr string
	var lastCode int

	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			time.Sleep(backoffs[attempt-2])
		}

		code, respBody, err := s.deliver(sub, body)
		delivery := &model.WebhookDelivery{
			SubscriptionID: sub.ID,
			EventType:      eventType,
			Payload:        string(body),
			StatusCode:     code,
			ResponseBody:   respBody,
			Attempt:        attempt,
			Success:        err == nil && code >= 200 && code < 300,
		}
		s.db.Create(delivery)

		if err == nil && code >= 200 && code < 300 {
			// Success: reset fail count
			s.db.Model(sub).Updates(map[string]interface{}{
				"fail_count": 0,
				"last_error": "",
			})
			return
		}

		if err != nil {
			lastErr = err.Error()
		} else {
			lastErr = fmt.Sprintf("HTTP %d: %s", code, respBody)
		}
		lastCode = code
		logger.Errorf("[WebhookService] delivery attempt %d failed for subscription %d: %s", attempt, sub.ID, lastErr)
	}

	// All 3 attempts failed
	_ = lastCode
	s.db.Model(sub).Updates(map[string]interface{}{
		"fail_count": gorm.Expr("fail_count + 1"),
		"last_error": lastErr,
	})

	// Disable after 3 consecutive delivery failures
	var updated model.WebhookSubscription
	s.db.First(&updated, sub.ID)
	if updated.FailCount >= 3 {
		s.db.Model(sub).Update("is_active", false)
		logger.Errorf("[WebhookService] subscription %d disabled after %d consecutive failures", sub.ID, updated.FailCount)
	}
}

// deliver performs a single HTTP POST delivery attempt.
func (s *WebhookService) deliver(sub *model.WebhookSubscription, body []byte) (statusCode int, respBody string, err error) {
	req, err := http.NewRequest(http.MethodPost, sub.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Inkframe-Webhook/1.0")
	if sub.Secret != "" {
		req.Header.Set("X-Inkframe-Signature", webhookSign(sub.Secret, body))
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(rb), nil
}

// webhookSign signs the body with HMAC-SHA256 using the given secret.
func webhookSign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
