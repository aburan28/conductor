// Package systemd generates systemd user units that run the Conductor stack on
// machines with systemd: Postgres (Docker), the conductord control plane
// (binary), and optionally a local vLLM endpoint (Docker) for OpenCode.
//
// The session-capture hook (bare sessions at shutdown + periodic) already lives
// in internal/shutdownhook and is intentionally not duplicated here: `conductor
// systemd install` composes both plans so one command covers daemon + hook.
//
// Every Docker-backed unit carries `ExecStartPre=-docker pull <image>` (note
// the `-` prefix: a pull failure must not stop a unit whose image is already
// cached, e.g. an offline reboot) AND the CLI offers `conductor systemd
// pull-images` to pre-pull everything while online, so a first enable does not
// race a multi-GB download against a service start timeout.
package systemd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PostgresImage is the database image. It must stay in sync with
// docker-compose.yml and .github/workflows/ci.yml.
const PostgresImage = "postgres:17-alpine"

// VLLM image tags. They must stay in sync with scripts/serve-local.sh
// default_image().
const (
	VLLMImageFlash = "vllm/vllm-openai:glm53-flash"
	VLLMImageFull  = "vllm/vllm-openai:v0.28.0"
)

// Variants mirrors serve-local.sh normalize_variant().
var Variants = []string{"flash", "glm53", "qwen"}

// File is one unit file to write, with the mode it should have.
type File struct {
	Path    string
	Content string
	Mode    os.FileMode
}

// Plan is everything needed to install (or remove) the daemon units on Linux.
type Plan struct {
	Files       []File
	Enable      []string // commands the user runs to activate the units
	Disable     []string // commands to deactivate them
	Images      []string // docker images the units need (pre-pull these)
	Notes       []string
	VLLMEnabled []string // vLLM variants included in this plan
}

// Options parameterizes generation.
type Options struct {
	// Exe is the conductor binary path (for the control-plane unit).
	Exe string
	// Conductord is the conductord binary path. Defaults to Exe's directory's
	// conductord sibling, then "conductord" on PATH.
	Conductord string
	// Home is the user's home (units go under ~/.config/systemd/user).
	Home string
	// RepoRoot is the conductor checkout (control-plane unit runs with it as
	// WorkingDirectory so relative .conductor/runtime paths resolve).
	RepoRoot string
	// DatabaseURL overrides the default local DATABASE_URL for the unit.
	DatabaseURL string
	// Addr overrides the default 127.0.0.1:8080 bind for the unit.
	Addr string
	// Registry overrides the docker.io default for vLLM images (mirrors the
	// REGISTRY env of scripts/serve-local.sh).
	Registry string
	// WithDB includes the postgres unit. Default true.
	WithDB *bool
	// VLLM lists serve variants (flash|glm53|qwen) to also generate units for.
	// Empty means no vLLM units.
	VLLM []string
}

const (
	dbUnitName      = "conductor-db"
	planeUnitName   = "conductor-control-plane"
	vllmUnitPrefix  = "conductor-vllm-"
	defaultDatabase = "postgres://conductor:conductor@localhost:55432/conductor?sslmode=disable"
	defaultAddr     = "127.0.0.1:8080"
)

// EffectiveRegistry returns the Docker registry for vLLM images.
func (o Options) EffectiveRegistry() string {
	if o.Registry != "" {
		return strings.TrimSuffix(o.Registry, "/")
	}
	if v := os.Getenv("REGISTRY"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "docker.io"
}

// VLLMImage returns the fully-qualified image for a serve variant.
func (o Options) VLLMImage(variant string) string {
	switch strings.ToLower(variant) {
	case "flash", "glm53-flash", "glm-5.3-flash":
		return o.EffectiveRegistry() + "/" + VLLMImageFlash
	default: // glm53, qwen
		return o.EffectiveRegistry() + "/" + VLLMImageFull
	}
}

// NormalizeVariant maps CLI aliases to canonical variant names.
func NormalizeVariant(s string) (string, bool) {
	switch strings.ToLower(s) {
	case "flash", "glm53-flash", "glm-5.3-flash", "glm5.3-flash":
		return "flash", true
	case "glm53", "glm-5.3", "glm5.3", "full":
		return "glm53", true
	case "qwen", "qwen3.8", "qwen38", "qwen3.8-27b", "qwen3-8":
		return "qwen", true
	}
	return "", false
}

// RequiredImages lists every docker image this plan needs.
func (o Options) RequiredImages() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(img string) {
		if _, ok := seen[img]; !ok {
			seen[img] = struct{}{}
			out = append(out, img)
		}
	}
	if o.withDB() {
		add(PostgresImage)
	}
	for _, v := range o.VLLM {
		if n, ok := NormalizeVariant(v); ok {
			add(o.VLLMImage(n))
		}
	}
	return out
}

