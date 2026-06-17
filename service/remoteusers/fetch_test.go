package remoteusers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchUsersParsesList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Etag", `"v1"`)
		_, _ = w.Write([]byte(`{"users":[{"name":"alice","password":"cEFzcw=="},{"name":"bob","password":"cEJzcw=="}]}`))
	}))
	defer server.Close()

	result, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.users) != 2 || result.users[0].Name != "alice" || result.users[1].Password != "cEJzcw==" {
		t.Fatalf("unexpected users: %+v", result.users)
	}
	if result.etag != `"v1"` {
		t.Fatalf("unexpected etag: %q", result.etag)
	}
}

func TestFetchUsersNotModified(t *testing.T) {
	var sentEtag string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sentEtag = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	result, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", `"v1"`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.notModified {
		t.Fatal("expected notModified=true")
	}
	if sentEtag != `"v1"` {
		t.Fatalf("If-None-Match not sent; got %q", sentEtag)
	}
}

func TestFetchUsersErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", ""); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}

func TestFetchUsersSendsToken(t *testing.T) {
	var gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "secret", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, `"secret"`) {
		t.Fatalf("token not in request body: %s", gotBody)
	}
}

func TestFetchUsersUsesPostMethod(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", ""); err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("expected POST, got %q", gotMethod)
	}
}

func TestFetchUsersMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer server.Close()

	if _, err := fetchUsers(context.Background(), server.Client(), server.URL, "tok", ""); err == nil {
		t.Fatal("expected error on malformed JSON body")
	}
}
