package systemd

import (
	"strings"
	"testing"
)

func TestBuildDBAndPlane(t *testing.T) {
	p, err := Build(Options{Home: "/home/dev", Conductord: "/usr/local/bin/conductord"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 2 {
		t.Fatalf("files = %d, want 2 (db + plane)", len(p.Files))
	}
	var db, plane string
	for _, f := range p.Files {
		switch {
		case strings.HasSuffix(f.Path, "conductor-db.service"):
			db = f.Content
		case strings.HasSuffix(f.Path, "conductor-control-plane.service"):
			plane = f.Content
		}
	}
	if db == "" || plane == "" {
		t.Fatalf("missing units: %+v", p.Files)
	}
	// Image guarantee: pre-pull precedes the run, tolerant when cached.
	if !strings.Contains(db, "ExecStartPre=-/usr/bin/docker pull "+PostgresImage) {
		t.Errorf("db unit must pre-pull the image:\n%s", db)
	}
	if !strings.Contains(db, PostgresImage+"\nExecStop") && !strings.Contains(db, PostgresImage) {
		t.Errorf("db unit must run the same image it pulls:\n%s", db)
	}
	if !strings.Contains(plane, "After=network-online.target conductor-db.service") {
		t.Errorf("plane must order behind db:\n%s", plane)
	}
	if !strings.Contains(plane, `ExecStart="/usr/local/bin/conductord" --addr 127.0.0.1:8080`) {
		t.Errorf("plane ExecStart wrong:\n%s", plane)
	}
	found := false
	for _, img := range p.Images {
		if img == PostgresImage {
			found = true
		}
	}
	if !found {
		t.Errorf("Images must include %s, got %v", PostgresImage, p.Images)
	}
}

func TestBuildVLLM(t *testing.T) {
	p, err := Build(Options{Home: "/home/dev", VLLM: []string{"qwen", "flash"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 4 { // db + plane + 2 vllm
		t.Fatalf("files = %d, want 4", len(p.Files))
	}
	var qwen string
	for _, f := range p.Files {
		if strings.HasSuffix(f.Path, "conductor-vllm-qwen.service") {
			qwen = f.Content
		}
	}
	if !strings.Contains(qwen, "ExecStartPre=-/usr/bin/docker pull docker.io/"+VLLMImageFull) {
		t.Errorf("qwen unit must pre-pull its image:\n%s", qwen)
	}
	if !strings.Contains(qwen, "--served-model-name qwen3.8-27b") {
		t.Errorf("qwen served name wrong:\n%s", qwen)
	}
	if !strings.Contains(qwen, "--tensor-parallel-size $TP") {
		t.Errorf("qwen must use systemd $TP expansion (no shell defaults):\n%s", qwen)
	}
	if strings.Contains(qwen, "${TP") {
		t.Errorf("qwen must not contain shell-style ${} expansion systemd cannot do:\n%s", qwen)
	}
	if !strings.Contains(qwen, "Environment=WEIGHTS=/home/dev/Qwen3.8-27B-FP8") {
		t.Errorf("qwen weights must resolve against Home:\n%s", qwen)
	}
	// Registry override flows into images (mirrors REGISTRY env).
	p2, err := Build(Options{Home: "/h", Registry: "mirror.example.com", VLLM: []string{"flash"}})
	if err != nil {
		t.Fatal(err)
	}
	if p2.Images[len(p2.Images)-1] != "mirror.example.com/"+VLLMImageFlash {
		t.Errorf("registry override wrong: %v", p2.Images)
	}
}

func TestBuildRejectsUnknownVariant(t *testing.T) {
	if _, err := Build(Options{Home: "/h", VLLM: []string{"llama"}}); err == nil {
		t.Fatal("expected error for unknown variant")
	}
}

func TestWithoutDB(t *testing.T) {
	no := false
	p, err := Build(Options{Home: "/h", WithDB: &no})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range p.Files {
		if strings.Contains(f.Path, "conductor-db") {
			t.Fatalf("db unit should be omitted: %s", f.Path)
		}
	}
	if len(p.Images) != 0 {
		t.Errorf("no images expected without db/vllm, got %v", p.Images)
	}
}
