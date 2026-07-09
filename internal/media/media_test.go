package media

import "testing"

func TestParseName(t *testing.T) {
	cases := []struct {
		in      string
		episode bool
		show    string
		s, e    int
		title   string
		year    int
	}{
		{"Static Signal S01E03 Cold Boot", true, "Static Signal", 1, 3, "Cold Boot", 0},
		{"the.expanse.s02e11.1080p.web-dl", true, "the expanse", 2, 11, "", 0},
		{"Severance - S01E09 - The We We Are", true, "Severance", 1, 9, "The We We Are", 0},
		{"Show (2019) S03E01", true, "Show", 3, 1, "", 2019},
		{"Blade Circuit (2023)", false, "", 0, 0, "Blade Circuit", 2023},
		{"Heat.1995.2160p.Remux", false, "", 0, 0, "Heat", 1995},
		{"Some Movie", false, "", 0, 0, "Some Movie", 0},
	}
	for _, c := range cases {
		isEp, show, s, e, title, year := ParseName(c.in)
		if isEp != c.episode || show != c.show || s != c.s || e != c.e || title != c.title || year != c.year {
			t.Errorf("%q => ep=%v show=%q S%dE%d title=%q year=%d; want ep=%v show=%q S%dE%d title=%q year=%d",
				c.in, isEp, show, s, e, title, year, c.episode, c.show, c.s, c.e, c.title, c.year)
		}
	}
}
