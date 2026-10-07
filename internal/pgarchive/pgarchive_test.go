package pgarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aburan28/conductor/internal/backup"
	"github.com/aburan28/conductor/internal/backup/s3fake"
)

func testKey(t *testing.T) *DataKey {
	t.Helper()
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	k, err := NewDataKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sealAll(t *testing.T, plain []byte, k *DataKey) []byte {
	t.Helper()
	r, err := Seal(bytes.NewReader(plain), k)
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func openAll(sealed []byte, k *DataKey) ([]byte, error) {
	r, err := Open(bytes.NewReader(sealed), k)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestSealRoundTripAcrossChunkBoundaries(t *testing.T) {
	k := testKey(t)
	for _, n := range []int{0, 1, sealChunk - 1, sealChunk, sealChunk + 1, 3*sealChunk + 17} {
		plain := make([]byte, n)
		_, _ = rand.Read(plain)
		sealed := sealAll(t, plain, k)
		if !IsSealed(sealed) || bytes.Contains(sealed, plain[:min(n, 64)]) && n >= 64 {
			t.Fatalf("n=%d: output does not look sealed", n)
		}
		got, err := openAll(sealed, k)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("n=%d: round trip failed: %v (got %d bytes)", n, err, len(got))
		}
	}
}

func TestSealRejectsTampering(t *testing.T) {
	k := testKey(t)
	plain := bytes.Repeat([]byte("wal "), sealChunk/2) // two chunks
	sealed := sealAll(t, plain, k)

	if _, err := openAll(sealed, testKey(t)); !errors.Is(err, ErrWrongKey) {
		t.Errorf("another key: %v", err)
	}
	flipped := append([]byte{}, sealed...)
	flipped[len(flipped)/2] ^= 1
	if _, err := openAll(flipped, k); err == nil {
		t.Error("a flipped bit opened")
	}
	// Cut after the first chunk: it must not open as a shorter, valid object.
	first := headerLen + 4 + sealChunk + 16
	if _, err := openAll(sealed[:first], k); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Errorf("truncation: %v", err)
	}
	if _, err := openAll(append(append([]byte{}, sealed...), 'x'), k); err == nil {
		t.Error("trailing data accepted")
	}
}

func TestNamesAndSegments(t *testing.T) {
	for name, ok := range map[string]bool{
		"000000010000000000000002":                 true,
		"000000010000000000000002.partial":         true,
		"00000002.history":                         true,
		"000000010000000000000002.00000028.backup": true,
		"../etc/passwd":                            false,
		"000000010000000000000002/x":               false,
		"00000001000000000000000g":                 false,
		"":                                         false,
	} {
		if validArchiveName(name) != ok {
			t.Errorf("validArchiveName(%q) = %v", name, !ok)
		}
	}
	seg, err := SegmentForLSN(1, "0/2000028", 16<<20)
	if err != nil || seg != "000000010000000000000002" {
		t.Errorf("segment %s %v", seg, err)
	}
	seg, _ = SegmentForLSN(3, "1/FF000000", 16<<20)
	if seg != "0000000300000001000000FF" {
		t.Errorf("segment across log ids: %s", seg)
	}
	seg, _ = SegmentForLSN(1, "0/40000000", 1<<30) // 1 GiB segments
	if seg != "000000010000000000000001" {
		t.Errorf("1 GiB segment: %s", seg)
	}
}

func TestSystemIdentifier(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "global"), 0o700); err != nil {
		t.Fatal(err)
	}
	// pg_control stores the identifier little-endian in its first eight bytes.
	b := binary.LittleEndian.AppendUint64(nil, 7693956267215457548)
	if err := os.WriteFile(filepath.Join(dir, "global", "pg_control"), append(b, make([]byte, 100)...), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := SystemIdentifier(dir)
	if err != nil || id != "7693956267215457548" {
		t.Fatalf("id %s %v", id, err)
	}
}

func fakeArchiver(t *testing.T, seal bool) (*Archiver, *s3fake.Server, string) {
	t.Helper()
	fake := s3fake.New("bucket")
	t.Cleanup(fake.Close)
	u, _ := url.Parse(fake.URL)
	client := backup.New(backup.S3Config{Bucket: "bucket", Endpoint: "http://" + u.Host, PathStyle: true, Insecure: true,
		AccessKey: "AKID", SecretKey: "SECRET"})
	state := t.TempDir()
	cfg := Config{S3: client, Prefix: "team", Seal: seal, StateDir: state,
		Passphrase: func(context.Context) string { return "correct horse" }}
	return New(cfg, "7001"), fake, state
}

