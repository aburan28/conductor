package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adamburan/conductor/internal/config"
	"github.com/adamburan/conductor/internal/systemd"
)

// ---------------------------------------------------------------------------
// systemd — daemon + hook units with guaranteed image pre-pulls
// ---------------------------------------------------------------------------
//
// `conductor systemd` covers the machines that HAVE systemd (the watchdog
// script covers the ones that do not): it generates user units for the
// postgres database (Docker), the conductord control plane (binary), and
// optionally a local vLLM endpoint (Docker), alongside the session-capture
// hook (`conductor sessions install-hook`) that covers bare sessions at
// shutdown. Image availability is guaranteed two ways: every Docker-backed
// unit pre-pulls its image at start (tolerant when cached+offline), and
// `conductor systemd pull-images` pre-pulls everything while online.

func cmdSystemd(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(`conductor systemd — user units for the daemon, db, and vLLM

  conductor systemd install [--install] [--with-db] [--no-db] [--with-vllm qwen|all] [--pull-images]
  conductor systemd pull-images [--with-vllm all]
  conductor systemd status [--with-vllm qwen]

Generates systemd user units for postgres (Docker), conductord (binary), and
optionally vLLM (Docker). Every Docker-backed unit pre-pulls its image at
start; pull-images pre-pulls while online so enables and reboots never race a
download. Session capture at shutdown stays separate:
` + "`conductor sessions install-hook`" + `.
`)
		return nil
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "install":
		return systemdInstall(rest)
	case "pull-images":
		return systemdPullImages(rest)
	case "status":
		return systemdStatus(rest)
	default:
		return fmt.Errorf("unknown systemd subcommand %q", sub)
	}
}

type systemdFlags struct {
	withDB    bool
	noDB      bool
	vllm      string
	registry  string
	addr      string
	dsn       string
	install   bool
	uninstall bool
	pullImgs  bool
	asJSON    bool
}

func parseSystemdFlags(args []string, name string) (*flag.FlagSet, *systemdFlags) {
	f := &systemdFlags{}
	fs := flag.NewFlagSet("systemd "+name, flag.ExitOnError)
	fs.BoolVar(&f.withDB, "with-db", true, "include the postgres unit")
	fs.BoolVar(&f.noDB, "no-db", false, "omit the postgres unit (external database)")
	fs.StringVar(&f.vllm, "with-vllm", "", "also generate vLLM units: flash, glm53, qwen, all, or comma-separated")
	fs.StringVar(&f.registry, "registry", "", "docker registry for vLLM images (default docker.io, or REGISTRY env)")
	fs.StringVar(&f.addr, "addr", "", "conductord bind address for the unit (default 127.0.0.1:8080)")
	fs.StringVar(&f.dsn, "dsn", "", "DATABASE_URL for the unit (default local postgres)")
	fs.BoolVar(&f.install, "install", false, "write the unit files (default is to print them)")
	fs.BoolVar(&f.uninstall, "uninstall", false, "remove the unit files")
	fs.BoolVar(&f.pullImgs, "pull-images", false, "with --install: pre-pull every image before writing units")
	fs.BoolVar(&f.asJSON, "json", false, "machine-readable output")
	return fs, f
}

func systemdOptions(f *systemdFlags) (systemd.Options, error) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "conductor"
	}
	conductord := siblingBinary(exe, "conductord")
	home, err := os.UserHomeDir()
	if err != nil {
		return systemd.Options{}, err
	}
	repo := ""
	if cwd, err := os.Getwd(); err == nil {
		if root, err := config.FindRoot(cwd); err == nil {
			repo = root
		}
	}
	withDB := f.withDB && !f.noDB
	opts := systemd.Options{
		Exe: exe, Conductord: conductord, Home: home, RepoRoot: repo,
		DatabaseURL: f.dsn, Addr: f.addr, Registry: f.registry, WithDB: &withDB,
	}
	if v := strings.TrimSpace(f.vllm); v != "" && !strings.EqualFold(v, "none") {
		if strings.EqualFold(v, "all") {
			opts.VLLM = append([]string{}, systemd.Variants...)
		} else {
			for _, part := range strings.Split(v, ",") {
				if p := strings.TrimSpace(part); p != "" {
					opts.VLLM = append(opts.VLLM, p)
				}
			}
		}
	}
	return opts, nil
}

