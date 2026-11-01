package changelog

import (
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/firmware"
)

const sample = `# Changelog

All notable changes. Preamble text that is not a release.

## Unreleased

### Added
- New thing with ` + "`code`" + ` and **bold**,
  continued on the next line.

## 0.10.1 (2026-10-22)

### Fixed
- Parent item:
  - first sub item
    wrapped.
  - second sub item
- <script>alert(1)</script> and [a link](javascript:alert(1)).

## 0.1.0

From this release on we follow SemVer.

### Added
- Something.

## 0.0.9

- Item without a section.
`

func plain(in []firmware.Inline) string {
	s := ""
	for _, x := range in {
		s += x.Text
	}
	return s
}

// The page lists every release with its date and sections; the preamble is not a release.
func TestParseStructure(t *testing.T) {
	rels := Parse(sample)
	want := []struct{ v, d string }{{"Unreleased", ""}, {"0.10.1", "2026-10-22"}, {"0.1.0", ""}, {"0.0.9", ""}}
	if len(rels) != len(want) {
		t.Fatalf("got %d releases, want %d: %+v", len(rels), len(want), rels)
	}
	for i, w := range want {
		if rels[i].Version != w.v || rels[i].Date != w.d {
			t.Errorf("release %d = %q (%q), want %q (%q)", i, rels[i].Version, rels[i].Date, w.v, w.d)
		}
	}
	add := rels[0].Sections[0]
	if add.Title != "Added" || len(add.Items) != 1 {
		t.Fatalf("Unreleased sections = %+v", rels[0].Sections)
	}
	// A wrapped item is one item; code and bold keep their kind.
	if got := plain(add.Items[0].Text); got != "New thing with code and bold, continued on the next line." {
		t.Errorf("item text = %q", got)
	}
	var code, strong bool
	for _, x := range add.Items[0].Text {
		code = code || (x.Code && x.Text == "code")
		strong = strong || (x.Strong && x.Text == "bold")
	}
	if !code || !strong {
		t.Errorf("inline kinds lost: %+v", add.Items[0].Text)
	}
	fixed := rels[1].Sections[0].Items
	if len(fixed) != 2 || len(fixed[0].Sub) != 2 {
		t.Fatalf("nested items = %+v", fixed)
	}
	if got := plain(fixed[0].Sub[0]); got != "first sub item wrapped." {
		t.Errorf("sub item = %q", got)
	}
	if got := plain(rels[2].Intro[0]); got != "From this release on we follow SemVer." {
		t.Errorf("intro = %q", got)
	}
	if s := rels[3].Sections; len(s) != 1 || s[0].Title != "" || len(s[0].Items) != 1 {
		t.Errorf("items without a section = %+v", s)
	}
}

// The changelog is rendered into a page: markup stays text and unsafe links lose their target.
func TestParseKeepsMarkupInert(t *testing.T) {
	item := Parse(sample)[1].Sections[0].Items[1].Text
	for _, x := range item {
		if x.Href != "" {
			t.Errorf("javascript link kept its target: %+v", x)
		}
	}
	if got := plain(item); got != "<script>alert(1)</script> and a link." {
		t.Errorf("text = %q", got)
	}
}

