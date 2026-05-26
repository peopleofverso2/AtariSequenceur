// Package mailer sends transactional emails via Brevo's HTTPS API.
//
// The dependency on a third-party service is intentional: SMTP from a
// short-lived Cloud Run instance is unreliable (no static egress IP, no
// reverse DNS, frequent greylisting) and Brevo's HTTPS API needs only
// an API key + a verified sender. When the key is missing the mailer
// degrades to a no-op so the app keeps working in dev / unconfigured.
package mailer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const brevoEndpoint = "https://api.brevo.com/v3/smtp/email"

// Brevo is a minimal client for the Brevo SMTP API.
type Brevo struct {
	apiKey   string
	from     string // expéditeur vérifié dans Brevo
	fromName string
	client   *http.Client
}

// New builds a mailer. If apiKey is empty Configured() returns false
// and Send becomes a no-op that reports ErrNotConfigured.
func New(apiKey, from, fromName string) *Brevo {
	return &Brevo{
		apiKey:   apiKey,
		from:     from,
		fromName: fromName,
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

// ErrNotConfigured is returned by Send when the API key is missing.
var ErrNotConfigured = errors.New("mailer: BREVO_API_KEY not configured")

// Configured reports whether the mailer has a usable API key.
func (b *Brevo) Configured() bool { return b != nil && b.apiKey != "" }

// Send delivers a single email. Body is HTML; the API ignores empty
// optional fields. Returns the Brevo message-id on success.
func (b *Brevo) Send(ctx context.Context, to, subject, htmlBody string) (string, error) {
	if !b.Configured() {
		return "", ErrNotConfigured
	}
	payload := map[string]any{
		"sender":      map[string]string{"email": b.from, "name": b.fromName},
		"to":          []map[string]string{{"email": to}},
		"subject":     subject,
		"htmlContent": htmlBody,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, brevoEndpoint, bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("api-key", b.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("brevo: HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		MessageID string `json:"messageId"`
	}
	_ = json.Unmarshal(body, &out)
	return out.MessageID, nil
}
