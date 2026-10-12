package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aburan28/conductor/internal/client"
	"github.com/aburan28/conductor/internal/secrets"
)

// Project secrets: paste credentials into Conductor (sealed at rest), inject them into the
// local shell or a wrapped coding agent, and let MCP tools retrieve them for a session.

func cmdSecrets(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return secretsList(ctx, args)
	}
	switch sub, rest := args[0], args[1:]; sub {
	case "list", "ls":
		return secretsList(ctx, rest)
	case "set":
		return secretsSet(ctx, rest)
	case "get":
		return secretsGet(ctx, rest)
	case "delete", "rm":
		return secretsDelete(ctx, rest)
	case "env":
		return secretsEnv(ctx, rest)
	case "inject":
		return secretsInject(ctx, rest)
	case "push":
		return secretsPush(ctx, rest)
	default:
		return fmt.Errorf("unknown secrets subcommand %q (want list, set, get, delete, env, inject, or push)", sub)
	}
}

func secretsClient(project string) (*client.Client, string, error) {
	api, creds, err := mustClient()
	if err != nil {
		return nil, "", err
	}
	ref, err := projectRef(project, creds)
	if err != nil {
		return nil, "", err
	}
	return api, "/v1/projects/" + ref + "/secrets", nil
}

func secretsList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets list", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	api, path, err := secretsClient(*project)
	if err != nil {
		return err
	}
	var out struct {
		Secrets []secrets.View `json:"secrets"`
	}
	if err := api.Get(ctx, path, &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out.Secrets)
	}
	if len(out.Secrets) == 0 {
		fmt.Println("No secrets yet. Paste one:")
		fmt.Println(`  conductor secrets set DATABASE_URL`)
		fmt.Println(`  # then paste the value and end with Ctrl-D`)
		return nil
	}
	for _, s := range out.Secrets {
		desc := s.Description
		if desc != "" {
			desc = "  " + desc
		}
		fmt.Printf("%-32s  hint=%-8s%s\n", s.Name, s.Hint, desc)
	}
	return nil
}

func secretsSet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets set", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	valueFlag := fs.String("value", "", "plaintext value (prefer stdin / interactive paste so it stays out of shell history)")
	fromFile := fs.String("from-file", "", "read the value from a file")
	desc := fs.String("description", "", "optional note (never a place for the secret itself)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor secrets set — paste a secret into Conductor's encrypted vault

Usage:
  conductor secrets set NAME                 # prompt / read value from stdin
  conductor secrets set NAME --from-file PATH
  conductor secrets set NAME --value '…'     # avoided when possible (shell history)

The value is sealed at rest under conductord's secret key. Maintainers only.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Flags must come before NAME (Go's flag package stops at the first positional).
	if fs.NArg() != 1 {
		return errors.New("usage: conductor secrets set [flags] NAME")
	}
	name := fs.Arg(0)
	value, err := readSecretValue(*valueFlag, *fromFile)
	if err != nil {
		return err
	}
	api, path, err := secretsClient(*project)
	if err != nil {
		return err
	}
	var view secrets.View
	if err := api.Post(ctx, path, secrets.SetRequest{
		Name: name, Value: value, Description: *desc,
	}, &view); err != nil {
		return err
	}
	fmt.Printf("Stored %s (hint %s). Retrieve with MCP coord_get_secret or:\n", view.Name, view.Hint)
	fmt.Printf("  conductor secrets get %s\n", view.Name)
	fmt.Println(`  eval "$(conductor secrets env)"`)
	return nil
}

func readSecretValue(valueFlag, fromFile string) (string, error) {
	switch {
	case valueFlag != "" && fromFile != "":
		return "", errors.New("pass only one of --value and --from-file")
	case fromFile != "":
		body, err := os.ReadFile(fromFile)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(body), "\n"), nil
	case valueFlag != "":
		return valueFlag, nil
	default:
		stat, _ := os.Stdin.Stat()
		if stat != nil && (stat.Mode()&os.ModeCharDevice) != 0 {
			fmt.Fprint(os.Stderr, "Paste the secret, then Ctrl-D:\n")
		}
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		v := strings.TrimRight(string(body), "\n")
		if v == "" {
			return "", errors.New("empty value")
		}
		return v, nil
	}
}

