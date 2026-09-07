package remoteusers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	E "github.com/sagernet/sing/common/exceptions"
)

// usageEntry is one user's running totals since the counters were last reset by
// the SoT. Totals, not deltas: a report that fails to arrive is superseded by
// the next one, so nothing is lost and nothing is counted twice.
type usageEntry struct {
	Name          string `json:"name"`
	UplinkBytes   int64  `json:"uplink_bytes"`
	DownlinkBytes int64  `json:"downlink_bytes"`
	TCPSessions   int64  `json:"tcp_sessions"`
	UDPSessions   int64  `json:"udp_sessions"`
}

type usageReport struct {
	Token string       `json:"token"`
	Node  string       `json:"node"`
	Usage []usageEntry `json:"usage"`
}

// reportURL derives the usage endpoint from the user-list URL by replacing the
// last path segment: .../admin/users -> .../admin/usage/report. Deriving it
// keeps one URL in the config; the two endpoints always live together.
func reportURL(usersURL string) string {
	slash := strings.LastIndex(usersURL, "/")
	if slash < 0 {
		return usersURL
	}
	return usersURL[:slash] + "/usage/report"
}

// postUsage sends the running totals for this node. A non-2xx response is an
// error, so the caller keeps the previous reported state and retries next tick.
func postUsage(ctx context.Context, client *http.Client, url, token, node string, usage []usageEntry) error {
	body, err := json.Marshal(usageReport{Token: token, Node: node, Usage: usage})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return E.New("unexpected status: ", response.Status)
	}
	return nil
}

// usageChanged reports whether any total moved since the last successful
// report, so an idle node stops writing to the SoT.
func usageChanged(previous, current []usageEntry) bool {
	if len(previous) != len(current) {
		return true
	}
	last := make(map[string]usageEntry, len(previous))
	for _, u := range previous {
		last[u.Name] = u
	}
	for _, u := range current {
		if last[u.Name] != u {
			return true
		}
	}
	return false
}
