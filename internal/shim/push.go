package shim

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// groupingPart renders one name/value pair of a Pushgateway grouping key.
// Values that are empty or contain "/" must use the base64 form.
func groupingPart(name, value string) string {
	if value == "" || strings.Contains(value, "/") {
		return "/" + name + "@base64/" + base64.RawURLEncoding.EncodeToString([]byte(value))
	}
	return "/" + name + "/" + url.PathEscape(value)
}

// PushURL is the Pushgateway endpoint for this job's group.
func PushURL(cfg Config) string {
	u := cfg.PushgatewayURL + "/metrics" + groupingPart("job", cfg.Job)
	if cfg.Instance != "" {
		u += groupingPart("instance", cfg.Instance)
	}
	return u
}

// Push POSTs body to the Pushgateway. POST (not PUT) replaces only the metric
// names present in body, so series omitted on purpose keep their old value.
func Push(ctx context.Context, cfg Config, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, PushURL(cfg), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, Proxy: http.ProxyFromEnvironment}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		first := ""
		if s := bufio.NewScanner(resp.Body); s.Scan() {
			first = strings.TrimSpace(s.Text())
			if len(first) > 200 {
				first = first[:200]
			}
		}
		return fmt.Errorf("pushgateway returned %s: %s", resp.Status, first)
	}
	return nil
}