func secretsGet(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets get", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: conductor secrets get NAME")
	}
	api, path, err := secretsClient(*project)
	if err != nil {
		return err
	}
	var out secrets.Opened
	if err := api.Get(ctx, path+"/"+fs.Arg(0), &out); err != nil {
		return err
	}
	if *asJSON {
		return emit(out)
	}
	fmt.Print(out.Value)
	if !strings.HasSuffix(out.Value, "\n") {
		fmt.Println()
	}
	return nil
}

func secretsDelete(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets delete", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: conductor secrets delete NAME")
	}
	api, path, err := secretsClient(*project)
	if err != nil {
		return err
	}
	if err := api.Delete(ctx, path+"/"+fs.Arg(0), nil); err != nil {
		return err
	}
	fmt.Printf("Deleted %s\n", fs.Arg(0))
	return nil
}

func secretsEnv(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets env", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	format := fs.String("format", "export", "export | dotenv | json | fish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env, err := fetchSecretEnv(ctx, *project)
	if err != nil {
		return err
	}
	switch *format {
	case "json":
		return emit(env)
	case "export", "dotenv", "fish":
		for _, name := range sortedMapKeys(env) {
			// A name that is not a shell identifier (stored before the tighter check)
			// would make `eval` / `source` abort before the rest of the vault loads.
			if !secrets.ShellName(name) {
				fmt.Fprintf(os.Stderr, "warning: skipping secret %q; not a shell identifier\n", name)
				continue
			}
			line, err := formatSecretLine(*format, name, env[name])
			if err != nil {
				return err
			}
			fmt.Print(line)
		}
	default:
		return fmt.Errorf("unknown --format %q (want export, dotenv, json, or fish)", *format)
	}
	return nil
}

// formatSecretLine renders one secret for a shell. fish has no `export` and `source`
// only accepts fish syntax, so it cannot reuse the bash/zsh lines.
func formatSecretLine(format, name, value string) (string, error) {
	switch format {
	case "dotenv":
		return fmt.Sprintf("%s=%s\n", name, shellSingleQuote(value)), nil
	case "export":
		return fmt.Sprintf("export %s=%s\n", name, shellSingleQuote(value)), nil
	case "fish":
		// `--` keeps a value that starts with '-' from being parsed as a set flag.
		// Fish single quotes still treat \\ and \' as escapes, so bash quoting
		// would store a different value or fail to parse.
		return fmt.Sprintf("set -gx %s -- %s\n", name, fishQuote(value)), nil
	default:
		return "", fmt.Errorf("unknown --format %q (want export, dotenv, json, or fish)", format)
	}
}

func fetchSecretEnv(ctx context.Context, project string) (map[string]string, error) {
	api, path, err := secretsClient(project)
	if err != nil {
		return nil, err
	}
	var out struct {
		Env map[string]string `json:"env"`
	}
	if err := api.Get(ctx, path+"/export", &out); err != nil {
		return nil, err
	}
	if out.Env == nil {
		out.Env = map[string]string{}
	}
	return out.Env, nil
}

func secretsInject(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets inject", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	shell := fs.String("shell", detectShell(), "bash | zsh | fish")
	rc := fs.String("rc", "", "shell rc file to update (default: ~/.zshrc or ~/.bashrc)")
	printOnly := fs.Bool("print", false, "print the snippet instead of writing an rc file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Ensure the vault is reachable before rewriting the user's shell config.
	if _, err := fetchSecretEnv(ctx, *project); err != nil {
		return err
	}
	api, creds, err := mustClient()
	if err != nil {
		return err
	}
	_ = api
	ref, err := projectRef(*project, creds)
	if err != nil {
		return err
	}
	snippet := secretsInjectSnippet(*shell, ref)
	if *printOnly {
		fmt.Print(snippet)
		return nil
	}
	path := *rc
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		switch *shell {
		case "zsh":
			path = filepath.Join(home, ".zshrc")
		case "fish":
			path = filepath.Join(home, ".config", "fish", "config.fish")
		default:
			path = filepath.Join(home, ".bashrc")
		}
	}
	if err := upsertRCBlock(path, secretsRCMarker, snippet); err != nil {
		return err
	}
	fmt.Printf("Updated %s. Open a new shell, or:\n  source %s\n", path, path)
	fmt.Println("Secrets are fetched at shell start via `conductor secrets env` (never written into the rc file).")
	return nil
}

const secretsRCMarker = "conductor-secrets"