func TestArchiveFetchIdempotentAndConflict(t *testing.T) {
	for _, seal := range []bool{false, true} {
		a, fake, state := fakeArchiver(t, seal)
		ctx := context.Background()
		dir := t.TempDir()
		seg := filepath.Join(dir, "seg")
		data := bytes.Repeat([]byte{0xAB}, 3<<20)
		if err := os.WriteFile(seg, data, 0o600); err != nil {
			t.Fatal(err)
		}
		name := "000000010000000000000003"
		if err := a.ArchiveWAL(ctx, seg, name); err != nil {
			t.Fatalf("seal=%v archive: %v", seal, err)
		}
		key := "team/db/7001/wal/" + name
		if seal {
			key += ".sealed"
		}
		stored, ok := fake.Object(key)
		if !ok {
			t.Fatalf("seal=%v: nothing at %s: %v", seal, key, fake.Keys(""))
		}
		if seal == bytes.Equal(stored, data) {
			t.Fatalf("seal=%v: stored plaintext=%v", seal, bytes.Equal(stored, data))
		}
		// A retry after a crash between upload and Postgres's bookkeeping succeeds.
		if err := a.ArchiveWAL(ctx, seg, name); err != nil {
			t.Fatalf("seal=%v retry: %v", seal, err)
		}
		// Different content under the same name is refused, not overwritten.
		if err := os.WriteFile(seg, []byte("other"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := a.ArchiveWAL(ctx, seg, name); err == nil || !strings.Contains(err.Error(), "different content") {
			t.Fatalf("seal=%v conflict: %v", seal, err)
		}
		st := a.LoadStatus()
		if st.Archived != 2 || st.LastWAL != name || st.LastError == "" {
			t.Errorf("seal=%v status: %+v", seal, st)
		}

		// Fetch on "another machine": no key cache, so the sealed key comes from the bucket.
		if seal {
			_ = os.RemoveAll(filepath.Join(state, "db-keys"))
			a.key = nil
		}
		dest := filepath.Join(dir, "restored")
		if err := a.FetchWAL(ctx, name, dest); err != nil {
			t.Fatalf("seal=%v fetch: %v", seal, err)
		}
		got, _ := os.ReadFile(dest)
		if !bytes.Equal(got, data) {
			t.Fatalf("seal=%v: fetched content differs", seal)
		}
		if err := a.FetchWAL(ctx, "000000010000000000000099", dest); !errors.Is(err, ErrNotArchived) {
			t.Errorf("seal=%v missing: %v", seal, err)
		}
		if err := a.ArchiveWAL(ctx, seg, "../../escape"); err == nil {
			t.Errorf("seal=%v: a bad name was archived", seal)
		}
	}
}

func TestSealedArchiveNeedsTheRightPassphrase(t *testing.T) {
	a, _, state := fakeArchiver(t, true)
	ctx := context.Background()
	seg := filepath.Join(t.TempDir(), "s")
	_ = os.WriteFile(seg, []byte("segment"), 0o600)
	if err := a.ArchiveWAL(ctx, seg, "000000010000000000000001"); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(state, "db-keys"))
	b := New(Config{S3: a.cfg.S3, Prefix: "team", Seal: true, StateDir: state,
		Passphrase: func(context.Context) string { return "wrong" }}, "7001")
	err := b.FetchWAL(ctx, "000000010000000000000001", filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "passphrase does not open") {
		t.Fatalf("wrong passphrase: %v", err)
	}
	c := New(Config{S3: a.cfg.S3, Prefix: "team", Seal: true, StateDir: t.TempDir()}, "7001")
	if err := c.ArchiveWAL(ctx, seg, "000000010000000000000002"); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("no passphrase: %v", err)
	}
}

