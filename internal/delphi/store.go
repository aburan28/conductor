package delphi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Flight states.
const (
	// FlightSubmitting is recorded before the provider call. A flight left in
	// this state means the call's outcome is unknown: the batch may exist.
	FlightSubmitting = "submitting"
	FlightRunning    = "running"
)

// Flight is one submitted (or being submitted) provider batch.
type Flight struct {
	Label       string      `json:"label"`
	Provider    string      `json:"provider"`
	BatchID     string      `json:"batch_id,omitempty"`
	State       string      `json:"state"`
	SubmittedAt time.Time   `json:"submitted_at"`
	Status      BatchStatus `json:"status"`
	Requests    []Request   `json:"requests"`
}

// Queued is a pending request with its enqueue time.
type Queued struct {
	Request
	EnqueuedAt time.Time `json:"enqueued_at"`
	Hash       string    `json:"hash"`
}

// Store persists the queue, the flights and the results. Implementations must
// keep a request in exactly one of queue, flight and results once each call
// returns; between calls a crash may leave it in two places, and readers
// resolve that in the direction that never submits a request twice.
type Store interface {
	// Enqueue adds requests. Re-adding an identical request is a no-op; an id
	// already used by a different request is an error.
	Enqueue(reqs []Request, now time.Time) (added int, err error)
	// Requeue puts a request back for another attempt, bypassing the id check.
	Requeue(r Request, now time.Time) error
	Pending() ([]Queued, error)
	Flights() ([]Flight, error)
	// BeginFlight records f and takes its requests off the queue.
	BeginFlight(f Flight) error
	UpdateFlight(f Flight) error
	// AbortFlight returns f's requests to the queue and forgets f; only for a
	// submission known not to have created a batch.
	AbortFlight(f Flight) error
	// FinishFlight stores results, requeues retries and forgets f.
	FinishFlight(f Flight, results []Result, retries []Request, now time.Time) error
	Result(id string) (Result, bool, error)
	Results() ([]Result, error)
	Event(kind string, fields map[string]any)
}

// FileStore keeps state under one directory: queue/, flights/, done/ and an
// append-only events.jsonl. Writes are atomic renames, so producers can
// enqueue while a loop runs.
type FileStore struct{ Dir string }

// OpenFileStore creates the layout if needed.
func OpenFileStore(dir string) (*FileStore, error) {
	for _, sub := range []string{"queue", "flights", "done"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &FileStore{Dir: dir}, nil
}

func (s *FileStore) path(parts ...string) string {
	return filepath.Join(append([]string{s.Dir}, parts...)...)
}

func writeAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func listJSON(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Hash identifies a request's content, ignoring the attempt counter.
func Hash(r Request) string {
	r.Attempt = 0
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (s *FileStore) Enqueue(reqs []Request, now time.Time) (int, error) {
	inFlight := map[string]string{}
	flights, err := s.Flights()
	if err != nil {
		return 0, err
	}
	for _, f := range flights {
		for _, r := range f.Requests {
			inFlight[r.ID] = Hash(r)
		}
	}
	existing := func(id string) (string, bool) {
		var q Queued
		if readJSON(s.path("queue", id+".json"), &q) == nil {
			return q.Hash, true
		}
		if h, ok := inFlight[id]; ok {
			return h, true
		}
		var res Result
		if readJSON(s.path("done", id+".json"), &res) == nil {
			return res.Hash, true
		}
		return "", false
	}
	added := 0
	for _, r := range reqs {
		h := Hash(r)
		if old, ok := existing(r.ID); ok {
			if old == h {
				continue
			}
			return added, fmt.Errorf("id %q is already used by a different request", r.ID)
		}
		if err := writeAtomic(s.path("queue", r.ID+".json"), Queued{Request: r, EnqueuedAt: now, Hash: h}); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

func (s *FileStore) Requeue(r Request, now time.Time) error {
	return writeAtomic(s.path("queue", r.ID+".json"), Queued{Request: r, EnqueuedAt: now, Hash: Hash(r)})
}

func (s *FileStore) Pending() ([]Queued, error) {
	flights, err := s.Flights()
	if err != nil {
		return nil, err
	}
	// A request both queued and in a flight means a crash inside BeginFlight:
	// the flight wins, so it is never submitted twice.
	inFlight := map[string]bool{}
	for _, f := range flights {
		for _, r := range f.Requests {
			inFlight[r.ID] = true
		}
	}
	files, err := listJSON(s.path("queue"))
	if err != nil {
		return nil, err
	}
	var out []Queued
	for _, p := range files {
		var q Queued
		if err := readJSON(p, &q); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if !inFlight[q.ID] {
			out = append(out, q)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EnqueuedAt.Before(out[j].EnqueuedAt) })
	return out, nil
}

func (s *FileStore) Flights() ([]Flight, error) {
	files, err := listJSON(s.path("flights"))
	if err != nil {
		return nil, err
	}
	var out []Flight
	for _, p := range files {
		var f Flight
		if err := readJSON(p, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *FileStore) BeginFlight(f Flight) error {
	if err := writeAtomic(s.path("flights", f.Label+".json"), f); err != nil {
		return err
	}
	for _, r := range f.Requests {
		if err := os.Remove(s.path("queue", r.ID+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *FileStore) UpdateFlight(f Flight) error {
	return writeAtomic(s.path("flights", f.Label+".json"), f)
}

func (s *FileStore) AbortFlight(f Flight) error {
	for _, r := range f.Requests {
		if err := s.Requeue(r, f.SubmittedAt); err != nil {
			return err
		}
	}
	return os.Remove(s.path("flights", f.Label+".json"))
}

func (s *FileStore) FinishFlight(f Flight, results []Result, retries []Request, now time.Time) error {
	for _, r := range results {
		if err := writeAtomic(s.path("done", r.ID+".json"), r); err != nil {
			return err
		}
	}
	for _, r := range retries {
		if err := s.Requeue(r, now); err != nil {
			return err
		}
	}
	return os.Remove(s.path("flights", f.Label+".json"))
}

func (s *FileStore) Result(id string) (Result, bool, error) {
	var r Result
	err := readJSON(s.path("done", id+".json"), &r)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	return r, err == nil, err
}

func (s *FileStore) Results() ([]Result, error) {
	files, err := listJSON(s.path("done"))
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(files))
	for _, p := range files {
		var r Result
		if err := readJSON(p, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ResultPath is where a finished result is stored.
func (s *FileStore) ResultPath(id string) string { return s.path("done", id+".json") }

func (s *FileStore) Event(kind string, fields map[string]any) {
	rec := map[string]any{"time": time.Now().UTC().Format(time.RFC3339), "event": kind}
	for k, v := range fields {
		rec[k] = v
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.path("events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// Lock takes the directory's single-loop lock. It returns an unlock function,
// or an error naming the process that holds it.
func (s *FileStore) Lock() (func(), error) {
	p := s.path("loop.lock")
	for i := 0; i < 2; i++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(p) }, nil
		}
		var pid int
		if b, rerr := os.ReadFile(p); rerr == nil {
			fmt.Sscanf(string(b), "%d", &pid)
		}
		if pid > 0 && processAlive(pid) {
			return nil, fmt.Errorf("another delphi loop (pid %d) is running on %s", pid, s.Dir)
		}
		os.Remove(p) // stale lock from a dead process
	}
	return nil, fmt.Errorf("could not take %s", p)
}
