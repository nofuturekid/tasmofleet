// Package changelog parses CHANGELOG.md for the What's new page (design v2, N5): releases with
// their date, sections (Added, Changed, Fixed, ...) and items. Inline text is read like firmware
// release notes (code spans, bold, links through firmware.NoteLink); everything else, raw HTML
// included, stays literal text, so the page escapes it like any other value.
package changelog

import (
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/nofuturekid/tasmofleet"
	"github.com/nofuturekid/tasmofleet/internal/firmware"
)

// Unreleased is the version heading of changes not yet in a release.
const Unreleased = "Unreleased"

// Release is one "## <version> ["<name>"] (<date>)" block, newest first in the file.
type Release struct {
	Version  string
	Name     string              // release name (a lighthouse, docs/release-names.md), empty for older releases
	Date     string              // YYYY-MM-DD, empty when the heading has none
	Intro    [][]firmware.Inline // paragraphs before the first section or item
	Sections []Section
}

// Section is one "### <title>" block; Title is empty for items listed without a section
// (the oldest releases).
type Section struct {
	Title string
	Items []Item
}

// Item is one bullet; Sub holds its nested bullets (one level, as the file uses).
type Item struct {
	Text []firmware.Inline
	Sub  [][]firmware.Inline
}

var (
	releaseHead = regexp.MustCompile(`^## +(\S+)(?: +"([^"]+)")?(?: +\((\d{4}-\d{2}-\d{2})\))?\s*$`)
	semver      = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
)

// Parse reads a changelog. It never fails: text before the first release heading (the file's
// preamble) is dropped, unknown lines become paragraph text.
func Parse(md string) []Release {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	var (
		out  []Release
		rel  *Release
		sec  *Section
		buf  []string // lines of the current item, sub-item or paragraph
		kind byte     // 'i' item, 's' sub-item, 'p' paragraph, 0 none
	)
	flush := func() {
		if len(buf) == 0 || rel == nil {
			buf, kind = nil, 0
			return
		}
		text := firmware.ParseInline(strings.Join(buf, " "), nil)
		switch kind {
		case 'p':
			rel.Intro = append(rel.Intro, text)
		case 'i':
			if sec == nil {
				rel.Sections = append(rel.Sections, Section{})
				sec = &rel.Sections[len(rel.Sections)-1]
			}
			sec.Items = append(sec.Items, Item{Text: text})
		case 's':
			if sec != nil && len(sec.Items) > 0 {
				last := &sec.Items[len(sec.Items)-1]
				last.Sub = append(last.Sub, text)
			}
		}
		buf, kind = nil, 0
	}
	for _, line := range strings.Split(md, "\n") {
		trim := strings.TrimSpace(line)
		switch {
		case releaseHead.MatchString(line):
			flush()
			m := releaseHead.FindStringSubmatch(line)
			out = append(out, Release{Version: m[1], Name: m[2], Date: m[3]})
			rel, sec = &out[len(out)-1], nil
		case rel == nil:
			// preamble
		case strings.HasPrefix(line, "### "):
			flush()
			rel.Sections = append(rel.Sections, Section{Title: strings.TrimSpace(line[4:])})
			sec = &rel.Sections[len(rel.Sections)-1]
		case strings.HasPrefix(line, "- "):
			flush()
			buf, kind = []string{trim[2:]}, 'i'
		case strings.HasPrefix(trim, "- ") && kind != 'p' && kind != 0:
			flush()
			buf, kind = []string{trim[2:]}, 's'
		case trim == "":
			if kind == 'p' {
				flush()
			}
		case kind != 0:
			buf = append(buf, trim)
		default:
			buf, kind = []string{trim}, 'p'
		}
	}
	flush()
	return out
}

// Embedded returns the parsed changelog of this binary, parsed once.
var Embedded = sync.OnceValue(func() []Release { return Parse(tasmofleet.Changelog) })

// Compare orders two release versions x.y.z numerically: -1, 0 or 1. Anything that is not
// x.y.z (Unreleased, "dev", empty) sorts below every release, so it never counts as news.
func Compare(a, b string) int {
	pa, okA := parse(a)
	pb, okB := parse(b)
	switch {
	case !okA && !okB:
		return 0
	case !okA:
		return -1
	case !okB:
		return 1
	}
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parse(v string) ([3]int, bool) {
	m := semver.FindStringSubmatch(v)
	if m == nil {
		return [3]int{}, false
	}
	var out [3]int
	for i := range out {
		out[i], _ = strconv.Atoi(m[i+1])
	}
	return out, true
}

// Valid reports whether v is a release version (x.y.z).
func Valid(v string) bool {
	_, ok := parse(v)
	return ok
}

// Latest is the newest released version in rels ("" when there is none); Unreleased never is.
func Latest(rels []Release) string {
	latest := ""
	for _, r := range rels {
		if _, ok := parse(r.Version); ok && (latest == "" || Compare(r.Version, latest) > 0) {
			latest = r.Version
		}
	}
	return latest
}

// NewSince counts the releases in rels a user has not seen: those newer than seen. With no seen
// version (the user never opened What's new) only the newest release counts, so a fresh account
// gets one hint instead of the whole history.
func NewSince(rels []Release, seen string) int {
	if !Valid(seen) {
		if Latest(rels) != "" {
			return 1
		}
		return 0
	}
	n := 0
	for _, r := range rels {
		if Compare(r.Version, seen) > 0 {
			n++
		}
	}
	return n
}

// Known reports whether version is a release heading in rels (Unreleased included).
func Known(rels []Release, version string) bool {
	for _, r := range rels {
		if r.Version == version {
			return true
		}
	}
	return false
}

var ciBuild = regexp.MustCompile(`^(edge|pr)-([0-9a-f]{7,40})$`)

// DisplayVersion is the version as the UI and the log show it. A release build (v0.15.2, 0.15.2)
// and a git describe string (v0.15.2-3-gabc1234) already name their release and stay as they
// are. Builds without one get the newest released heading of rels as base: the CI images
// edge-<sha> and pr-<sha> become "0.15.2+edge (<sha7>)" (the commit abbreviated to 7 characters, as
// GitHub shows it; the stamp itself keeps its length), and "dev" (a plain docker build without
// git history) "0.15.2+dev". Anything else, and a changelog without a release, is shown as given.
func DisplayVersion(raw string, rels []Release) string {
	base := Latest(rels)
	if base == "" {
		return raw
	}
	if m := ciBuild.FindStringSubmatch(raw); m != nil {
		return base + "+" + m[1] + " (" + m[2][:7] + ")" // 7 characters, like GitHub (#436)
	}
	if raw == "dev" {
		return base + "+dev"
	}
	return raw
}
