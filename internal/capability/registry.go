package capability

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Target is where calibration items were sent.
type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort,omitempty"`
}

func (t Target) String() string {
	s := t.Provider + ":" + t.Model
	if t.Effort != "" {
		s += "@" + t.Effort
	}
	return s
}

// Calibration is one suite run against one target, graded. Answers that never arrived
// (errored, expired, missing) are counted apart and excluded from the pass rate, since they
// measure the infrastructure, not the model; a refusal is an answer and counts as a failure.
type Calibration struct {
	Schema      string   `json:"schema"`
	Suite       string   `json:"suite"`
	SuiteSHA256 string   `json:"suite_sha256"`
	Family      string   `json:"family"`
	Target      Target   `json:"target"`
	Graded      int      `json:"graded"`
	Passed      int      `json:"passed"`
	Refused     int      `json:"refused"`
	NotAnswered int      `json:"not_answered"`
	InputTokens int64    `json:"input_tokens"`
	OutputToks  int64    `json:"output_tokens"`
	CostUSD     *float64 `json:"estimated_cost_usd,omitempty"`
	RunDir      string   `json:"run_dir"`
	FinishedAt  string   `json:"finished_at"`
	Grades      []Grade  `json:"grades"`
}

// Evidence is one public benchmark result. It is never a number typed from memory: every
// record names where it came from and when it was read.
type Evidence struct {
	Schema    string   `json:"schema"`
	Model     string   `json:"model"`
	Aliases   []string `json:"aliases,omitempty"` // the same model under other providers' ids
	Benchmark string   `json:"benchmark"`
	Version   string   `json:"version"`
	Score     float64  `json:"score"`
	// Scale is percent (0..100), fraction (0..1), elo or index. Only percent and fraction
	// feed a family prior; elo and index scores are shown, never averaged.
	Scale        string    `json:"scale"`
	Interval     []float64 `json:"interval,omitempty"`
	SourceURL    string    `json:"source_url"`
	Retrieved    string    `json:"retrieved"` // YYYY-MM-DD
	SelfReported bool      `json:"self_reported"`
	Notes        string    `json:"notes,omitempty"`
}

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// Validate lists what is wrong with e; now bounds the retrieval date.
func (e Evidence) Validate(now time.Time) []string {
	var out []string
	if e.Schema != EvidenceSchema {
		out = append(out, "schema must be "+EvidenceSchema)
	}
	if strings.TrimSpace(e.Model) == "" || strings.TrimSpace(e.Benchmark) == "" {
		out = append(out, "model and benchmark are required")
	}
	if strings.TrimSpace(e.Version) == "" {
		out = append(out, "version is required (the benchmark's version, split or date)")
	}
	if u, err := url.Parse(e.SourceURL); err != nil || u.Scheme != "https" || u.Host == "" {
		out = append(out, "source_url must be an https URL")
	}
	if d, err := time.Parse("2006-01-02", e.Retrieved); !datePattern.MatchString(e.Retrieved) || err != nil {
		out = append(out, "retrieved must be a YYYY-MM-DD date")
	} else if d.After(now) {
		out = append(out, "retrieved is in the future")
	}
	switch e.Scale {
	case "percent":
		if e.Score < 0 || e.Score > 100 {
			out = append(out, "a percent score is between 0 and 100")
		}
	case "fraction":
		if e.Score < 0 || e.Score > 1 {
			out = append(out, "a fraction score is between 0 and 1")
		}
	case "elo", "index":
	default:
		out = append(out, "scale must be percent, fraction, elo or index")
	}
	if len(e.Interval) != 0 && (len(e.Interval) != 2 || e.Interval[0] > e.Interval[1]) {
		out = append(out, "interval is [low, high]")
	}
	return out
}

func (e Evidence) fraction() (float64, bool) {
	switch e.Scale {
	case "percent":
		return e.Score / 100, true
	case "fraction":
		return e.Score, true
	}
	return 0, false
}

func (e Evidence) names(model string) bool {
	if e.Model == model {
		return true
	}
	for _, a := range e.Aliases {
		if a == model {
			return true
		}
	}
	return false
}

// FamilyPolicy is a family's equivalence margin, required level and stakes.
type FamilyPolicy struct {
	Delta  float64 `json:"delta,omitempty"`
	Level  float64 `json:"level,omitempty"`
	Stakes string  `json:"stakes,omitempty"` // low | high
}

// Policy is families.json: margins and levels per family, and how much each public
// benchmark speaks to each family (a reviewed configuration, never a constant in code).
type Policy struct {
	Delta      float64                       `json:"delta,omitempty"`
	Families   map[string]FamilyPolicy       `json:"families,omitempty"`
	Benchmarks map[string]map[string]float64 `json:"benchmarks,omitempty"`
}

