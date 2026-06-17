package remoteusers

import (
	"path/filepath"
	"testing"
)

func TestCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "users.json")
	in := &cachedUsers{Users: []userEntry{{Name: "alice", Password: "pa"}}, Etag: `"v1"`}
	if err := saveCache(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || len(out.Users) != 1 || out.Users[0].Name != "alice" || out.Etag != `"v1"` {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
}

func TestLoadCacheMissingFileReturnsNil(t *testing.T) {
	out, err := loadCache(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Fatalf("expected nil for missing file, got %+v", out)
	}
}

func TestCacheEmptyPathIsNoop(t *testing.T) {
	if err := saveCache("", &cachedUsers{}); err != nil {
		t.Fatal(err)
	}
	out, err := loadCache("")
	if err != nil || out != nil {
		t.Fatalf("empty path should be a no-op; got %+v, %v", out, err)
	}
}
