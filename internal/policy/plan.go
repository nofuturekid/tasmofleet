package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/cmdpolicy"
	"github.com/nofuturekid/tasmofleet/internal/model"
	"github.com/nofuturekid/tasmofleet/internal/settings"
)

// "Make compliant" turns findings into the commands that remove the
// deviations. Only a deviation whose current value was read LIVE becomes a
// command: a value from an old backup says what the device had then, not what
// it has now, and an unknown value says nothing. Both are listed and skipped,
// never guessed. The ESP32 exception needs no code here: Check already leaves
// out the rules that do not apply to a device's family, tags or name.

// Why a deviation was not turned into a command.
const (
	SkipUnknown  = "the current value is unknown"
	SkipNotLive  = "only known from a backup, not read live"
	SkipConflict = "two rules expect different values"
)

// FixCommand is one command of a plan.
type FixCommand struct {
	Command string // the rule's setting, e.g. "SetOption0"
	Text    string // what is sent, e.g. "SetOption0 0"
	Have    string // the live value
	Want    string
}

// FixSkip is a deviation that is listed but not fixed.
type FixSkip struct {
	Command string
	Reason  string
}

// FixPlan is what "Make compliant" would do for one device. Commands are in
// the order they are sent (see OrderCommands).
type FixPlan struct {
	DeviceID model.DeviceID
	Commands []FixCommand
	Skipped  []FixSkip
}

// Lines are the command texts in sending order.
func (p FixPlan) Lines() []string {
	out := make([]string, len(p.Commands))
	for i, c := range p.Commands {
		out[i] = c.Text
	}
	return out
}

var setOptionRe = regexp.MustCompile(`^SetOption[0-9]{1,3}$`)

var setOptionValueRe = regexp.MustCompile(`^[0-9]{1,5}$`)

// CommandText is the command that sets a rule's setting to want. The settings
// the editor knows are set with the editor's validation; a SetOption takes a
// plain number. Any other command (a generic rule) is set as "<Command> <want>"
// only when the command policy lets an admin send it without a confirmation:
// nothing that asks in the console is ever sent from a rule.
func CommandText(command, want string) (string, error) {
	want = canon(want)
	if canonName, curated := settings.Curated(command); curated && canonName != command {
		return "", fmt.Errorf("%s: write the setting as %s", command, canonName)
	}
	if f, ok := settings.Lookup(command); ok {
		if f.Secret {
			return "", fmt.Errorf("%s is write-only", command)
		}
		if err := f.Validate(want); err != nil {
			return "", err
		}
		return f.Command(want), nil
	}
	if setOptionRe.MatchString(command) && setOptionValueRe.MatchString(want) {
		if settings.SetOptionParam(command) {
			if err := settings.ValidateSetOptionParam(want); err != nil {
				return "", err
			}
		}
		return command + " " + want, nil
	}
	return genericText(command, want)
}

// genericText is the set command of a generic rule.
func genericText(command, want string) (string, error) {
	if !wantRe.MatchString(want) {
		return "", fmt.Errorf("%s: the expected value is not a plain value", command)
	}
	if ok, why := cmdpolicy.PolicyReadable(command); !ok {
		return "", fmt.Errorf("%s cannot be set from the policy: %s", command, why)
	}
	text, err := cmdpolicy.PolicySetText(command, want)
	if err != nil {
		return "", fmt.Errorf("%s cannot be set from the policy: %w", command, err)
	}
	return text, nil
}

// PlanFix builds one plan per device that has at least one finding, in the
// order the devices first appear. live says whether the finding's value (or
// lack of one) comes from a live read of the device.
func PlanFix(findings []Finding, live func(model.DeviceID, string) bool) []FixPlan {
	var order []model.DeviceID
	plans := map[model.DeviceID]*FixPlan{}
	wants := map[model.DeviceID]map[string]string{}
	for _, f := range findings {
		p, ok := plans[f.DeviceID]
		if !ok {
			p = &FixPlan{DeviceID: f.DeviceID}
			plans[f.DeviceID] = p
			wants[f.DeviceID] = map[string]string{}
			order = append(order, f.DeviceID)
		}
		key := strings.ToLower(f.Rule.Command)
		skip := func(reason string) { p.Skipped = append(p.Skipped, FixSkip{f.Rule.Command, reason}) }
		switch {
		case f.Unknown:
			skip(SkipUnknown)
			continue
		case !live(f.DeviceID, key):
			skip(SkipNotLive)
			continue
		}
		if prev, dup := wants[f.DeviceID][key]; dup {
			if canon(prev) != canon(f.Rule.Want) {
				p.Commands = dropCommand(p.Commands, f.Rule.Command)
				skip(SkipConflict)
			}
			continue
		}
		text, err := CommandText(f.Rule.Command, f.Rule.Want)
		if err != nil {
			skip(err.Error())
			continue
		}
		wants[f.DeviceID][key] = f.Rule.Want
		p.Commands = append(p.Commands, FixCommand{Command: f.Rule.Command, Text: text, Have: f.Have, Want: f.Rule.Want})
	}
	out := make([]FixPlan, 0, len(order))
	for _, id := range order {
		p := plans[id]
		p.Commands = OrderCommands(p.Commands)
		out = append(out, *p)
	}
	return out
}

func dropCommand(cs []FixCommand, command string) []FixCommand {
	out := cs[:0:0]
	for _, c := range cs {
		if !strings.EqualFold(c.Command, command) {
			out = append(out, c)
		}
	}
	return out
}

// OrderCommands puts the commands in the order they must be sent. A command
// that restarts the device goes last (it would cut off whatever followed, and
// the restart saves everything), and SaveData goes right before those: its
// handler writes all settings to flash, so anything after it would stay in
// memory only. The rest keeps the rules' order.
func OrderCommands(cs []FixCommand) []FixCommand {
	var plain, save, restart []FixCommand
	for _, c := range cs {
		switch {
		case settings.RestartsDevice(c.Command):
			restart = append(restart, c)
		case strings.EqualFold(c.Command, "SaveData"):
			save = append(save, c)
		default:
			plain = append(plain, c)
		}
	}
	return append(append(plain, save...), restart...)
}