func siblingBinary(exe, name string) string {
	if dir := filepath.Dir(exe); dir != "" {
		if cand := filepath.Join(dir, name); fileExists(cand) {
			return cand
		}
	}
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return name
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func systemdInstall(args []string) error {
	fs, f := parseSystemdFlags(args, "install")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor systemd install — user units for the daemon, db, and vLLM

Generates systemd user units for postgres (Docker, image pre-pulled at start),
the conductord control plane (binary, ordered after the db), and optionally a
local vLLM endpoint (Docker) for OpenCode. Pair with the session-capture hook
for bare sessions at shutdown:

  conductor systemd install --with-vllm qwen     print daemon+db+qwen units
  conductor systemd install --install            write the files (then run the printed command)
  conductor systemd install --install --with-vllm all --pull-images
  conductor systemd pull-images --with-vllm all  pre-pull every image while online
  conductor sessions install-hook --install      bare-session capture at shutdown (separate)

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("systemd units are Linux-only (this machine is %s); the session hook covers darwin via launchd", runtime.GOOS)
	}
	opts, err := systemdOptions(f)
	if err != nil {
		return err
	}
	plan, err := systemd.Build(opts)
	if err != nil {
		return err
	}

	if f.uninstall {
		removed := 0
		for _, file := range plan.Files {
			if err := os.Remove(file.Path); err == nil {
				removed++
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		if f.asJSON {
			return emit(map[string]any{"removed": removed, "disable": plan.Disable})
		}
		fmt.Printf("Removed %d unit file(s). Deactivate with:\n", removed)
		for _, cmd := range plan.Disable {
			fmt.Printf("  %s\n", cmd)
		}
		return nil
	}

	if f.asJSON {
		return emit(plan)
	}
	if !f.install {
		fmt.Printf("Systemd user units (daemon + db%s). Run with --install to write these files:\n\n", vllmSuffix(plan))
		for _, file := range plan.Files {
			fmt.Printf("# %s\n%s\n", file.Path, file.Content)
		}
		fmt.Println("Then enable:")
		for _, cmd := range plan.Enable {
			fmt.Printf("  %s\n", cmd)
		}
		printSystemdNotes(plan)
		return nil
	}

	// --install also pre-pulls when asked with --pull-images.
	if f.pullImgs {
		if err := pullImages(plan.Images); err != nil {
			return err
		}
	}
	for _, file := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file.Path, []byte(file.Content), file.Mode); err != nil {
			return err
		}
		fmt.Printf("  wrote  %s\n", file.Path)
	}
	fmt.Println("\nActivate (Conductor does not run system commands for you):")
	for _, cmd := range plan.Enable {
		fmt.Printf("  %s\n", cmd)
	}
	printSystemdNotes(plan)
	return nil
}

func vllmSuffix(plan systemd.Plan) string {
	if len(plan.VLLMEnabled) == 0 {
		return ""
	}
	return " + vLLM (" + strings.Join(plan.VLLMEnabled, ",") + ")"
}

func printSystemdNotes(plan systemd.Plan) {
	for _, n := range plan.Notes {
		fmt.Printf("\nNote: %s\n", n)
	}
	if len(plan.Images) > 0 {
		fmt.Printf("\nImages needed: %s\n", strings.Join(plan.Images, ", "))
		fmt.Println("Pre-pull while online: conductor systemd pull-images")
	}
}

// systemdPullImages pulls the db + selected vLLM images so a later enable or
// offline reboot never races a download against a start timeout.
func systemdPullImages(args []string) error {
	fs, f := parseSystemdFlags(args, "pull-images")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor systemd pull-images — pre-pull every image systemd units need

  conductor systemd pull-images                 postgres only
  conductor systemd pull-images --with-vllm all db + all vLLM images

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, err := systemdOptions(f)
	if err != nil {
		return err
	}
	plan, err := systemd.Build(opts)
	if err != nil {
		return err
	}
	if len(plan.Images) == 0 {
		fmt.Println("No images required (db omitted, no vLLM variants).")
		return nil
	}
	return pullImages(plan.Images)
}

func pullImages(images []string) error {
	docker, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker not found on PATH; install Docker, then retry")
	}
	failed := false
	for _, img := range images {
		fmt.Printf("docker pull %s\n", img)
		cmd := exec.Command(docker, "pull", img)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "error: pull %s: %v\n", img, err)
			failed = true
		}
	}
	if failed {
		return errors.New("one or more image pulls failed")
	}
	fmt.Printf("Pulled %d image(s).\n", len(images))
	return nil
}

// systemdStatus reports which required images are present locally (docker
// image inspect) without pulling anything.
func systemdStatus(args []string) error {
	fs, f := parseSystemdFlags(args, "status")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `conductor systemd status — which unit images are cached locally

  conductor systemd status --with-vllm qwen

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts, err := systemdOptions(f)
	if err != nil {
		return err
	}
	plan, err := systemd.Build(opts)
	if err != nil {
		return err
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return errors.New("docker not found on PATH")
	}
	missing := 0
	for _, img := range plan.Images {
		if exec.Command(docker, "image", "inspect", img).Run() == nil {
			fmt.Printf("  cached   %s\n", img)
		} else {
			fmt.Printf("  missing  %s\n", img)
			missing++
		}
	}
	if missing > 0 {
		fmt.Printf("\n%d image(s) missing — run: conductor systemd pull-images%s\n", missing, vllmFlag(f))
		return fmt.Errorf("%d image(s) missing", missing)
	}
	fmt.Println("\nAll images cached.")
	return nil
}

func vllmFlag(f *systemdFlags) string {
	if v := strings.TrimSpace(f.vllm); v != "" {
		return " --with-vllm " + v
	}
	return ""
}
