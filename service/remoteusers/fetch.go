package remoteusers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	E "github.com/sagernet/sing/common/exceptions"
)

// userUpdater is the narrow capability the service needs from a managed
// inbound. adapter.ManagedSSMServer satisfies it; tests use a fake.
type userUpdater interface {
	UpdateUsers(users []string, uPSKs []string) error
}

type userEntry struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

type usersResponse struct {
	Users []userEntry `json:"users"`
}

type fetchResult struct {
	users       []userEntry
	etag        string
	notModified bool
}

// fetchUsers POSTs {"token":...} to url and parses {"users":[{name,password}]}.
// When etag is non-empty it is sent as If-None-Match; a 304 yields notModified.
func fetchUsers(ctx context.Context, client *http.Client, url, token, etag string) (fetchResult, error) {
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		return fetchResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fetchResult{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if etag != "" {
		request.Header.Set("If-None-Match", etag)
	}
	response, err := client.Do(request)
	if err != nil {
		return fetchResult{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return fetchResult{notModified: true}, nil
	default:
		return fetchResult{}, E.New("unexpected status: ", response.Status)
	}
	content, err := io.ReadAll(response.Body)
	if err != nil {
		return fetchResult{}, err
	}
	var parsed usersResponse
	if err = json.Unmarshal(content, &parsed); err != nil {
		return fetchResult{}, E.Cause(err, "parse user list")
	}
	return fetchResult{users: parsed.Users, etag: response.Header.Get("Etag")}, nil
}

// hashUsers returns a stable, order-independent hash of the user set.
func hashUsers(users []userEntry) [32]byte {
	sorted := make([]userEntry, len(users))
	copy(sorted, users)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	h := sha256.New()
	for _, u := range sorted {
		h.Write([]byte(u.Name))
		h.Write([]byte{0})
		h.Write([]byte(u.Password))
		h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// applyUsers pushes the full user set to every target via UpdateUsers. It is
// best-effort across multiple targets: a failure mid-list leaves earlier targets
// already updated and returns the error without touching the rest. The caller
// does not commit any state on error, so the next poll re-applies the full set
// to every target and self-heals the partial update.
func applyUsers(targets []userUpdater, users []userEntry) error {
	names := make([]string, len(users))
	passwords := make([]string, len(users))
	for i, u := range users {
		names[i] = u.Name
		passwords[i] = u.Password
	}
	for _, target := range targets {
		if err := target.UpdateUsers(names, passwords); err != nil {
			return err
		}
	}
	return nil
}
