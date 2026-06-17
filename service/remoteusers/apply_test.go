package remoteusers

import (
	"errors"
	"testing"
)

type fakeUpdater struct {
	users     []string
	uPSKs     []string
	history   [][]string
	callCount int
	err       error
}

func (f *fakeUpdater) UpdateUsers(users []string, uPSKs []string) error {
	if f.err != nil {
		return f.err
	}
	f.users = append([]string(nil), users...)
	f.uPSKs = append([]string(nil), uPSKs...)
	f.history = append(f.history, append([]string(nil), users...))
	f.callCount++
	return nil
}

func TestApplyUsersPushesToAllTargets(t *testing.T) {
	a, b := &fakeUpdater{}, &fakeUpdater{}
	err := applyUsers([]userUpdater{a, b}, []userEntry{{Name: "alice", Password: "pa"}, {Name: "bob", Password: "pb"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []*fakeUpdater{a, b} {
		if len(f.users) != 2 || f.users[0] != "alice" || f.uPSKs[1] != "pb" {
			t.Fatalf("unexpected push: %v / %v", f.users, f.uPSKs)
		}
	}
}

func TestApplyUsersPropagatesError(t *testing.T) {
	bad := &fakeUpdater{err: errors.New("boom")}
	if err := applyUsers([]userUpdater{bad}, []userEntry{{Name: "x", Password: "y"}}); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestHashUsersOrderIndependentAndContentSensitive(t *testing.T) {
	a := hashUsers([]userEntry{{Name: "alice", Password: "1"}, {Name: "bob", Password: "2"}})
	b := hashUsers([]userEntry{{Name: "bob", Password: "2"}, {Name: "alice", Password: "1"}})
	if a != b {
		t.Fatal("hash must be order-independent")
	}
	c := hashUsers([]userEntry{{Name: "alice", Password: "1"}, {Name: "bob", Password: "3"}})
	if a == c {
		t.Fatal("hash must change when a password changes")
	}
}
