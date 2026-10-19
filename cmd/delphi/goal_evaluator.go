package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// goalEvaluation records a tool check, not a proof of the draft's claims.
type goalEvaluation struct {
	Passed    bool     `json:"passed"`
	Feedback  string   `json:"feedback"`
	Score     *float64 `json:"score,omitempty"`
	DraftHash string   `json:"draft_sha256"`
}

type boundedOutput struct {
	buf bytes.Buffer
	max int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.max-b.buf.Len() {
		return 0, errors.New("evaluator output exceeds limit")
	}
	return b.buf.Write(p)
}

func evaluationFeedback(e *goalEvaluation) string {
	if e == nil {
		return "No external evaluator configured."
	}
	if e.Score != nil {
		return fmt.Sprintf("passed=%t score=%g feedback=%s", e.Passed, *e.Score, e.Feedback)
	}
	return fmt.Sprintf("passed=%t feedback=%s", e.Passed, e.Feedback)
}

func runGoalEvaluator(ctx context.Context, dir string, s goalState) (goalEvaluation, error) {
	var result goalEvaluation
	if s.Evaluator == "" || s.EvalTimeoutSecs <= 0 || s.EvalTimeoutSecs > 600 {
		return result, errors.New("invalid evaluator configuration")
	}
	hash := sha256.Sum256([]byte(s.Draft))
	input, err := json.Marshal(struct {
		Goal      string `json:"goal"`
		Round     int    `json:"round"`
		Plan      string `json:"plan"`
		Draft     string `json:"draft"`
		DraftHash string `json:"draft_sha256"`
	}{s.Goal, s.Round, s.Plan, s.Draft, hex.EncodeToString(hash[:])})
	if err != nil {
		return result, err
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(s.EvalTimeoutSecs)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(callCtx, s.Evaluator)
	// Evaluators often launch a test runner. Cancel the whole process group so
	// descendants cannot keep running or hold the output pipes open past timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	cmd.Dir = dir
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	stdout := &boundedOutput{max: 1 << 20}
	stderr := &boundedOutput{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if callCtx.Err() != nil {
			return result, fmt.Errorf("evaluator timed out or was interrupted: %w", callCtx.Err())
		}
		return result, fmt.Errorf("evaluator failed: %w: %s", err, strings.TrimSpace(stderr.buf.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.buf.Bytes()))
	decoder.DisallowUnknownFields()
	var output struct {
		Passed   *bool    `json:"passed"`
		Feedback string   `json:"feedback"`
		Score    *float64 `json:"score"`
	}
	if err := decoder.Decode(&output); err != nil {
		return result, fmt.Errorf("invalid evaluator JSON: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, errors.New("evaluator must emit exactly one JSON object")
	}
	if output.Passed == nil || (!*output.Passed && strings.TrimSpace(output.Feedback) == "") {
		return result, errors.New("evaluator must provide passed and feedback for a failed check")
	}
	if output.Score != nil && (math.IsNaN(*output.Score) || math.IsInf(*output.Score, 0) || *output.Score < 0 || *output.Score > 1) {
		return result, errors.New("evaluator score must be between 0 and 1")
	}
	return goalEvaluation{Passed: *output.Passed, Feedback: output.Feedback, Score: output.Score, DraftHash: hex.EncodeToString(hash[:])}, nil
}
