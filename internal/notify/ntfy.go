// Package notify sends alerts to the phone through ntfy.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message is one alert.
type Message struct {
	Title    string
	Body     string
	Priority int      // 1 (min) to 5 (max); 0 = server default
	Tags     []string // ntfy tags, shown as emoji
	Click    string   // address opened when the alert is tapped
}

// Ntfy publishes to an ntfy server (ntfy.sh or self-hosted).
type Ntfy struct {
	URL    string // e.g. https://ntfy.sh
	Topic  string
	Token  string // optional access token
	Client *http.Client
}

// Send publishes one message. It uses ntfy's JSON form so titles can
// contain any characters (å, ä, ö).
func (n Ntfy) Send(ctx context.Context, m Message) error {
	if n.Topic == "" {
		return fmt.Errorf("ntfy: no topic set")
	}
	body := map[string]any{"topic": n.Topic, "message": m.Body}
	if m.Title != "" {
		body["title"] = m.Title
	}
	if m.Priority > 0 {
		body["priority"] = m.Priority
	}
	if len(m.Tags) > 0 {
		body["tags"] = m.Tags
	}
	if m.Click != "" {
		body["click"] = m.Click
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.URL, "/")+"/", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	c := n.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("ntfy answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
