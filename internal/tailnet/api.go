package tailnet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultAPIBase is Tailscale's API address.
const DefaultAPIBase = "https://api.tailscale.com"

// DeleteDevice removes a device from the tailnet with an API access token
// (tskey-api-…). Logging a node out only switches it off; Tailscale keeps
// it in the Machines list until it is deleted like this or by hand.
func DeleteDevice(ctx context.Context, base, apiKey, nodeID string) error {
	if base == "" {
		base = DefaultAPIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		strings.TrimRight(base, "/")+"/api/v2/device/"+url.PathEscape(nodeID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale api: %w", err)
	}
	defer resp.Body.Close()
	// 404: already gone, which is what we want.
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	return fmt.Errorf("tailscale api answered %s: %s", resp.Status, strings.TrimSpace(string(msg)))
}