func (o Options) withDB() bool {
	if o.WithDB != nil {
		return *o.WithDB
	}
	return true
}

// Build produces the install plan for Linux systemd user units.
func Build(o Options) (Plan, error) {
	if o.Home == "" {
		return Plan{}, fmt.Errorf("systemd: no home directory")
	}
	if o.Exe == "" {
		o.Exe = "conductor"
	}
	if o.Conductord == "" {
		o.Conductord = "conductord"
	}
	if o.DatabaseURL == "" {
		o.DatabaseURL = defaultDatabase
	}
	if o.Addr == "" {
		o.Addr = defaultAddr
	}
	var vllm []string
	for _, v := range o.VLLM {
		n, ok := NormalizeVariant(v)
		if !ok {
			return Plan{}, fmt.Errorf("systemd: unknown vLLM variant %q (flash|glm53|qwen)", v)
		}
		dup := false
		for _, e := range vllm {
			if e == n {
				dup = true
			}
		}
		if !dup {
			vllm = append(vllm, n)
		}
	}

	base := filepath.Join(o.Home, ".config", "systemd", "user")
	var files []File
	var enable, disable []string

	if o.withDB() {
		files = append(files, File{
			Path:    filepath.Join(base, dbUnitName+".service"),
			Content: dbUnit(o),
			Mode:    0o644,
		})
		enable = append(enable, "systemctl --user enable --now "+dbUnitName+".service")
		disable = append(disable, "systemctl --user disable --now "+dbUnitName+".service")
	}
	files = append(files, File{
		Path:    filepath.Join(base, planeUnitName+".service"),
		Content: planeUnit(o),
		Mode:    0o644,
	})
	enable = append(enable, "systemctl --user enable --now "+planeUnitName+".service")
	disable = append(disable, "systemctl --user disable --now "+planeUnitName+".service")

	for _, v := range vllm {
		name := vllmUnitPrefix + v
		files = append(files, File{
			Path:    filepath.Join(base, name+".service"),
			Content: vllmUnit(o, v),
			Mode:    0o644,
		})
		enable = append(enable, "systemctl --user enable --now "+name+".service")
		disable = append(disable, "systemctl --user disable --now "+name+".service")
	}

	// Enable ordering: reload first, db before plane.
	enableFull := []string{"systemctl --user daemon-reload"}
	enableFull = append(enableFull, enable...)

	notes := []string{
		"Units are user units: for a headless host run: loginctl enable-linger " + shellQuote(os.Getenv("USER")),
		"Images are pre-pulled with: conductor systemd pull-images" + vllmPullHint(vllm),
		"Session capture (bare sessions at shutdown + periodic) is separate: conductor sessions install-hook --install",
	}
	return Plan{
		Files:       files,
		Enable:      enableFull,
		Disable:     []string{"systemctl --user disable --now " + strings.Join(disableUnits(disable), " ")},
		Images:      o.RequiredImages(),
		Notes:       notes,
		VLLMEnabled: vllm,
	}, nil
}

func disableUnits(disable []string) []string {
	var out []string
	for _, d := range disable {
		f := strings.TrimPrefix(d, "systemctl --user disable --now ")
		out = append(out, f)
	}
	return out
}

func vllmPullHint(vllm []string) string {
	if len(vllm) == 0 {
		return ""
	}
	return " --with-vllm " + strings.Join(vllm, ",")
}

// dbUnit runs postgres in Docker with a named volume. The ExecStartPre pull
// uses the `-` prefix so an offline reboot with a cached image still starts;
// use pull-images while online to guarantee the cache.
func dbUnit(o Options) string {
	return fmt.Sprintf(`[Unit]
Description=Conductor — postgres (control-plane database)
Documentation=https://github.com/adamburan/conductor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Restart=on-failure
RestartSec=5s
# Pre-pull so the first enable does not race a download against start; the '-'
# prefix tolerates an offline reboot when the image is already cached. Run
# `+"`conductor systemd pull-images`"+` while online to guarantee the cache.
ExecStartPre=-/usr/bin/docker pull %s
ExecStartPre=-/usr/bin/docker rm -f conductor-db
ExecStart=/usr/bin/docker run --rm --name conductor-db \
  -p 127.0.0.1:55432:5432 \
  -e POSTGRES_USER=conductor \
  -e POSTGRES_PASSWORD=conductor \
  -e POSTGRES_DB=conductor \
  -v conductor-pgdata:/var/lib/postgresql/data \
  %s
ExecStop=/usr/bin/docker stop conductor-db

[Install]
WantedBy=default.target
`, PostgresImage, PostgresImage)
}