// DefaultDelta is the equivalence margin when families.json sets none: five points.
const DefaultDelta = 0.05

// DeltaFor is the margin for family.
func (p Policy) DeltaFor(family string) float64 {
	if d := p.Families[family].Delta; d > 0 {
		return d
	}
	if p.Delta > 0 {
		return p.Delta
	}
	return DefaultDelta
}

// Registry is a directory: families.json, evidence/*.json, calibration/*.json.
type Registry struct {
	Dir          string
	Policy       Policy
	Evidence     []Evidence
	Calibrations []Calibration
}

// Open reads the registry at dir; a missing directory is an empty registry.
func Open(dir string) (*Registry, error) {
	r := &Registry{Dir: dir}
	if data, err := os.ReadFile(filepath.Join(dir, "families.json")); err == nil {
		if err := json.Unmarshal(data, &r.Policy); err != nil {
			return nil, fmt.Errorf("families.json: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := readAll(filepath.Join(dir, "evidence"), func(data []byte, name string) error {
		var e Evidence
		if err := json.Unmarshal(data, &e); err != nil {
			return fmt.Errorf("evidence/%s: %w", name, err)
		}
		r.Evidence = append(r.Evidence, e)
		return nil
	}); err != nil {
		return nil, err
	}
	if err := readAll(filepath.Join(dir, "calibration"), func(data []byte, name string) error {
		var c Calibration
		if err := json.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("calibration/%s: %w", name, err)
		}
		r.Calibrations = append(r.Calibrations, c)
		return nil
	}); err != nil {
		return nil, err
	}
	return r, nil
}

func readAll(dir string, each func([]byte, string) error) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.IsDir() || filepath.Ext(ent.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			return err
		}
		if err := each(data, ent.Name()); err != nil {
			return err
		}
	}
	return nil
}

// writeOnce creates path with data, refusing to replace anything.
func writeOnce(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// AddCalibration writes c, write-once, and returns its path.
func (r *Registry) AddCalibration(c Calibration) (string, error) {
	c.Schema = CalibrationSchema
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	stamp := strings.NewReplacer(":", "", "-", "").Replace(c.FinishedAt)
	name := unsafeName.ReplaceAllString(fmt.Sprintf("%s--%s--%s--%s", c.Family, c.Suite, c.Target, stamp), "_") + ".json"
	path := filepath.Join(r.Dir, "calibration", name)
	if err := writeOnce(path, append(data, '\n')); err != nil {
		return "", err
	}
	r.Calibrations = append(r.Calibrations, c)
	return path, nil
}

// ImportEvidence validates every record first and writes none unless all are valid. Each
// record is stored under its content hash, so importing the same record twice is a no-op.
func (r *Registry) ImportEvidence(records []Evidence, now time.Time) (added int, err error) {
	for i, e := range records {
		if problems := e.Validate(now); len(problems) > 0 {
			return 0, fmt.Errorf("record %d (%s / %s): %s", i, e.Model, e.Benchmark, strings.Join(problems, "; "))
		}
	}
	for _, e := range records {
		data, _ := json.MarshalIndent(e, "", "  ")
		sum := sha256.Sum256(data)
		path := filepath.Join(r.Dir, "evidence", hex.EncodeToString(sum[:8])+".json")
		if err := writeOnce(path, append(data, '\n')); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return added, err
		}
		r.Evidence = append(r.Evidence, e)
		added++
	}
	return added, nil
}

// Row is one model's standing on one family.
type Row struct {
	Target  string   `json:"target"`
	Model   string   `json:"model"`
	Graded  int      `json:"graded"`
	Passed  int      `json:"passed"`
	Rate    float64  `json:"pass_rate"`
	Low     float64  `json:"wilson95_low"`
	High    float64  `json:"wilson95_high"`
	CostPer *float64 `json:"cost_per_accepted_usd,omitempty"`
	Suites  []string `json:"suites"`
	// Label is calibrated or prior-only; a prior-only model has public evidence and no run.
	Label   string   `json:"label"`
	Prior   *float64 `json:"prior,omitempty"`
	Verdict Verdict  `json:"vs_reference,omitempty"`
	// MeetsLevel is set when the family has a level: the Wilson low bound is at or above it.
	MeetsLevel *bool `json:"meets_level,omitempty"`
}

