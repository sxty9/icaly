package event

import "testing"

func TestNormAddr(t *testing.T) {
	cases := map[string]string{
		"mailto:Alice@Example.com": "alice@example.com",
		" mailto:Bob@Example.COM ": "bob@example.com", // leading space before the scheme
		"Carol@Example.com":        "carol@example.com",
		"  spaced@x.test  ":        "spaced@x.test",
		"":                         "",
		"   ":                      "",
	}
	for in, want := range cases {
		if got := NormAddr(in); got != want {
			t.Errorf("NormAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameAddr(t *testing.T) {
	same := [][2]string{
		{"mailto:a@b.test", "A@B.test"},
		{" a@b.test ", "a@b.test"},
		{"mailto:User@Host.test", " user@host.test "},
	}
	for _, c := range same {
		if !SameAddr(c[0], c[1]) {
			t.Errorf("SameAddr(%q, %q) = false, want true", c[0], c[1])
		}
	}

	diff := [][2]string{
		{"a@b.test", "c@b.test"},
		{"", ""},         // empty matches nothing, not even itself
		{"", "a@b.test"}, // empty vs present
		{"a@b.test", ""}, // present vs empty
	}
	for _, c := range diff {
		if SameAddr(c[0], c[1]) {
			t.Errorf("SameAddr(%q, %q) = true, want false", c[0], c[1])
		}
	}
}
