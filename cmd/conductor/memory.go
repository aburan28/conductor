package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/memory"
)

func cmdMemory(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: conductor memory <configure|disable|status|remember|search|timeline|show|forget|context>")
	}
	switch args[0] {
	case "configure":
		fs := flag.NewFlagSet("memory configure", flag.ContinueOnError)
		local := fs.Bool("local", false, "use a local Redis server on 127.0.0.1:6379")
		urlStdin := fs.Bool("url-stdin", false, "read the Redis URL from stdin, keeping credentials out of shell history")
		cluster := fs.Bool("cluster", false, "use Redis cluster mode (ElastiCache configuration endpoint)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *local == *urlStdin {
			return errors.New("choose exactly one of --local or --url-stdin")
		}
		url := "redis://127.0.0.1:6379/0"
		if *urlStdin {
			body, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
			if err != nil {
				return err
			}
			url = strings.TrimSpace(string(body))
		}
		if err := memory.Configure(url, *cluster); err != nil {
			return err
		}
		fmt.Println("Conductor memory configured. Content stays in this Redis store and is encrypted with a key in the local Conductor state directory.")
		return memoryStatus(ctx)
	case "disable":
		if err := memory.Disable(); err != nil {
			return err
		}
		fmt.Println("Conductor memory capture disabled; existing Redis records and the local encryption key were kept.")
		return nil
	case "status":
		return memoryStatus(ctx)
	case "remember":
		fs := flag.NewFlagSet("memory remember", flag.ContinueOnError)
		project := fs.String("project", "", "project identity (default: current repository)")
		kind := fs.String("kind", "note", "observation kind")
		session := fs.String("session", "", "session identifier")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(os.Stdin, 8193))
		if err != nil {
			return err
		}
		if len(body) > 8192 {
			return errors.New("memory note exceeds 8192 bytes")
		}
		store, err := memory.Open()
		if err != nil {
			return err
		}
		defer store.Close()
		o, err := store.Remember(ctx, memory.Observation{Project: memoryProject(*project), Session: *session, Kind: *kind, Summary: string(body), Harness: "manual"})
		if err != nil {
			return err
		}
		return emit(o)
	case "search":
		return memoryList(ctx, "search", args[1:])
	case "timeline":
		return memoryList(ctx, "timeline", args[1:])
	case "show":
		return memoryList(ctx, "show", args[1:])
	case "forget":
		fs := flag.NewFlagSet("memory forget", flag.ContinueOnError)
		project := fs.String("project", "", "project identity (default: current repository)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return errors.New("usage: conductor memory forget <observation-id> [--project NAME]")
		}
		store, err := memory.Open()
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.Forget(ctx, memoryProject(*project), fs.Arg(0)); err != nil {
			return err
		}
		fmt.Println("Observation deleted.")
		return nil
	case "context":
		fs := flag.NewFlagSet("memory context", flag.ContinueOnError)
		project := fs.String("project", "", "project identity (default: current repository)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		store, err := memory.Open()
		if err != nil {
			return err
		}
		defer store.Close()
		out, err := store.Context(ctx, memoryProject(*project), "", 1600)
		if err != nil {
			return err
		}
		fmt.Print(out)
		return nil
	default:
		return fmt.Errorf("unknown memory subcommand %q", args[0])
	}
}

func memoryProject(project string) string {
	if project != "" {
		return project
	}
	return memory.ProjectForDir("")
}

func memoryStatus(ctx context.Context) error {
	cfg, _, err := memory.LoadConfig()
	if err != nil {
		return err
	}
	store, err := memory.Open()
	if err != nil {
		return err
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := store.Ping(ctx); err != nil {
		return fmt.Errorf("memory Redis is configured but unreachable: %w", err)
	}
	count, err := store.Count(ctx, memoryProject(""))
	if err != nil {
		return err
	}
	backend := "local Redis"
	if strings.HasPrefix(cfg.RedisURL, "rediss://") {
		backend = "TLS Redis / ElastiCache"
	}
	if cfg.Cluster {
		backend += " cluster"
	}
	fmt.Printf("Conductor memory: enabled, %s, %d observations in this project.\n", backend, count)
	return nil
}

func memoryList(ctx context.Context, action string, args []string) error {
	fs := flag.NewFlagSet("memory "+action, flag.ContinueOnError)
	project := fs.String("project", "", "project identity (default: current repository)")
	limit := fs.Int("limit", 20, "maximum results")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := memory.Open()
	if err != nil {
		return err
	}
	defer store.Close()
	ref := memoryProject(*project)
	switch action {
	case "search":
		query := strings.Join(fs.Args(), " ")
		items, err := store.Search(ctx, ref, query, *limit)
		if err != nil {
			return err
		}
		return emit(memory.Previews(items))
	case "timeline":
		if fs.NArg() != 1 {
			return errors.New("usage: conductor memory timeline <observation-id>")
		}
		items, err := store.Timeline(ctx, ref, fs.Arg(0), *limit)
		if err != nil {
			return err
		}
		return emit(memory.Previews(items))
	case "show":
		if fs.NArg() != 1 {
			return errors.New("usage: conductor memory show <observation-id>")
		}
		o, err := store.Get(ctx, ref, fs.Arg(0))
		if err != nil {
			return err
		}
		return emit(o)
	}
	return errors.New("unknown memory list action")
}

// hookMemoryObserve is intentionally fail-open and silent. Hook payloads stay in-process;
// only compact observations cross the Redis connection.
func hookMemoryObserve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hook memory-observe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	harness := fs.String("harness", "unknown", "coding tool")
	if err := fs.Parse(args); err != nil {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(body) == 0 {
		return nil
	}
	o, err := memory.FromHook(body, *harness)
	if err != nil || o == nil {
		return nil
	}
	store, err := memory.Open()
	if err != nil {
		return nil
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _ = store.Remember(ctx, *o)
	return nil
}

func hookMemoryContext(ctx context.Context, args []string) error {
	body, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil || len(body) == 0 {
		return nil
	}
	var e memory.HookEvent
	if json.Unmarshal(body, &e) != nil || e.Prompt == "" {
		return nil
	}
	store, err := memory.Open()
	if err != nil {
		return nil
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := store.Related(ctx, memory.ProjectForDir(e.Cwd), e.Prompt, e.SessionID, 1600)
	if err == nil && out != "" {
		fmt.Print(out)
	}
	return nil
}

func memoryStartupContext(ctx context.Context, cwd, session string) string {
	store, err := memory.Open()
	if err != nil {
		return ""
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
	defer cancel()
	out, err := store.Context(ctx, memory.ProjectForDir(cwd), session, 1600)
	if err != nil {
		return ""
	}
	return out
}
