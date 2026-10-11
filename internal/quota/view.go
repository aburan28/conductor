package quota

import "time"

// Row is one reading as its owner sees it, with the level it stands at now.
type Row struct {
	Snapshot
	Level Level `json:"level"`
}

// View is an owner's view of their logins: GET /v1/quota.
type View struct {
	Thresholds Thresholds `json:"thresholds"`
	Snapshots  []Row      `json:"snapshots"`
	Logins     []Login    `json:"logins"`
}

// NewView classifies readings for their owner.
func NewView(snaps []Snapshot, t Thresholds, now time.Time) View {
	v := View{Thresholds: t, Snapshots: make([]Row, 0, len(snaps)), Logins: Logins(snaps, t, now)}
	for _, s := range snaps {
		v.Snapshots = append(v.Snapshots, Row{Snapshot: s, Level: t.Level(s, now)})
	}
	if v.Logins == nil {
		v.Logins = []Login{}
	}
	return v
}

// TeamView is everything a project member learns about other people's logins: counts.
// GET /v1/projects/{p}/quota.
type TeamView struct {
	Project     string `json:"project"`
	WindowHours int    `json:"window_hours"`
	Logins      int    `json:"logins"`     // logins of project members reported recently
	NearLimit   int    `json:"near_limit"` // at or above the warning threshold, exhausted included
	Exhausted   int    `json:"exhausted"`
	// Mine is the caller's own view, so one request serves the dashboard card.
	Mine View `json:"mine"`
}
