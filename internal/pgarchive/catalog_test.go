package pgarchive

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// putBackup stores a manifest and its base.tar the way BaseBackup does, for a fake bucket.
func putBackup(t *testing.T, a *Archiver, fakePut func(string, []byte), m Manifest) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	fakePut(a.manifestKey(m.ID), b)
	fakePut(a.baseTarKey(m.ID, m.Sealed), []byte("tar"))
}

// The IDs are wall-clock stamps and the clock can step backwards, so the backup with the later
// ID can be the older one. Order, "latest" and prune must follow the log position instead.
func TestBackupsFollowLogPositionNotIDs(t *testing.T) {
	a, fake, _ := fakeArchiver(t, false)
	ctx := context.Background()
	put := func(m Manifest) { putBackup(t, a, fake.Put, m) }

	// Later ID, earlier log position.
	put(Manifest{ID: "20261002T000000Z", Timeline: 1, StartLSN: "0/1000000", StartWAL: "000000010000000000000001"})
	// Earlier ID, later log position: the newest backup.
	put(Manifest{ID: "20261001T000000Z", Timeline: 1, StartLSN: "0/5000000", StartWAL: "000000010000000000000005"})

	backups, err := a.Backups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 || backups[0].ID != "20261002T000000Z" || backups[1].ID != "20261001T000000Z" {
		t.Fatalf("catalog order %v; want oldest by log position first", ids(backups))
	}

	latest, err := chooseBackup(backups, "")
	if err != nil || latest.ID != "20261001T000000Z" {
		t.Fatalf("latest is %q (%v); want the backup at the later log position", latest.ID, err)
	}
	if got, err := chooseBackup(backups, "latest"); err != nil || got.ID != "20261001T000000Z" {
		t.Fatalf("latest by name is %q (%v)", got.ID, err)
	}
	if got, err := chooseBackup(backups, "20261002T000000Z"); err != nil || got.ID != "20261002T000000Z" {
		t.Fatalf("explicit backup is %q (%v)", got.ID, err)
	}

	res, err := a.Prune(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.RemovedBackups, ",") != "20261002T000000Z" || strings.Join(res.Kept, ",") != "20261001T000000Z" {
		t.Fatalf("prune removed %v and kept %v; the backup at the later log position must survive", res.RemovedBackups, res.Kept)
	}
	if _, ok := fake.Object(a.manifestKey("20261001T000000Z")); !ok {
		t.Fatal("the newest backup by log position was deleted")
	}
}

// Timelines are not comparable by LSN: a backup on a newer timeline is the live lineage, even
// when its LSN is smaller than one on the abandoned branch.
func TestBackupsOrderTimelinesBeforeLogPositions(t *testing.T) {
	a, fake, _ := fakeArchiver(t, false)
	put := func(m Manifest) { putBackup(t, a, fake.Put, m) }
	put(Manifest{ID: "20261001T000000Z", Timeline: 1, StartLSN: "0/9000000", StartWAL: "000000010000000000000009"})
	put(Manifest{ID: "20261002T000000Z", Timeline: 2, StartLSN: "0/2000000", StartWAL: "000000020000000000000002"})

	backups, err := a.Backups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if latest, _ := chooseBackup(backups, ""); latest.ID != "20261002T000000Z" {
		t.Fatalf("latest is %q; want the backup on the newer timeline", latest.ID)
	}
}

// Prune must not delete the older backups while the newest one has no archive: then the older
// ones may be the only usable base.
func TestPruneKeepsOlderBackupsWhenTheNewestHasNoArchive(t *testing.T) {
	a, fake, _ := fakeArchiver(t, false)
	ctx := context.Background()
	putBackup(t, a, fake.Put, Manifest{ID: "20261001T000000Z", Timeline: 1, StartLSN: "0/1000000", StartWAL: "000000010000000000000001"})
	// The manifest is there but its base.tar is not.
	b, _ := json.Marshal(Manifest{ID: "20261002T000000Z", Timeline: 1, StartLSN: "0/5000000", StartWAL: "000000010000000000000005"})
	fake.Put(a.manifestKey("20261002T000000Z"), b)

	if _, err := a.Prune(ctx, 1); err == nil {
		t.Fatal("prune deleted backups while the newest backup had no archive")
	}
	if _, ok := fake.Object(a.manifestKey("20261001T000000Z")); !ok {
		t.Fatal("the only usable older backup was deleted")
	}
}

func ids(ms []Manifest) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
