package quota

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Codex records its rate-limit state in the session rollout it already writes: every
// token_count event (`event_msg` with payload type "token_count") carries a rate_limits
// object beside the token totals internal/usage reads —
//
//	{"limit_id":"codex","primary":{"used_percent":43.0,"window_minutes":300,"resets_at":1790713159},
//	 "secondary":{"used_percent":12.0,"window_minutes":10080,"resets_at":1791100000},
//	 "plan_type":"plus","rate_limit_reached_type":null}
//
// (RateLimitSnapshot in codex-rs/protocol). Three things about it are easy to get wrong and
// each has bitten a community tool: the slot does not say which window it is — a Pro plan's
// only window is weekly and arrives as "primary" — so windows are named by window_minutes;
// builds before resets_at wrote resets_in_seconds, relative to the event; and the newest
// file is not always the newest reading, because Codex rewrites old rollouts on resume, so
// readings are ordered by their own timestamps.

type codexRateRecord struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type       string           `json:"type"`
		RateLimits *codexRateLimits `json:"rate_limits"`
	} `json:"payload"`
}

type codexRateLimits struct {
	LimitID              string           `json:"limit_id"`
	Primary              *codexRateWindow `json:"primary"`
	Secondary            *codexRateWindow `json:"secondary"`
	PlanType             string           `json:"plan_type"`
	RateLimitReachedType *string          `json:"rate_limit_reached_type"`
}

type codexRateWindow struct {
	UsedPercent     *float64        `json:"used_percent"`
	WindowMinutes   *int            `json:"window_minutes"`
	ResetsAt        json.RawMessage `json:"resets_at"`
	ResetsInSeconds *float64        `json:"resets_in_seconds"`
}

// CodexSnapshots returns the latest reading of every window found in a rollout. account
// labels the login; the caller fills in Machine.
func CodexSnapshots(r io.Reader, account string) []Snapshot {
	latest := map[string]Snapshot{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		// Most lines are conversation; skip them without decoding a byte of their content.
		if !bytes.Contains(line, []byte(`"rate_limits"`)) {
			continue
		}
		var rec codexRateRecord
		if json.Unmarshal(line, &rec) != nil || rec.Type != "event_msg" ||
			rec.Payload.Type != "token_count" || rec.Payload.RateLimits == nil {
			continue
		}
		for _, s := range codexWindows(rec.Timestamp, rec.Payload.RateLimits, account) {
			if cur, ok := latest[s.Window]; !ok || !s.ObservedAt.Before(cur.ObservedAt) {
				latest[s.Window] = s
			}
		}
	}
	out := make([]Snapshot, 0, len(latest))
	for _, s := range latest {
		out = append(out, s)
	}
	Sort(out)
	return out
}

func codexWindows(at time.Time, rl *codexRateLimits, account string) []Snapshot {
	var out []Snapshot
	reached := rl.RateLimitReachedType != nil && *rl.RateLimitReachedType != ""
	prefix := ""
	if id := strings.TrimSpace(rl.LimitID); id != "" && id != "codex" {
		prefix = id + ":"
	}
	for slot, w := range map[string]*codexRateWindow{"primary": rl.Primary, "secondary": rl.Secondary} {
		if w == nil || w.UsedPercent == nil {
			continue
		}
		minutes := 0
		if w.WindowMinutes != nil {
			minutes = *w.WindowMinutes
		}
		name := WindowName(minutes)
		if minutes <= 0 {
			name = slot // nothing better to go on
		}
		s := Snapshot{
			Harness: "codex", Account: account, Window: prefix + name, WindowMinutes: minutes,
			UsedPercent: Float(*w.UsedPercent), ResetsAt: codexResetsAt(at, w), Plan: rl.PlanType,
			Source: "codex-rollout", SourceKind: KindLocalFile, ObservedAt: at.UTC(),
		}
		// rate_limit_reached_type does not say which window ran out; the one at or past
		// 100% did, and if neither reads that high the snapshot is stale in the other
		// direction, so both are marked.
		if reached && (*w.UsedPercent >= 100 || !codexAnyFull(rl)) {
			s.LimitReached = true
		}
		out = append(out, s)
	}
	return out
}

func codexAnyFull(rl *codexRateLimits) bool {
	for _, w := range []*codexRateWindow{rl.Primary, rl.Secondary} {
		if w != nil && w.UsedPercent != nil && *w.UsedPercent >= 100 {
			return true
		}
	}
	return false
}

// codexResetsAt accepts every encoding Codex has used: Unix seconds (current), an RFC 3339
// string, or seconds relative to the event (older builds).
func codexResetsAt(at time.Time, w *codexRateWindow) *time.Time {
	raw := bytes.TrimSpace(w.ResetsAt)
	if len(raw) > 0 && string(raw) != "null" {
		var n float64
		if json.Unmarshal(raw, &n) == nil && n > 0 {
			return Time(time.Unix(int64(n), 0))
		}
		var s string
		if json.Unmarshal(raw, &s) == nil {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return Time(t)
			}
			if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
				return Time(time.Unix(int64(n), 0))
			}
		}
	}
	if w.ResetsInSeconds != nil && *w.ResetsInSeconds >= 0 && !at.IsZero() {
		return Time(at.Add(time.Duration(*w.ResetsInSeconds * float64(time.Second))).Truncate(time.Second))
	}
	return nil
}

// Collection bounds for reading rollouts on every sidecar tick: the newest few files, and
// only the tail of each, which is where the latest token_count event is.
const (
	codexMaxFiles = 8
	codexTailSize = 2 << 20
	codexMaxAge   = 8 * 24 * time.Hour // a weekly window's last reading can be this old
)

// ReadCodex returns the latest reading of every window of the login in codexHome.
func ReadCodex(codexHome, account string, now time.Time) []Snapshot {
	files := codexRecentRollouts(filepath.Join(codexHome, "sessions"), now.Add(-codexMaxAge))
	var all []Snapshot
	for _, path := range files {
		all = append(all, readTail(path, func(r io.Reader) []Snapshot { return CodexSnapshots(r, account) })...)
	}
	for i := range all {
		all[i].StateDir = codexHome
	}
	return Latest(all)
}

func codexRecentRollouts(root string, since time.Time) []string {
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable or missing: no reading, not an error
		}
		if d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if info, err := d.Info(); err == nil && !info.ModTime().Before(since) {
			files = append(files, file{path, info.ModTime()})
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	if len(files) > codexMaxFiles {
		files = files[:codexMaxFiles]
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	return out
}

// readTail parses the last codexTailSize bytes of a file, dropping the partial first line.
func readTail(path string, parse func(io.Reader) []Snapshot) []Snapshot {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	if info.Size() > codexTailSize {
		if _, err := f.Seek(info.Size()-codexTailSize, io.SeekStart); err != nil {
			return nil
		}
		br := bufio.NewReader(f)
		if _, err := br.ReadBytes('\n'); err != nil {
			return nil
		}
		return parse(br)
	}
	return parse(f)
}
