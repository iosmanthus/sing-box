package remoteusers

import (
	"encoding/json"
	"os"
	"time"
)

type cachedUsers struct {
	Users       []userEntry `json:"users"`
	Etag        string      `json:"etag,omitempty"`
	LastUpdated time.Time   `json:"last_updated"`
}

// loadCache reads the cached user list. A missing file or empty path returns
// (nil, nil) — there is simply no floor yet.
func loadCache(path string) (*cachedUsers, error) {
	if path == "" {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cache cachedUsers
	if err = json.Unmarshal(content, &cache); err != nil {
		return nil, err
	}
	return &cache, nil
}

// saveCache atomically writes the cached user list (temp file + rename). An
// empty path is a no-op.
func saveCache(path string, cache *cachedUsers) error {
	if path == "" {
		return nil
	}
	content, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