// planeUnit runs conductord as a user service. It is a plain binary (no image
// to pull); After/Wants keeps startup ordered behind the db unit when present.
func planeUnit(o Options) string {
	after := "network-online.target"
	if o.withDB() {
		after += " " + dbUnitName + ".service"
	}
	working := ""
	if o.RepoRoot != "" {
		working = "WorkingDirectory=" + o.RepoRoot + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=Conductor — control plane (conductord)
Documentation=https://github.com/adamburan/conductor
After=%s
Wants=network-online.target
%s

[Service]
Type=simple
Restart=on-failure
RestartSec=5s
%sEnvironment=DATABASE_URL=%s
ExecStart=%s --addr %s

[Install]
WantedBy=default.target
`, after, dbWants(o), working, o.DatabaseURL, systemdArg(o.Conductord), o.Addr)
}

func dbWants(o Options) string {
	if o.withDB() {
		return "Wants=" + dbUnitName + ".service\n"
	}
	return ""
}

// vllmUnit runs one serve-local variant in Docker. Weights resolve at
// generation time from $WEIGHTS or the same ~/ host paths serve-local.sh
// searches, and TP (tensor parallel) from $TP or 1 — both baked into
// Environment= lines because systemd does not do shell-style ${VAR:-default}
// expansion. Override at runtime with:
// systemctl --user set-environment TP=8 WEIGHTS=/data/weights
func vllmUnit(o Options, variant string) string {
	image := o.VLLMImage(variant)
	weights := vllmWeightsPath(o, variant)
	tp := os.Getenv("TP")
	if tp == "" {
		tp = "1"
	}
	return fmt.Sprintf(`[Unit]
Description=Conductor — local vLLM (%s) for OpenCode
Documentation=https://github.com/adamburan/conductor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Restart=on-failure
RestartSec=10s
# Image pre-pull (tolerant when cached+offline). Weights/TP are baked below;
# override live with: systemctl --user set-environment TP=8 WEIGHTS=/data/w
Environment=WEIGHTS=%s
Environment=TP=%s
ExecStartPre=-/usr/bin/docker pull %s
ExecStartPre=-/usr/bin/docker rm -f %s
ExecStart=/usr/bin/docker run --rm --name %s \
  --gpus all --ipc=host --network host \
  -e VLLM_ENGINE_READY_TIMEOUT_S=3600 \
  -v "$WEIGHTS:/models:ro" \
  %s \
  /models --host 0.0.0.0 --port 8000 --served-model-name %s --tensor-parallel-size $TP
ExecStop=/usr/bin/docker stop %s

[Install]
WantedBy=default.target
`, variant, weights, tp, image, vllmContainer(variant), vllmContainer(variant), image, vllmServed(variant), vllmContainer(variant))
}

func vllmContainer(variant string) string {
	switch variant {
	case "flash":
		return "glm53-flash"
	case "glm53":
		return "glm53"
	default:
		return "qwen38-27b"
	}
}

func vllmServed(variant string) string {
	switch variant {
	case "flash":
		return "zai-org/GLM-5.3-Flash"
	case "glm53":
		return "glm-5.3"
	default:
		return "qwen3.8-27b"
	}
}

// vllmWeightsPath resolves the host weights dir at generation time: $WEIGHTS
// wins, else the first ~/ candidate serve-local.sh would pick.
func vllmWeightsPath(o Options, variant string) string {
	if w := os.Getenv("WEIGHTS"); w != "" {
		return w
	}
	home := o.Home
	if home == "" {
		home = "$HOME"
	}
	switch variant {
	case "flash":
		return filepath.Join(home, "GLM-5.3-Flash")
	case "glm53":
		return filepath.Join(home, "GLM-5.3")
	default:
		return filepath.Join(home, "Qwen3.8-27B-FP8")
	}
}

// systemdArg double-quotes a path for Exec lines so spaces survive systemd's
// argument tokenization (same escaping as internal/shutdownhook).
func systemdArg(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

func shellQuote(s string) string {
	if s == "" {
		return "$USER"
	}
	if !strings.ContainsAny(s, " \t'\"\\$") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
