package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aburan28/conductor/internal/config"
)

// conductor down stops the control plane started by `conductor up` (and, when run inside
// that repository, the one started by `make up`). Postgres keeps running, matching
// `make down`; `--db` also stops the container.
func cmdDown(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("down", flag.ExitOnError)
	stopDB := fs.Bool("db", false, "also stop the Postgres container (conductor-db)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	dirs := []string{}
	if dir, err := runtimeDir(); err == nil {
		dirs = append(dirs, dir)
	}
	if cwd, err := os.Getwd(); err == nil {
		if root, err := config.FindRoot(cwd); err == nil {
			dirs = append(dirs, filepath.Join(root, ".conductor", "runtime"))
		}
	}

	stopped := false
	seen := map[string]bool{}
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true

		pidFile := filepath.Join(abs, "conductord.pid")
		pid := readPid(pidFile)
		if pid == 0 {
			continue
		}
		if !processAlive(pid) {
			os.Remove(pidFile) // stale
			continue
		}
		fmt.Printf("stopping conductord (pid %d)\n", pid)
		syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 40 && processAlive(pid); i++ {
			time.Sleep(250 * time.Millisecond)
		}
		if processAlive(pid) {
			fmt.Fprintln(os.Stderr, "did not exit; sending SIGKILL")
			syscall.Kill(pid, syscall.SIGKILL)
		}
		os.Remove(pidFile)
		stopped = true
	}
	if !stopped {
		fmt.Println("no control plane started by `conductor up` is running")
	}

	if !*stopDB {
		return nil
	}
	out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", "conductor-db").CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such object") {
			fmt.Println("no conductor-db container to stop")
			return nil
		}
		return fmt.Errorf("docker inspect conductor-db: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "true" {
		fmt.Println("conductor-db is not running")
		return nil
	}
	if out, err := exec.Command("docker", "stop", "conductor-db").CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop conductor-db: %v\n%s", err, out)
	}
	fmt.Println("stopped conductor-db (data volume kept; start it again with conductor up)")
	return nil
}
