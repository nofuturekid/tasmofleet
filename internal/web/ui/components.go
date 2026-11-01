package ui

import (
	"context"
	"hash/fnv"
	"html"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
)

// softWrite writes the parts HTML-escaped with a <wbr> between them. Written by hand, not in a
// .templ file: templ puts a space between sibling nodes, and a space is a visible character.
func softWrite(parts []string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		for i, p := range parts {
			if i > 0 {
				if _, err := io.WriteString(w, "<wbr>"); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, html.EscapeString(p)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Soft writes s with break opportunities (<wbr>) at the natural points of a file name, URL or
// host:port (SoftParts), so a narrow column breaks "tasmota-minimal.bin.gz" after "minimal." and
// not inside "bin". The text is the same; overflow-wrap still cuts a token that has no point.
func Soft(s string) templ.Component { return softWrite(SoftParts(s)) }

// SoftJSON writes a device's reply with break opportunities after its commas (SoftJSONParts).
func SoftJSON(s string) templ.Component { return softWrite(SoftJSONParts(s)) }

// SoftJSONParts cuts s after every comma: a device's JSON reply has no spaces, so without these
// points a narrow box would break it inside "Boot" or "ESP8266EX".
func SoftJSONParts(s string) []string {
	parts := strings.SplitAfter(s, ",")
	if n := len(parts); n > 1 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	return parts
}

// SoftParts cuts s after the characters where a file name, URL or host:port reads naturally across
// two lines: after "/", "-" and "_", after a "." that is followed by a letter (hostnames and
// extensions, not the dots of an IP address or a version), and after a ":" that is followed by a
// digit (the port). Joined again the parts are s.
func SoftParts(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s)-1; i++ {
		next := s[i+1]
		isLetter := next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z'
		isDigit := next >= '0' && next <= '9'
		if strings.IndexByte("/-_", s[i]) >= 0 && next != s[i] || s[i] == '.' && isLetter || s[i] == ':' && isDigit {
			parts = append(parts, s[start:i+1])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

// The design system's components (design v2, WP F4; docs/spec/design-system.md, section
// "Components"). Each component takes typed props; values are text, never markup, so templ
// escapes all of them. Sizes that come from data (progress segments, the fleet health bar) are
// SVG presentation attributes, because the content security policy forbids style attributes.

// Tone is the colour family of a badge, dot, banner, toast or tile: ok, warn, danger, neutral or
// accent (information). Anything else renders as neutral.
type Tone string

// The tones of the prototype (TONE in design/v2/template.html).
const (
	ToneOK      Tone = "ok"
	ToneWarn    Tone = "warn"
	ToneDanger  Tone = "danger"
	ToneNeutral Tone = "neutral"
	ToneAccent  Tone = "accent"
)

// class is the tone's CSS class; an unknown tone is neutral, so a typo never drops the colour.
func (t Tone) class() string {
	switch t {
	case ToneOK, ToneWarn, ToneDanger, ToneAccent, ToneNeutral:
		return "t-" + string(t)
	case toneRest: // never drawn
	}
	return "t-neutral"
}

// Btn is a button or a link that looks like one. Kind is "" (default), "primary", "danger"
// (outlined), "danger-solid", "ghost" or "link" (text only). With Href it is a link (it only
// navigates, or opens the dialog named by Dialog); without it a button of Type (default
// "button"). Name and Value make it a submit button that sends a value (console chips).
type Btn struct {
	Label, Kind, Href, Type string
	Name, Value             string
	Dialog                  string // id of the dialog dialog.js opens instead of following Href
	Small, Disabled         bool
	Aria                    string // accessible name when the label is a glyph (icon button "⋯")
	Icon                    bool   // square icon button
}

func (b Btn) classes() string {
	c := "btn"
	if b.Kind != "" {
		c += " btn-" + b.Kind
	}
	if b.Small {
		c += " btn-sm"
	}
	if b.Icon {
		c += " btn-icon"
	}
	return c
}

func (b Btn) buttonType() string {
	if b.Type == "" {
		return "button"
	}
	return b.Type
}

// BannerData is an inline banner: a dot, a bold lead, the text and an optional action.
type BannerData struct {
	Tone        Tone
	Title, Text string
	Action      *Btn
	Alert       bool // role=alert (a failure the user has to act on) instead of role=status
}

// ToastData is a toast shown in the flow (the style guide); live toasts come from toast.js.
type ToastData struct {
	Tone         Tone
	Text, Action string
}

// StatTileData is one dashboard tile, a link to the filtered list.
type StatTileData struct {
	Tone               Tone
	Label, Value, Hint string
	Href               string
}

// KVRow is one label/value line of a card; Mono sets the value in the monospace font
// (addresses, versions, ids).
type KVRow struct {
	Key, Value string
	Mono       bool
	Mask       bool      // a value the pixel comparison cannot reproduce (address, MAC, readings): data-visual-mask
	Wide       bool      // a long value: on a phone it takes the whole row of a compact card (Identity)
	Soft       bool      // a host:port, URL or file name: break opportunities at its natural points (Soft)
	At         time.Time // when set, Value is the fallback text of that instant and time.js shows it in the browser's zone
	Stamp      StampFmt  // how time.js shows At; the zero value is StampShort
}

// Tab is one tab of a tab row. Tabs are links (each tab its own route). A disabled tab says why.
type Tab struct {
	Label, Href string
	Current     bool
	Badge       int
	Reason      string // the tab cannot be used now: why (rendered disabled, reason as title)
}

// MenuItem is one entry of a row menu: a link (Href), or with Dialog a link that opens that
// dialog. Sep draws a rule before it; Why explains a disabled entry.
type MenuItem struct {
	Label, Href, Dialog string
	Danger, Sep         bool
	Disabled            bool
	Why                 string
}

// DialogData is a confirmation dialog: title, body, an optional list of what changes or a
// command input, Cancel and the confirming verb. The form posts to Action with the CSRF token.
type DialogData struct {
	ID, Title, Body string
	Changes         []KVRow // name and the command or value that changes
	Command         string  // non-empty: a Command input prefilled with this value
	Action, CSRF    string
	Fields          []KVRow // hidden fields the form posts (Key = name)
	Verb            string
	// Alt, when set, is a second button that posts the same fields to AltAction instead
	// (Make compliant: "Choose Advisory rows…" opens the preview with the boxes).
	Alt, AltAction string
	Danger         bool
	Static         bool // drawn open in the flow (style guide), left alone by the scripts
	OOB            bool // sent as an htmx out-of-band swap of the dialog already on the page
}

// StepUpData is the "Confirm it's you" dialog of local account and SSO changes.
type StepUpData struct {
	ID, Name, Sub string // who confirms, and how they signed in
	What          string // the action that needs the confirmation
	IdP, IdPHref  string // identity provider and its re-authentication route (POST); empty for local accounts
	// IdPHref empty with an IdP: the re-authentication is the form's own submit (a POST to Action).
	Local        bool   // a local password is accepted (posted as passwordName() to Action)
	PasswordName string // the password input's name; empty: "current"
	Action, CSRF string
	Next         string  // local path the identity provider returns to (form field "next")
	Fields       []KVRow // further hidden fields the form posts (Key = name)
	Error        string
	Static       bool // drawn open in the flow (style guide), left alone by the scripts
	Open         bool // the page answers a refused action: dialog.js opens it as a modal at once
}

func (s StepUpData) passwordName() string {
	if s.PasswordName == "" {
		return "current"
	}
	return s.PasswordName
}

// ConfirmData is the page form of a confirmation, for browsers without JavaScript: the same
// look as the dialog, its own route.
type ConfirmData struct {
	Title, Heading, Text, Warning string
	Facts                         []KVRow
	Action, CSRF                  string
	Fields                        []KVRow // hidden fields (Key = name)
	Ack                           string  // a required checkbox "ack" with this label; empty: none
	Verb                          string
	Danger                        bool
	CancelHref, CancelLabel       string
}

// FieldData is a label-above input with help text and the server's error for it.
type FieldData struct {
	Label, Name, Type, Value string
	Placeholder, Help, Error string
	Mono, Wide               bool
	Autocomplete             string
	Autofocus                bool // the page's first focus: the field the server rejected, else the first input
}

// Option is one choice of a radio card group or segmented control.
type Option struct {
	Value, Label, Text string
	Checked, Disabled  bool
}

// AttProposal is a "Needs attention" proposal with two inline actions (POST forms). Version is
// the proposal content the admin saw; both forms send it, so an action never applies content that
// changed after the page was drawn.
type AttProposal struct {
	Name, Text                     string
	Primary, Secondary             string // button labels
	PrimaryAction, SecondaryAction string // POST paths
	Version                        string
}

// AttItem is a "Needs attention" device: a dot, the name and the reason, a link to it.
type AttItem struct {
	Tone             Tone
	Name, Text, Href string
}

// TxnData is a transaction card: id, title, state badge, the three-colour progress and a summary.
type TxnData struct {
	ID, Title, State, Summary, Href string
	StateTone                       Tone
	OK, Failed, Active, Total       int
	// Stopped devices were never started (a template transaction stops after the first failure); drawn grey.
	Stopped int
}

// TxnRowData is one row of the transaction history.
type TxnRowData struct {
	ID, Title, Result, When, Href string
	Tone                          Tone
	WhenAt                        time.Time // when set, When is the fallback text of that instant (StampDay) and time.js shows it in the browser's zone
}

// ConsoleLine is one line of the console: time, text, kind ("cmd", "err", "reply" for the
// device's answer to the command just sent, "alert" for its refusal or failure, or "" for other
// output). Datetime (RFC 3339) makes
// the time a <time> element; Who (who sent a command) follows the text, dimmed.
type ConsoleLine struct{ Time, Datetime, Text, Kind, Who string }

// ConsoleData is the device console: the lines, the prompt form and the quick commands (submit
// buttons below the panel that send their command). The forms post the field "command";
// Hidden fields go with every send (the console's "since", for example). ClearHref makes Clear
// a link (the page without the lines so far).
type ConsoleData struct {
	Title, Action, CSRF string
	Input               string // the prompt's value (a command sent back for correction)
	HistoryAdd          string // a command just sent that the browser may keep in its Arrow Up history ("" keeps nothing)
	ClearHref           string
	Hidden              []KVRow
	Lines               []ConsoleLine
	Quick               []string
}

// DiffRow is one setting of a backup comparison; Changed marks a difference.
type DiffRow struct {
	Key, Old, New string
	Changed       bool
}

// DiffData is a comparison of two backups, side by side or unified.
type DiffData struct {
	OldLabel, NewLabel string
	Unified            bool
	Rows               []DiffRow
}

// NoteBlock is one block of release notes: "h" heading, "p" paragraph, "ul" list, "code" code
// block, "breaking" the breaking-change box (Text is its heading, Items its points). Text only:
// release notes never become markup (firmware/notes.go contract).
type NoteBlock struct {
	Kind, Text string
	Items      []string
}

// units is n of total as a share of 1000 units (the SVG bars' coordinate space).
func units(n, total int) int {
	if total <= 0 || n <= 0 {
		return 0
	}
	return n * 1000 / total
}

// barSeg is one segment of a bar: its x and width in the 1000-unit space and its tone.
type barSeg struct {
	X, W int
	Tone Tone
}

// segments lays out the parts of a bar left to right with gap units between non-empty parts.
func segments(gap int, parts ...struct {
	N    int
	Tone Tone
}) []barSeg {
	total := 0
	for _, p := range parts {
		total += p.N
	}
	var out []barSeg
	x := 0
	for _, p := range parts {
		w := units(p.N, total)
		if w == 0 {
			continue
		}
		out = append(out, barSeg{X: x, W: w, Tone: p.Tone})
		x += w
	}
	if len(out) > 1 { // shrink each part by its share of the gaps so the bar keeps its width
		g := gap * (len(out) - 1)
		x = 0
		for i := range out {
			out[i].W = out[i].W - g/len(out)
			if out[i].W < 1 {
				out[i].W = 1
			}
			out[i].X = x
			x += out[i].W + gap
		}
	}
	return out
}

type part = struct {
	N    int
	Tone Tone
}

// healthSegments are the fleet health bar's parts: online, offline, unknown.
func healthSegments(ok, off, unk int) []barSeg {
	return segments(2, part{ok, ToneOK}, part{off, ToneDanger}, part{unk, ToneNeutral})
}

// complianceSegments are a rule's bar: match, deviate, not judged. Same math as the health bar.
func complianceSegments(match, deviate, unknown int) []barSeg {
	return segments(2, part{match, ToneOK}, part{deviate, ToneWarn}, part{unknown, ToneNeutral})
}

// txnSegments are the transaction bar's parts: succeeded, failed, running. The devices still
// waiting (Total minus the three) are the empty rest of the bar, as in the prototype.
func txnSegments(t TxnData) []barSeg {
	rest := max(t.Total-t.OK-t.Failed-t.Active-t.Stopped, 0)
	segs := segments(1, part{t.OK, ToneOK}, part{t.Failed, ToneDanger}, part{t.Active, ToneAccent}, part{t.Stopped, ToneNeutral}, part{rest, toneRest})
	out := segs[:0]
	for _, s := range segs {
		if s.Tone != toneRest {
			out = append(out, s)
		}
	}
	return out
}

// toneRest marks the undrawn rest of a bar.
const toneRest Tone = "rest"

// AvatarTone picks the avatar's colour from the username, so a user keeps one colour everywhere.
func AvatarTone(username string) Tone {
	tones := [...]Tone{ToneAccent, ToneOK, ToneWarn, ToneDanger, ToneNeutral}
	h := fnv.New32a()
	_, _ = h.Write([]byte(username)) // a hash.Hash never returns an error from Write
	return tones[h.Sum32()%uint32(len(tones))]
}

func itoa(n int) string { return strconv.Itoa(n) }

// stampOf is the format of a KVRow's time: StampShort unless the row names another.
func (r KVRow) stampOf() StampFmt {
	if r.Stamp.Key == "" {
		return StampShort
	}
	return r.Stamp
}
