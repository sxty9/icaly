package store

import "testing"

// fakeGC is a stand-in for the contax live-membership checker: grpID -> username -> member.
type fakeGC struct{ m map[string]map[string]bool }

func (f fakeGC) ContaxMember(grpID, username string) bool { return f.m[grpID][username] }

func TestGrantsAndAccessLevel(t *testing.T) {
	st := openTest(t)
	cal, err := st.CreateCalendar("alice", "Work", "#f00", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	g1, err := st.AddGrant("alice", cal.ID, "holistic", "team", "Team", "view", false, nil)
	if err != nil {
		t.Fatalf("add holistic: %v", err)
	}
	g2, err := st.AddGrant("alice", cal.ID, "adhoc", "", "Buddies", "edit", false, []string{"bob", "bob"})
	if err != nil {
		t.Fatalf("add adhoc: %v", err)
	}
	if _, err := st.AddGrant("alice", cal.ID, "contax", "grp-1", "Friends", "view", false, nil); err != nil {
		t.Fatalf("add contax: %v", err)
	}

	grants, err := st.ListGrants("alice", cal.ID)
	if err != nil || len(grants) != 3 {
		t.Fatalf("listgrants: err=%v n=%d", err, len(grants))
	}
	for _, g := range grants {
		if g.Kind == "adhoc" && (len(g.Members) != 1 || g.Members[0] != "bob") {
			t.Fatalf("adhoc members wrong: %+v", g.Members)
		}
	}

	gc := fakeGC{m: map[string]map[string]bool{"grp-1": {"erin": true}}}
	// bob is in team (view) AND ad-hoc (edit) → max is edit.
	if lvl := st.AccessLevel("bob", []string{"team"}, false, "alice", cal.ID, nil); lvl != AccessEdit {
		t.Fatalf("bob expected edit, got %q", lvl)
	}
	// carol only in team → view.
	if lvl := st.AccessLevel("carol", []string{"team"}, false, "alice", cal.ID, nil); lvl != AccessView {
		t.Fatalf("carol expected view, got %q", lvl)
	}
	// dave: no group, not ad-hoc, not contax → none.
	if lvl := st.AccessLevel("dave", []string{"other"}, false, "alice", cal.ID, gc); lvl != AccessNone {
		t.Fatalf("dave expected none, got %q", lvl)
	}
	// erin via live contax membership → view; with nil checker the contax grant is inert.
	if lvl := st.AccessLevel("erin", nil, false, "alice", cal.ID, gc); lvl != AccessView {
		t.Fatalf("erin (contax) expected view, got %q", lvl)
	}
	if lvl := st.AccessLevel("erin", nil, false, "alice", cal.ID, nil); lvl != AccessNone {
		t.Fatalf("erin (contax, nil gc) expected none, got %q", lvl)
	}
	// owner and admin always edit.
	if lvl := st.AccessLevel("alice", nil, false, "alice", cal.ID, nil); lvl != AccessEdit {
		t.Fatalf("owner expected edit, got %q", lvl)
	}
	if lvl := st.AccessLevel("root", nil, true, "alice", cal.ID, nil); lvl != AccessEdit {
		t.Fatalf("admin expected edit, got %q", lvl)
	}

	// Bump the team grant view→edit; replace ad-hoc members bob→frank.
	if err := st.SetGrant("alice", cal.ID, g1.ID, "edit", false, nil); err != nil {
		t.Fatalf("setgrant level: %v", err)
	}
	if lvl := st.AccessLevel("carol", []string{"team"}, false, "alice", cal.ID, nil); lvl != AccessEdit {
		t.Fatalf("carol after bump expected edit, got %q", lvl)
	}
	if err := st.SetGrant("alice", cal.ID, g2.ID, "edit", false, []string{"frank"}); err != nil {
		t.Fatalf("setgrant members: %v", err)
	}
	if lvl := st.AccessLevel("bob", nil, false, "alice", cal.ID, nil); lvl != AccessNone {
		t.Fatalf("bob removed from ad-hoc expected none, got %q", lvl)
	}
	if lvl := st.AccessLevel("frank", nil, false, "alice", cal.ID, nil); lvl != AccessEdit {
		t.Fatalf("frank in ad-hoc expected edit, got %q", lvl)
	}

	// CalendarsFor: carol (team edit) sees Alice's shared calendar with the right metadata.
	cals, err := st.CalendarsFor("carol", []string{"team"}, nil)
	if err != nil {
		t.Fatalf("calendarsfor: %v", err)
	}
	var shared *SharedCalendar
	for i := range cals {
		if cals[i].Owner == "alice" && cals[i].ID == cal.ID {
			shared = &cals[i]
		}
	}
	if shared == nil {
		t.Fatalf("carol should see alice's shared calendar; got %+v", cals)
	}
	if shared.SharedBy != "alice" || shared.Level != AccessEdit || shared.ReadOnly || shared.FeedToken != "" {
		t.Fatalf("shared calendar metadata wrong: %+v", shared)
	}

	// Remove the team grant → carol loses access; delete the calendar → grants purged.
	if err := st.RemoveGrant("alice", cal.ID, g1.ID); err != nil {
		t.Fatalf("removegrant: %v", err)
	}
	if lvl := st.AccessLevel("carol", []string{"team"}, false, "alice", cal.ID, nil); lvl != AccessNone {
		t.Fatalf("carol after remove expected none, got %q", lvl)
	}
	if err := st.DeleteCalendar("alice", cal.ID); err != nil {
		t.Fatalf("deletecal: %v", err)
	}
	if g, _ := st.ListGrants("alice", cal.ID); len(g) != 0 {
		t.Fatalf("grants not purged after calendar delete: %d", len(g))
	}
}

func TestGrantExternalConsentTogglesPublic(t *testing.T) {
	st := openTest(t)
	cal, err := st.CreateCalendar("alice", "Fam", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	g, err := st.AddGrant("alice", cal.ID, "contax", "grp-x", "Fam", "view", true, nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if c, ok := st.calendarRow("alice", cal.ID); !ok || !c.Public {
		t.Fatalf("public should be on after external-consent grant: %+v", c)
	}
	if err := st.SetGrant("alice", cal.ID, g.ID, "view", false, nil); err != nil {
		t.Fatalf("setgrant: %v", err)
	}
	if c, _ := st.calendarRow("alice", cal.ID); c.Public {
		t.Fatalf("public should be off after consent withdrawn")
	}
}