func TestPruneKeepsWhatTheNewestBackupsNeed(t *testing.T) {
	a, fake, _ := fakeArchiver(t, false)
	ctx := context.Background()
	put := func(m Manifest) {
		b, _ := json.Marshal(m)
		fake.Put(a.manifestKey(m.ID), b)
		fake.Put(a.baseTarKey(m.ID, false), []byte("tar"))
	}
	put(Manifest{ID: "20261001T000000Z", StartWAL: "000000010000000000000002"})
	put(Manifest{ID: "20261002T000000Z", StartWAL: "000000010000000000000005"})
	put(Manifest{ID: "20261003T000000Z", StartWAL: "000000020000000000000008"})
	for _, n := range []string{"000000010000000000000002", "000000010000000000000003", "000000010000000000000004",
		"000000010000000000000005", "000000010000000000000006", "00000002.history", "000000020000000000000007",
		"000000020000000000000008"} {
		fake.Put(a.walDir()+n, []byte("w"))
	}
	if _, err := a.Prune(ctx, 0); err == nil {
		t.Error("keep 0 accepted")
	}
	res, err := a.Prune(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.RemovedBackups, ",") != "20261001T000000Z" || res.RemovedWAL != 3 {
		t.Fatalf("prune: %+v", res)
	}
	left := strings.Join(fake.Keys(a.walDir()), " ")
	for _, want := range []string{"000000010000000000000005", "00000002.history", "000000020000000000000007"} {
		if !strings.Contains(left, want) {
			t.Errorf("%s was pruned; left %s", want, left)
		}
	}
	if _, ok := fake.Object(a.manifestKey("20261001T000000Z")); ok {
		t.Error("the old manifest survived")
	}
	backups, _ := a.Backups(ctx)
	if len(backups) != 2 {
		t.Errorf("backups after prune: %d", len(backups))
	}
}

func tarOf(t *testing.T, entries map[string]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, typ := range entries {
		h := &tar.Header{Name: name, Typeflag: typ, Mode: 0o600}
		if typ == tar.TypeReg {
			h.Size = 1
		}
		if typ == tar.TypeSymlink {
			h.Linkname = "/etc"
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if typ == tar.TypeReg {
			_, _ = tw.Write([]byte("x"))
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestExtractTarRefusesEscapes(t *testing.T) {
	for name, entries := range map[string]map[string]byte{
		"dotdot":   {"../evil": tar.TypeReg},
		"absolute": {"/etc/evil": tar.TypeReg},
		"symlink":  {"pg_tblspc/1": tar.TypeSymlink},
		"nested":   {"base/../../evil": tar.TypeReg},
	} {
		dir := t.TempDir()
		if err := extractTar(bytes.NewReader(tarOf(t, entries)), dir); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dir := t.TempDir()
	if err := extractTar(bytes.NewReader(tarOf(t, map[string]byte{"base/1/123": tar.TypeReg, "pg_wal": tar.TypeDir})), dir); err != nil {
		t.Fatalf("a normal archive: %v", err)
	}
	if info, err := os.Stat(filepath.Join(dir, "base", "1", "123")); err != nil || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("extracted file %v %v", info, err)
	}
}

func TestRestoreRefusesNonEmptyDirAndBadTarget(t *testing.T) {
	a, _, _ := fakeArchiver(t, false)
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "PG_VERSION"), []byte("16"), 0o600)
	_, err := a.Restore(context.Background(), RestoreOptions{DataDir: dir, RestoreCommand: "x %f %p"})
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty: %v", err)
	}
	_, err = a.Restore(context.Background(), RestoreOptions{DataDir: t.TempDir(), RestoreCommand: "x %f %p", TargetTime: "yesterday"})
	if err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("bad target: %v", err)
	}
	_, err = a.Restore(context.Background(), RestoreOptions{DataDir: t.TempDir(), RestoreCommand: "x"})
	if err == nil {
		t.Error("a restore command without the file placeholders was accepted")
	}
	_, err = a.Restore(context.Background(), RestoreOptions{DataDir: t.TempDir(), RestoreCommand: "x %f %p"})
	if err == nil || !strings.Contains(err.Error(), "no base backup") {
		t.Errorf("empty archive: %v", err)
	}
}

func TestPutStreamMultipartThroughS3Client(t *testing.T) {
	a, fake, _ := fakeArchiver(t, true)
	k := testKey(t)
	plain := make([]byte, 12<<20+123)
	_, _ = rand.Read(plain)
	r, _ := Seal(bytes.NewReader(plain), k)
	n, err := a.s3.PutStream(context.Background(), "big/object", r, backup.MinPartSize, "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := fake.Object("big/object")
	if !ok || int64(len(stored)) != n || fake.Requests["POST"] < 2 {
		t.Fatalf("multipart: ok=%v len=%d n=%d posts=%d", ok, len(stored), n, fake.Requests["POST"])
	}
	got, err := openAll(stored, k)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip through multipart: %v", err)
	}

}
