package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// zstdAvailable reports whether the optional zstd tool is installed. Compressed
// .jsonl.zst sessions are copied verbatim without it; their metadata and text cannot be
// read, so --project filtering and the secret scan cannot see inside them.
func zstdAvailable() bool {
	_, err := exec.LookPath("zstd")
	return err == nil
}

// zstdReader streams the decompressed contents of a .jsonl.zst file. The caller must
// Close it, which waits for the tool to exit.
func zstdReader(path string) (io.ReadCloser, error) {
	cmd := exec.Command("zstd", "-dc", "--", path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &zstdStream{out: stdout, cmd: cmd, stderr: &stderr}, nil
}

type zstdStream struct {
	out    io.ReadCloser
	cmd    *exec.Cmd
	stderr *bytes.Buffer
}

func (z *zstdStream) Read(p []byte) (int, error) { return z.out.Read(p) }

func (z *zstdStream) Close() error {
	err := z.out.Close()
	waitErr := z.cmd.Wait()
	if waitErr != nil {
		if msg := strings.TrimSpace(z.stderr.String()); msg != "" {
			return fmt.Errorf("zstd: %s", msg)
		}
		return waitErr
	}
	return err
}

// decompressZstd reads a compressed session, capped at max bytes.
func decompressZstd(path string, max int64) ([]byte, error) {
	rc, err := zstdReader(path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readCapped(rc, max, path)
}

// decompressZstdBytes decompresses an in-memory frame, which is how a compressed session
// is compared with a local copy before deciding whether it only grew.
func decompressZstdBytes(data []byte) ([]byte, error) {
	cmd := exec.Command("zstd", "-dc")
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("zstd: %s", msg)
		}
		return nil, err
	}
	if int64(out.Len()) > HardMaxSessionBytes {
		return nil, fmt.Errorf("decompressed session exceeds the %d-byte limit", int64(HardMaxSessionBytes))
	}
	return out.Bytes(), nil
}

// ErrZstdMissing is returned when a compressed session cannot be inspected because the
// optional tool is not installed. It is a skip, not a failure: the raw bytes still travel.
var ErrZstdMissing = errors.New("the optional zstd tool is not installed")
