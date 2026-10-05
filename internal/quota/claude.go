package quota

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Claude Code has one documented source and one fallback.
//
// The documented source is the status line: Claude Code runs the configured statusLine
// command with a JSON payload on stdin after every assistant message, and for Pro and Max
// subscribers that payload carries
//
//	"rate_limits": {"five_hour": {"used_percentage": 23.5, "resets_at": 1738425600},
//	                "seven_day": {"used_percentage": 41.2, "resets_at": 1738857600}}
//
// (plus "spend_limit" behind a Claude apps gateway). `conductor quota statusline` is a shim
// that records those fields and then runs the user's own status line command unchanged.
//
// The fallback is the transcript. When a limit lands, Claude Code writes a synthetic
// assistant record — isApiErrorMessage true, error "rate_limit", model "<synthetic>" — whose
// text says which limit and when it resets: "You've hit your weekly limit · resets Jun 3 at
// 4pm (Europe/Berlin)", or in older builds "Claude AI usage limit reached|1749924000". Only
// records carrying those markers are looked at; the conversation itself is skipped without
// being decoded.

// statusPayload is the subset of the status line payload this package reads. There are no
// fields for the transcript path, the working directory, or the session name, so they
// cannot be decoded even by accident.
type statusPayload struct {
	RateLimits map[string]*statusWindow `json:"rate_limits"`
}

type statusWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       *float64 `json:"resets_at"`
	UsedUSD        *float64 `json:"used_usd"`
	LimitUSD       *float64 `json:"limit_usd"`
	Period         string   `json:"period"`
}

// StatuslineSnapshots extracts the rate-limit windows from a status line payload. A payload
// without them (an API-key login, or before the first response) yields nothing.
func StatuslineSnapshots(payload []byte, account string, now time.Time) []Snapshot {
	var p statusPayload
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var out []Snapshot
	for name, w := range p.RateLimits {
		if w == nil || (w.UsedPercentage == nil && w.UsedUSD == nil) {
			continue
		}
		s := Snapshot{
			Harness: "claude", Account: account, UsedPercent: w.UsedPercentage,
			Source: "claude-statusline", SourceKind: KindDocumented, ObservedAt: now.UTC(),
		}
		switch name {
		case "five_hour":
			s.Window, s.WindowMinutes = Window5h, 300
		case "seven_day":
			s.Window, s.WindowMinutes = WindowWeekly, 10080
		case "spend_limit":
			s.Window = WindowSpend
			if w.Period != "" {
				s.Window = WindowSpend + ":" + w.Period
			}
			if w.UsedUSD != nil && w.LimitUSD != nil {
				s.Used, s.Limit, s.Unit = w.UsedUSD, w.LimitUSD, "usd"
			}
		default:
			// A window a later version adds (a per-model weekly limit, say) is kept under
			// its own name rather than dropped.
			s.Window = name
		}
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			s.ResetsAt = Time(time.Unix(int64(*w.ResetsAt), 0))
		}
		out = append(out, s)
	}
	Sort(out)
	return out
}