func secretsInjectSnippet(shell, project string) string {
	projFlag := ""
	if project != "" {
		projFlag = " --project " + shellSingleQuote(project)
	}
	switch shell {
	case "fish":
		return fmt.Sprintf("# >>> %s >>>\n# Project secrets from Conductor (encrypted at rest; fetched at shell start).\nif type -q conductor\n  conductor secrets env --format fish%s | source\nend\n# <<< %s <<<\n",
			secretsRCMarker, projFlag, secretsRCMarker)
	default:
		return fmt.Sprintf("# >>> %s >>>\n# Project secrets from Conductor (encrypted at rest; fetched at shell start).\nif command -v conductor >/dev/null 2>&1; then\n  eval \"$(conductor secrets env%s)\"\nfi\n# <<< %s <<<\n",
			secretsRCMarker, projFlag, secretsRCMarker)
	}
}

func secretsPush(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("secrets push", flag.ExitOnError)
	project := fs.String("project", "", "project id or slug")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor secrets push — install project secrets into a coding-agent runtime

Usage:
  conductor secrets push claude [--project SLUG]   merge into ~/.claude/settings.json env
  conductor secrets push cursor [--project SLUG]   write a Cursor-local env helper (see notes)

Claude Code has no remote "team secrets" API; the supported path is the local settings.json
"env" map (or shell inject). Cursor Cloud Agents have no public secrets API in-tree — use
shell inject for local Cursor, and your cloud provider's secret store for remote agents.
`)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: conductor secrets push claude|cursor")
	}
	env, err := fetchSecretEnv(ctx, *project)
	if err != nil {
		return err
	}
	switch target := fs.Arg(0); target {
	case "claude":
		return pushClaudeSettings(env)
	case "cursor":
		return pushCursorHelper(env)
	default:
		return fmt.Errorf("unknown target %q (want claude or cursor)", target)
	}
}

func pushClaudeSettings(env map[string]string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var root map[string]any
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		if err := json.Unmarshal(body, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	} else {
		root = map[string]any{}
	}
	existing, _ := root["env"].(map[string]any)
	if existing == nil {
		existing = map[string]any{}
	}
	for k, v := range env {
		existing[k] = v
	}
	root["env"] = existing
	// Backup before rewrite — settings.json may hold other local preferences.
	if _, err := os.Stat(path); err == nil {
		bak := path + ".conductor-bak-" + time.Now().Format("20060102T150405")
		if err := copyFile(path, bak); err != nil {
			return err
		}
		fmt.Printf("Backed up previous settings to %s\n", bak)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o600); err != nil {
		return err
	}
	fmt.Printf("Merged %d secret(s) into %s under \"env\".\n", len(env), path)
	fmt.Println("Note: Claude Code has no remote secrets API; this writes plaintext into local settings.")
	fmt.Println("Prefer `conductor secrets inject` + wrap, or MCP coord_get_secret, when you can avoid persisting values.")
	return nil
}

func pushCursorHelper(env map[string]string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".conductor")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "cursor-secrets.env")
	var b strings.Builder
	b.WriteString("# Generated by `conductor secrets push cursor`. Do not commit.\n")
	b.WriteString("# Cursor Cloud Agents: no public secrets API in Conductor — configure secrets in Cursor's cloud UI / your provider.\n")
	for _, name := range sortedMapKeys(env) {
		fmt.Fprintf(&b, "%s=%s\n", name, shellSingleQuote(env[name]))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Printf("Wrote %d secret(s) to %s (0600).\n", len(env), path)
	fmt.Println("Local Cursor/CLI: `set -a; source ~/.conductor/cursor-secrets.env; set +a` or use `conductor secrets inject`.")
	fmt.Println("Gap: Cursor Cloud Agents / Background Agents have no documented secrets-upload API that Conductor can call; configure those in Cursor's product settings.")
	return nil
}

func detectShell() string {
	shell := filepath.Base(os.Getenv("SHELL"))
	switch shell {
	case "zsh", "bash", "fish":
		return shell
	default:
		return "bash"
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func sortedMapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Local sort without importing sort again if package already has helpers.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

func upsertRCBlock(path, marker, snippet string) error {
	begin := "# >>> " + marker + " >>>"
	end := "# <<< " + marker + " <<<"
	var existing string
	if body, err := os.ReadFile(path); err == nil {
		existing = string(body)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if i := strings.Index(existing, begin); i >= 0 {
		if j := strings.Index(existing[i:], end); j >= 0 {
			j = i + j + len(end)
			// consume trailing newline
			for j < len(existing) && (existing[j] == '\n' || existing[j] == '\r') {
				j++
			}
			existing = existing[:i] + existing[j:]
		}
	}
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		existing += "\n"
	}
	existing += snippet
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(existing), 0o600)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
