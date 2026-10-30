package cmdpolicy

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

var statusZero = regexp.MustCompile(`(?i)^status(\+|-)?(0+|off|0x0*)$`)

// FuzzPolicy states the invariants the console relies on, for any input:
//
//   - an operator can send only read/operator class commands;
//   - nothing that can chain a second command is sendable, except a Backlog;
//   - the text that is sent (Canonical) is judged the same as the original;
//   - Status 0 in any spelling is never sendable without its warning;
//   - Redacted shows arguments only for known-harmless commands, and a
//     malformed line only as RejectedText;
//   - unknown or low roles get nothing.
func FuzzPolicy(f *testing.F) {
	for _, s := range []string{
		"Power ON;Reset 1", "Power ON%3BReset 1", "Power%3BReset", "Power ON\nReset 1", "Power\tON",
		"Power ON\u037eReset 1", "Power ON\uff1bReset 1", "Backlog Power ON", "Backlog0 x", "Json {\"Status\":0}",
		"Backlog Power ON; Reset 1", "Backlog Power ON;\u037eReset", "Backlog MqttPassword s3cret; Power ON",
		"Status 0", "Status0", "Status 00", "Status +0", "Status 0x0", "Status off", "Status 2", "Status 11",
		"Restart 1", "Restart 2", "Reset 1", "Upgrade 1", "OtaUrl x", "Power1 TOGGLE", "Power0 ON", "Power9 ON",
		"SetOption19 1", "SO19", "MqttPassword x", "Rule1 ON x DO y ENDON", "Publish a b", "WebSend [h] Power ON",
		"Wifi 0", "WebServer off", "SaveData", "FriendlyName1 Küche", "", "   ", "\x00", "\xff\xfe",
		strings.Repeat("a", 300), "Power ON\u202eNO", "Power\u200b ON", "Rule1 password=hunter2",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, cmd string) {
		v, cerr := Classify(cmd)
		if v.Class < ClassRead || v.Class > ClassForbidden {
			t.Fatalf("Classify(%q) has class %d", cmd, v.Class)
		}
		if red := Redacted(cmd); errors.Is(cerr, ErrInvalid) && red != RejectedText {
			t.Fatalf("malformed %q logged as %q", cmd, red)
		}
		for _, bad := range []model.Role{model.RoleNone, -1, model.RoleAdmin + 1, 99} {
			if _, err := Authorize(cmd, bad); err == nil {
				t.Fatalf("role %d may send %q", bad, cmd)
			}
		}
		for _, role := range []model.Role{model.RoleOperator, model.RoleAdmin} {
			vv, err := Authorize(cmd, role)
			if err != nil {
				continue
			}
			if vv.Class == ClassForbidden {
				t.Fatalf("forbidden %q authorised for role %d", cmd, role)
			}
			if role == model.RoleOperator && vv.Class != ClassRead && vv.Class != ClassOperator {
				t.Fatalf("operator may send %v command %q", vv.Class, cmd)
			}
			send, _, err := Prepare(cmd, role, PrepareOpts{})
			if err != nil {
				t.Fatalf("Authorize ok but Prepare failed for %q: %v", cmd, err)
			}
			// Only a Backlog may carry ';', and then only the ASCII one.
			if strings.ContainsAny(send, "\r\n\t\x00\x7f") || strings.Contains(strings.ToLower(send), "%3b") {
				t.Fatalf("sendable %q carries a separator: %q", cmd, send)
			}
			head, _, _ := strings.Cut(send, " ")
			if strings.Contains(send, ";") && !strings.EqualFold(strings.TrimRight(head, "0123456789"), "backlog") {
				t.Fatalf("%q chains commands without being a Backlog: %q", cmd, send)
			}
			again, aerr := Classify(send)
			if aerr != nil || again != vv {
				t.Fatalf("canonical %q of %q is judged differently: %+v %v", send, cmd, again, aerr)
			}
			// Status 0 in any spelling must never be sent without its warning.
			tok, rest, _ := strings.Cut(send, " ")
			if strings.EqualFold(strings.TrimRight(tok, "0123456789"), "status") && statusZero.MatchString(tok+strings.ReplaceAll(rest, " ", "")) && vv.Confirm == "" {
				t.Fatalf("Status 0 sendable without a warning as %q", send)
			}
			// Operators keep to the operator set.
			if role == model.RoleOperator && vv.Confirm != "" {
				t.Fatalf("operator may send %q with a consequence %q", send, vv.Confirm)
			}
		}
		if cerr == nil {
			p, _ := parse(cmd)
			c := v.Class
			shown := Redacted(cmd)
			if p.args != "" && p.base != "backlog" && c != ClassRead && c != ClassOperator && !loggedArgs[p.base] && shown != p.name+" ***" {
				t.Fatalf("arguments of %q leak into %q", cmd, shown)
			}
		}
	})
}