// Table is a family's rows, the reference row first (the highest Wilson low bound), then by
// that bound; prior-only rows come last.
type Table struct {
	Family    string  `json:"family"`
	Delta     float64 `json:"delta"`
	Level     float64 `json:"level,omitempty"`
	Reference string  `json:"reference,omitempty"`
	Rows      []Row   `json:"rows"`
}

// Families lists every family that has a calibration or a benchmark mapping.
func (r *Registry) Families() []string {
	set := map[string]bool{}
	for _, c := range r.Calibrations {
		set[c.Family] = true
	}
	for _, fams := range r.Policy.Benchmarks {
		for f := range fams {
			set[f] = true
		}
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Table builds family's table. For each target it pools the latest run of every suite of
// the family (a changed suite is a new suite); older runs of the same suite are superseded.
func (r *Registry) Table(family string) Table {
	t := Table{Family: family, Delta: r.Policy.DeltaFor(family), Level: r.Policy.Families[family].Level}
	type key struct{ target, suite string }
	latest := map[key]Calibration{}
	for _, c := range r.Calibrations {
		if c.Family != family {
			continue
		}
		k := key{c.Target.String(), c.SuiteSHA256}
		if prev, ok := latest[k]; !ok || c.FinishedAt > prev.FinishedAt {
			latest[k] = c
		}
	}
	rows := map[string]*Row{}
	costKnown := map[string]bool{}
	cost := map[string]float64{}
	for k, c := range latest {
		row := rows[k.target]
		if row == nil {
			row = &Row{Target: k.target, Model: c.Target.Model, Label: "calibrated"}
			rows[k.target], costKnown[k.target] = row, true
		}
		row.Graded += c.Graded
		row.Passed += c.Passed
		row.Suites = append(row.Suites, c.Suite)
		if c.CostUSD == nil {
			costKnown[k.target] = false
		} else {
			cost[k.target] += *c.CostUSD
		}
	}
	var calibrated []*Row
	for name, row := range rows {
		sort.Strings(row.Suites)
		if row.Graded > 0 {
			row.Rate = float64(row.Passed) / float64(row.Graded)
		}
		row.Low, row.High = Wilson(row.Passed, row.Graded)
		if costKnown[name] && row.Passed > 0 {
			c := cost[name] / float64(row.Passed)
			row.CostPer = &c
		}
		row.Prior = r.prior(row.Model, family)
		if t.Level > 0 {
			ok := row.Low >= t.Level
			row.MeetsLevel = &ok
		}
		calibrated = append(calibrated, row)
	}
	sort.Slice(calibrated, func(i, j int) bool {
		if calibrated[i].Low != calibrated[j].Low {
			return calibrated[i].Low > calibrated[j].Low
		}
		return calibrated[i].Target < calibrated[j].Target
	})
	if len(calibrated) > 0 {
		ref := calibrated[0]
		t.Reference = ref.Target
		for _, row := range calibrated[1:] {
			row.Verdict = Compare(row.Passed, row.Graded, ref.Passed, ref.Graded, t.Delta)
		}
	}
	for _, row := range calibrated {
		t.Rows = append(t.Rows, *row)
	}
	// Models with public evidence for this family and no calibration of their own.
	seen := map[string]bool{}
	for _, row := range calibrated {
		seen[row.Model] = true
	}
	var priorOnly []Row
	for _, e := range r.Evidence {
		if seen[e.Model] {
			continue
		}
		if p := r.prior(e.Model, family); p != nil {
			seen[e.Model] = true
			priorOnly = append(priorOnly, Row{Target: e.Model, Model: e.Model, Label: "prior-only", Prior: p})
		}
	}
	sort.Slice(priorOnly, func(i, j int) bool { return *priorOnly[i].Prior > *priorOnly[j].Prior })
	t.Rows = append(t.Rows, priorOnly...)
	return t
}

// prior is the weighted mean, over this family's mapped benchmarks, of the model's
// percent/fraction scores (the latest retrieval per benchmark), or nil without evidence.
func (r *Registry) prior(model, family string) *float64 {
	best := map[string]Evidence{}
	for _, e := range r.Evidence {
		if !e.names(model) || r.Policy.Benchmarks[e.Benchmark][family] <= 0 {
			continue
		}
		if _, ok := e.fraction(); !ok {
			continue
		}
		if prev, ok := best[e.Benchmark]; !ok || e.Retrieved > prev.Retrieved {
			best[e.Benchmark] = e
		}
	}
	var sum, weight float64
	for b, e := range best {
		w := r.Policy.Benchmarks[b][family]
		f, _ := e.fraction()
		sum, weight = sum+w*f, weight+w
	}
	if weight == 0 {
		return nil
	}
	p := sum / weight
	return &p
}
