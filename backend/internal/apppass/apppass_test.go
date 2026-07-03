package apppass

import "testing"

func TestAppPasswordLifecycle(t *testing.T) {
	s := New(t.TempDir())

	token, meta, err := s.Create("alice", "Thunderbird")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(token) < 32 {
		t.Errorf("token too short: %q", token)
	}
	if meta.Label != "Thunderbird" {
		t.Errorf("label = %q", meta.Label)
	}

	if !s.Verify("alice", token) {
		t.Errorf("valid token did not verify")
	}
	if s.Verify("alice", "wrong-token") {
		t.Errorf("wrong token verified")
	}
	if s.Verify("bob", token) {
		t.Errorf("token verified for the wrong user")
	}

	list, _ := s.List("alice")
	if len(list) != 1 || list[0].ID != meta.ID {
		t.Fatalf("list = %v", list)
	}

	if err := s.Delete("alice", meta.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if s.Verify("alice", token) {
		t.Errorf("token still valid after delete")
	}
	if list, _ := s.List("alice"); len(list) != 0 {
		t.Errorf("list not empty after delete: %v", list)
	}
}

func TestMultipleTokens(t *testing.T) {
	s := New(t.TempDir())
	t1, _, _ := s.Create("alice", "a")
	t2, _, _ := s.Create("alice", "b")
	if !s.Verify("alice", t1) || !s.Verify("alice", t2) {
		t.Errorf("both tokens should verify")
	}
	if list, _ := s.List("alice"); len(list) != 2 {
		t.Errorf("want 2 tokens, got %d", len(list))
	}
}
