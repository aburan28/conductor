package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"

	"github.com/adamburan/conductor/internal/db"
	"github.com/adamburan/conductor/internal/domain"
	"github.com/adamburan/conductor/internal/quota"
)

// Usage limits (docs/USAGE_LIMITS.md). A principal reports readings of their own logins and
// reads them back; a project's members see how many of the team's logins are near their
// limit, and nothing more.

func (s *Server) quotaRoutes(m *http.ServeMux) {
	auth := s.authenticate
	m.HandleFunc("POST /v1/quota", auth(s.recordQuota))
	m.HandleFunc("GET /v1/quota", auth(s.getQuota))
	m.HandleFunc("GET /v1/projects/{project}/quota", auth(s.getProjectQuota))
}

type quotaBody struct {
	Snapshots  []quota.Snapshot `json:"snapshots"`
	Thresholds quota.Thresholds `json:"thresholds"`
	// Project, when set, is the project whose event stream hears about a crossed level —
	// the one the reporting session works in. The readings themselves belong to the caller.
	Project string `json:"project,omitempty"`
}

// maxQuotaSnapshots bounds one report: a machine has a handful of logins with two or three
// windows each, so this is generous and still cannot be used to flood the table.
const maxQuotaSnapshots = 200

// quotaLabel is the shape of every label a snapshot carries. It admits a state directory's
// name, a window name, a host name and a plan, and refuses anything with '@', spaces, or
// slashes — an email address or a path pasted into the wrong field does not get stored.
var quotaLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,63}$`)

func validQuotaSnapshot(q *quota.Snapshot, now time.Time) error {
	for field, v := range map[string]string{"harness": q.Harness, "account": q.Account, "window": q.Window, "source": q.Source} {
		if !quotaLabel.MatchString(v) {
			return fmt.Errorf("%w: quota snapshot %s %q is not a label", domain.ErrInvalidArgument, field, v)
		}
	}
	for field, v := range map[string]string{"machine": q.Machine, "unit": q.Unit, "plan": q.Plan} {
		if v != "" && !quotaLabel.MatchString(v) {
			return fmt.Errorf("%w: quota snapshot %s %q is not a label", domain.ErrInvalidArgument, field, v)
		}
	}
	if !quota.ValidKind(q.SourceKind) {
		return fmt.Errorf("%w: quota snapshot source_kind %q", domain.ErrInvalidArgument, q.SourceKind)
	}
	for _, v := range []*float64{q.UsedPercent, q.Used, q.Limit} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 1e12) {
			return fmt.Errorf("%w: quota snapshot value out of range", domain.ErrInvalidArgument)
		}
	}
	if q.WindowMinutes < 0 || q.WindowMinutes > 366*24*60 {
		return fmt.Errorf("%w: quota snapshot window_minutes out of range", domain.ErrInvalidArgument)
	}
	// A reading from the future is a skewed clock; it would pin the window until the clock
	// caught up, so it is recorded as now.
	if q.ObservedAt.IsZero() || q.ObservedAt.After(now) {
		q.ObservedAt = now
	}
	return nil
}

// recordQuota stores the caller's readings: what the wrap sidecar, `conductor quota`, and
// `conductor usage sync` collect on the caller's machine.
func (s *Server) recordQuota(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	var body quotaBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		s.fail(w, r, domain.ErrInvalidArgument)
		return
	}
	if len(body.Snapshots) > maxQuotaSnapshots {
		s.fail(w, r, fmt.Errorf("%w: at most %d snapshots per report", domain.ErrInvalidArgument, maxQuotaSnapshots))
		return
	}
	now := time.Now().UTC()
	for i := range body.Snapshots {
		if err := validQuotaSnapshot(&body.Snapshots[i], now); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	var target db.QuotaEventTarget
	if body.Project != "" {
		project, err := s.resolveProject(r.Context(), p, body.Project)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if _, err := s.svc.Authorize(r.Context(), p, project.ID, domain.RoleContributor); err != nil {
			s.fail(w, r, err)
			return
		}
		target = db.QuotaEventTarget{OrganizationID: project.OrganizationID, ProjectID: project.ID}
	}
	report, err := s.store.RecordQuota(r.Context(), p.ID, body.Snapshots, body.Thresholds.Normalize(), target)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, report)
}

func (s *Server) quotaView(r *http.Request, p domain.Principal) (quota.View, error) {
	snaps, err := s.store.ListQuota(r.Context(), p.ID)
	if err != nil {
		return quota.View{}, err
	}
	// The server does not know each person's thresholds; the defaults classify here, and
	// the CLI reclassifies with the owner's own.
	return quota.NewView(snaps, quota.DefaultThresholds, time.Now().UTC()), nil
}

// getQuota returns the caller's own readings, from every machine they reported from. There
// is no way to ask for anyone else's.
func (s *Server) getQuota(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	view, err := s.quotaView(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, view)
}

// teamQuotaWindow is how recent a reading must be to count: a day covers anyone who worked
// today without counting a login nobody has touched in a week.
const teamQuotaWindow = 24 * time.Hour

func (s *Server) getProjectQuota(w http.ResponseWriter, r *http.Request, p domain.Principal) {
	project, _, err := s.project(r, p, domain.RoleObserver)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now().UTC()
	rows, err := s.store.ListProjectQuota(r.Context(), project.ID, now.Add(-teamQuotaWindow))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	th := quota.DefaultThresholds
	worst := map[int]quota.Level{}
	for _, row := range rows {
		lvl := th.Level(row.Snapshot, now)
		if lvl.Rank() > worst[row.Login].Rank() {
			worst[row.Login] = lvl
		} else if _, ok := worst[row.Login]; !ok {
			worst[row.Login] = lvl
		}
	}
	out := quota.TeamView{Project: project.Slug, WindowHours: int(teamQuotaWindow / time.Hour), Logins: len(worst)}
	for _, lvl := range worst {
		if lvl.Rank() >= quota.LevelWarning.Rank() {
			out.NearLimit++
		}
		if lvl == quota.LevelExhausted {
			out.Exhausted++
		}
	}
	if out.Mine, err = s.quotaView(r, p); err != nil {
		s.fail(w, r, err)
		return
	}
	s.ok(w, r, http.StatusOK, out)
}
