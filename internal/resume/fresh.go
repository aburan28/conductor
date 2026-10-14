package resume

import (
	"github.com/aburan28/conductor/internal/accounts"
)

// AccountSwitch records one exhausted account being moved off before sessions
// were resumed.
type AccountSwitch struct {
	Harness string `json:"harness"`
	From    string `json:"from"`
	To      string `json:"to"`
}

// Summary is the machine-local resume endpoint's whole answer: which accounts were
// switched and what happened to each session.
type Summary struct {
	Switched []AccountSwitch `json:"switched"`
	Actions  []Action        `json:"actions"`
	Resumed  int             `json:"resumed"`
	Running  int             `json:"running"`
	Failed   int             `json:"failed"`
}

// AllFresh is the one-button reboot recovery: for every harness that has account
// profiles, move off an account marked out of usage (onto the first profile
// with quota left, when one exists), then wake every paused or saved session.
//
// Switching happens before resuming because harnesses read their credentials at
// startup — a session reopened after a switch comes up under the fresh account.
func AllFresh() (Summary, error) {
	sum := Summary{Switched: []AccountSwitch{}, Actions: []Action{}}
	for _, harness := range accounts.Harnesses() {
		profiles, err := accounts.List(harness)
		if err != nil || len(profiles) < 2 {
			// No profiles registered, or the accounts dir is unreadable: resume is
			// still the point; account switching is an opt-in layer on top.
			continue
		}
		from := accounts.Active(harness)
		if from == "" {
			continue
		}
		to, err := accounts.EnsureUsable(harness)
		if err != nil {
			sum.Failed++
			sum.Actions = append(sum.Actions, Action{Action: "error",
				Detail: harness + " account switch: " + err.Error()})
			continue
		}
		if to != "" {
			sum.Switched = append(sum.Switched, AccountSwitch{Harness: harness, From: from, To: to})
		}
	}
	actions, err := All("")
	if err != nil {
		return sum, err
	}
	sum.Actions = append(sum.Actions, actions...)
	for _, a := range actions {
		switch a.Action {
		case "error":
			sum.Failed++
		case "running":
			sum.Running++
		default:
			sum.Resumed++
		}
	}
	return sum, nil
}