// The binary's own CHANGELOG.md must parse into releases in descending order: a heading typo
// would silently drop a release from What's new or miscount "N new".
func TestEmbeddedChangelog(t *testing.T) {
	rels := Embedded()
	if len(rels) < 10 {
		t.Fatalf("embedded changelog has %d releases", len(rels))
	}
	prev := ""
	for _, r := range rels {
		if r.Version == Unreleased {
			continue
		}
		if _, ok := parse(r.Version); !ok {
			t.Errorf("release heading %q is not x.y.z", r.Version)
		}
		if prev != "" && Compare(r.Version, prev) >= 0 {
			t.Errorf("release %s follows %s: not newest first", r.Version, prev)
		}
		if len(r.Sections) == 0 {
			t.Errorf("release %s has no items", r.Version)
		}
		prev = r.Version
	}
}

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.10.0", "0.9.0", 1}, // numeric, not lexical
		{"0.9.0", "0.10.0", -1},
		{"1.0.0", "1.0.0", 0},
		{"0.0.2", "0.0.10", -1},
		{"Unreleased", "0.0.1", -1},
		{"0.0.1", "dev", 1},
		{"dev", "", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// "N new since your last visit": releases newer than the stored one; Unreleased never counts.
func TestNewSince(t *testing.T) {
	rels := Parse(sample)
	cases := map[string]int{
		"0.10.1": 0, // up to date
		"0.1.0":  1,
		"0.0.9":  2,
		"0.0.1":  3,
		"":       1, // never opened: one hint for the newest release, not the whole history
		"bogus":  1,
	}
	for seen, want := range cases {
		if got := NewSince(rels, seen); got != want {
			t.Errorf("NewSince(%q) = %d, want %d", seen, got, want)
		}
	}
	if got := NewSince(Parse("## Unreleased\n- x\n"), ""); got != 0 {
		t.Errorf("only Unreleased: NewSince = %d, want 0", got)
	}
	if Latest(rels) != "0.10.1" {
		t.Errorf("Latest = %q", Latest(rels))
	}
	if !Known(rels, "0.1.0") || Known(rels, "9.9.9") {
		t.Error("Known")
	}
}

// Why: a build that is not a release tag must still say which release it is based on, so a bug
// report with "edge" or "dev" can be placed. The base is the newest released heading of the
// embedded changelog (Unreleased never counts); builds that already carry a base stay as they are.
func TestDisplayVersion(t *testing.T) {
	rels := Parse(sample) // newest release heading: 0.10.1, plus Unreleased
	cases := []struct{ name, raw, want string }{
		{"release tag build, nothing to add", "v0.15.2", "v0.15.2"},
		{"image stamped from a tag has no v", "0.10.1", "0.10.1"},
		// #436: GitHub shows 7 characters, so the UI and the log do too; the image tag keeps its 12
		{"CI edge image: base plus the 7-character commit", "edge-0123456789ab", "0.10.1+edge (0123456)"},
		{"CI pull request image", "pr-0123456789ab", "0.10.1+pr (0123456)"},
		{"a hash that is already short stays as it is", "edge-0123456", "0.10.1+edge (0123456)"},
		{"git describe between releases already has base and hash", "v0.10.1-3-gabc1234", "v0.10.1-3-gabc1234"},
		{"git describe of a dirty tree", "v0.10.1-3-gabc1234-dirty", "v0.10.1-3-gabc1234-dirty"},
		{"plain docker build has no git history", "dev", "0.10.1+dev"},
		{"unknown stamps are shown as given, not guessed at", "parity", "parity"},
	}
	for _, c := range cases {
		if got := DisplayVersion(c.raw, rels); got != c.want {
			t.Errorf("%s: DisplayVersion(%q) = %q, want %q", c.name, c.raw, got, c.want)
		}
	}
	if got := DisplayVersion("dev", Parse("## Unreleased\n\n- x\n")); got != "dev" {
		t.Errorf("without a released heading there is no base: %q", got)
	}
}

// A minor release carries a lighthouse name in its heading (docs/release-names.md); the What's
// new page must still list it, with its name, or the running release silently disappears there.
func TestParseNamedReleaseHeading(t *testing.T) {
	rels := Parse("## Unreleased\n\n## 0.21.0 \"Tower of Hercules\" (2026-10-30)\n\n### Added\n- x\n\n## 0.20.0 (2026-10-30)\n\n- y\n")
	if len(rels) != 3 {
		t.Fatalf("got %d releases, want 3: %+v", len(rels), rels)
	}
	r := rels[1]
	if r.Version != "0.21.0" || r.Name != "Tower of Hercules" || r.Date != "2026-10-30" {
		t.Fatalf("named heading parsed as %+v", r)
	}
	if rels[2].Name != "" || rels[2].Version != "0.20.0" {
		t.Fatalf("unnamed heading parsed as %+v", rels[2])
	}
}