// claudeLimitRecord is the subset of a transcript line that marks a hit limit. The message
// text is decoded only for records already identified as synthetic API errors.
type claudeLimitRecord struct {
	Type              string    `json:"type"`
	Timestamp         time.Time `json:"timestamp"`
	IsAPIErrorMessage bool      `json:"isApiErrorMessage"`
	Error             string    `json:"error"`
	Message           struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

var (
	claudeLegacyLimit = regexp.MustCompile(`(?i)usage limit reached\|(\d{9,11})`)
	claudeLimitText   = regexp.MustCompile(`(?i)(hit your (?:[a-z0-9 ]+ )?limit|usage limit reached|limit reached)`)
	claudeResetsText  = regexp.MustCompile(`(?i)resets\s+(?:at\s+)?(.+?)\s*(?:\(([^)]+)\))?\s*\.?\s*$`)
)

// ClaudeLimitSnapshots returns one exhausted snapshot per window for every "limit reached"
// record in a transcript, the latest per window.
func ClaudeLimitSnapshots(r io.Reader, account string) []Snapshot {
	latest := map[string]Snapshot{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"isApiErrorMessage":true`)) &&
			!bytes.Contains(line, []byte(`"isApiErrorMessage": true`)) {
			continue
		}
		var rec claudeLimitRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || !rec.IsAPIErrorMessage {
			continue
		}
		if rec.Error != "" && rec.Error != "rate_limit" {
			continue // an overload or an auth failure is not a usage limit
		}
		text := syntheticText(rec.Message.Content)
		s, ok := ParseClaudeLimitText(text, rec.Timestamp)
		if !ok {
			continue
		}
		s.Account = account
		if cur, ok := latest[s.Window]; !ok || !s.ObservedAt.Before(cur.ObservedAt) {
			latest[s.Window] = s
		}
	}
	out := make([]Snapshot, 0, len(latest))
	for _, s := range latest {
		out = append(out, s)
	}
	Sort(out)
	return out
}

func syntheticText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// ParseClaudeLimitText reads a Claude Code limit message: which window ran out and when it
// resets. at is when the message was written; relative times ("resets 3pm") are resolved
// against it. A message that is not about a usage limit returns ok=false.
func ParseClaudeLimitText(text string, at time.Time) (Snapshot, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len(text) > 400 {
		return Snapshot{}, false
	}
	s := Snapshot{
		Harness: "claude", LimitReached: true, UsedPercent: Float(100),
		Source: "claude-transcript", SourceKind: KindLocalFile, ObservedAt: at.UTC(),
	}
	if m := claudeLegacyLimit.FindStringSubmatch(text); m != nil {
		n, _ := strconv.ParseInt(m[1], 10, 64)
		s.Window, s.WindowMinutes = Window5h, 300
		s.ResetsAt = Time(time.Unix(n, 0))
		return s, true
	}
	if !claudeLimitText.MatchString(text) {
		return Snapshot{}, false
	}
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "weekly"):
		s.Window, s.WindowMinutes = WindowWeekly, 10080
		for _, model := range []string{"opus", "sonnet"} {
			if strings.Contains(lower, model) {
				s.Window = model + ":" + WindowWeekly
			}
		}
	default:
		// "your session limit" and the bare "your limit" are the five-hour window.
		s.Window, s.WindowMinutes = Window5h, 300
	}
	if i := strings.LastIndex(lower, "resets"); i >= 0 {
		if m := claudeResetsText.FindStringSubmatch(text[i:]); m != nil {
			if t, ok := parseResetPhrase(m[1], m[2], at); ok {
				s.ResetsAt = Time(t)
			}
		}
	}
	return s, true
}

var (
	resetClock   = regexp.MustCompile(`(?i)^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)
	resetWeekday = map[string]time.Weekday{
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
		"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
	}
	resetMonth = map[string]time.Month{
		"jan": time.January, "feb": time.February, "mar": time.March, "apr": time.April,
		"may": time.May, "jun": time.June, "jul": time.July, "aug": time.August,
		"sep": time.September, "oct": time.October, "nov": time.November, "dec": time.December,
	}
)

// parseResetPhrase resolves "3pm", "2:40am", "Mon 12:00am", or "Jun 3 at 4pm" in the named
// zone to the first such moment at or after at.
func parseResetPhrase(phrase, zone string, at time.Time) (time.Time, bool) {
	loc := time.Local
	if zone = strings.TrimSpace(zone); zone != "" {
		if l, err := time.LoadLocation(zone); err == nil {
			loc = l
		}
	}
	if at.IsZero() {
		at = time.Now()
	}
	ref := at.In(loc)
	fields := strings.Fields(strings.ReplaceAll(strings.ToLower(phrase), ",", " "))
	var (
		month   time.Month
		day     int
		weekday = time.Weekday(-1)
		clock   string
	)
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		switch {
		case f == "at" || f == "on":
		case len(f) >= 3 && resetMonth[f[:3]] != 0 && i+1 < len(fields):
			month = resetMonth[f[:3]]
			d, err := strconv.Atoi(strings.TrimRight(fields[i+1], "stndrh"))
			if err != nil {
				return time.Time{}, false
			}
			day = d
			i++
		case len(f) >= 3 && isWeekday(f[:3]):
			weekday = resetWeekday[f[:3]]
		default:
			clock += f
		}
	}
	m := resetClock.FindStringSubmatch(clock)
	if m == nil {
		return time.Time{}, false
	}
	hour, _ := strconv.Atoi(m[1])
	minute := 0
	if m[2] != "" {
		minute, _ = strconv.Atoi(m[2])
	}
	switch strings.ToLower(m[3]) {
	case "pm":
		if hour < 12 {
			hour += 12
		}
	case "am":
		if hour == 12 {
			hour = 0
		}
	}
	if hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	switch {
	case month != 0:
		t := time.Date(ref.Year(), month, day, hour, minute, 0, 0, loc)
		if t.Before(ref) {
			t = t.AddDate(1, 0, 0)
		}
		return t, true
	case weekday >= 0:
		t := time.Date(ref.Year(), ref.Month(), ref.Day(), hour, minute, 0, 0, loc)
		for t.Weekday() != weekday || t.Before(ref) {
			t = t.AddDate(0, 0, 1)
		}
		return t, true
	}
	t := time.Date(ref.Year(), ref.Month(), ref.Day(), hour, minute, 0, 0, loc)
	if t.Before(ref) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

func isWeekday(s string) bool { _, ok := resetWeekday[s]; return ok }

// Transcript scan bounds: limit messages only matter while their window is open, so the
// newest few transcripts across every project, and only their tails, are enough.
const (
	claudeMaxFiles = 24
	claudeMaxAge   = 8 * 24 * time.Hour
)

// ReadClaudeTranscripts returns the limit-reached readings from the newest transcripts of the
// login in configDir. Readings whose window has already reset are dropped.
func ReadClaudeTranscripts(configDir, account string, now time.Time) []Snapshot {
	root := filepath.Join(configDir, "projects")
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	since := now.Add(-claudeMaxAge)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Subagent transcripts live a level down and never carry the session's limit.
			if path != root && filepath.Dir(path) != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && !info.ModTime().Before(since) {
			files = append(files, file{path, info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > claudeMaxFiles {
		files = files[:claudeMaxFiles]
	}
	var all []Snapshot
	for _, f := range files {
		all = append(all, readTail(f.path, func(r io.Reader) []Snapshot { return ClaudeLimitSnapshots(r, account) })...)
	}
	var out []Snapshot
	for _, s := range Latest(all) {
		// A limit whose reset has passed — or, when the message gave no reset time, one
		// older than its window — no longer says anything about now.
		if s.Reset(now) || (s.ResetsAt == nil && now.Sub(s.ObservedAt) > s.Duration()) {
			continue
		}
		s.StateDir = configDir
		out = append(out, s)
	}
	return out
}
