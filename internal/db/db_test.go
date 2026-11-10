package db

import (
	"context"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "dota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func pragma(t *testing.T, d *DB, name string) string {
	t.Helper()
	var v string
	if err := d.Read.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return v
}

func TestOpenAndMigrate(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)

	migs, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	from, to, err := Migrate(ctx, d.Write)
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || to != len(migs) {
		t.Errorf("first Migrate: from=%d to=%d, want 0 and %d", from, to, len(migs))
	}

	if got := pragma(t, d, "journal_mode"); got != "wal" {
		t.Errorf("journal_mode = %q, want wal", got)
	}
	// 2 = INCREMENTAL. Open must enable it before the WAL switch.
	if got := pragma(t, d, "auto_vacuum"); got != "2" {
		t.Errorf("auto_vacuum = %q, want 2 (incremental)", got)
	}
	if _, err := d.Write.Exec("INSERT INTO settings (key, value) VALUES ('k', 'v')"); err != nil {
		t.Errorf("settings table missing: %v", err)
	}

	from, to, err = Migrate(ctx, d.Write)
	if err != nil {
		t.Fatal(err)
	}
	if from != len(migs) || to != len(migs) {
		t.Errorf("second Migrate: from=%d to=%d, want no-op at %d", from, to, len(migs))
	}
}

func TestMigrateRejectsNewerSchema(t *testing.T) {
	d := openTemp(t)
	if _, err := d.Write.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Migrate(context.Background(), d.Write); err == nil {
		t.Fatal("Migrate on a newer schema succeeded, want error")
	}
}

func TestMigrateFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	d := openTemp(t)
	migs := []migration{
		{version: 1, name: "001_ok.sql", sql: "CREATE TABLE a (x INTEGER);"},
		{version: 2, name: "002_bad.sql", sql: "CREATE TABLE b (x INTEGER); NOT SQL;"},
	}
	if _, _, err := apply(ctx, d.Write, migs); err == nil {
		t.Fatal("apply with a broken migration succeeded, want error")
	}
	if v, _ := UserVersion(ctx, d.Write); v != 1 {
		t.Errorf("user_version = %d, want 1", v)
	}
	var n int
	d.Read.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'b'").Scan(&n)
	if n != 0 {
		t.Error("table b from the failed migration was not rolled back")
	}
}

func TestReadPoolIsReadOnly(t *testing.T) {
	d := openTemp(t)
	if _, err := d.Read.Exec("CREATE TABLE t (x INTEGER)"); err == nil {
		t.Fatal("write through the read pool succeeded, want error")
	}
}

func TestLoadMigrations(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	tests := []struct {
		name  string
		files fstest.MapFS
		ok    bool
	}{
		{"sorted and contiguous", fstest.MapFS{
			"migrations/002_b.sql": file("B"),
			"migrations/001_a.sql": file("A"),
		}, true},
		{"gap", fstest.MapFS{
			"migrations/001_a.sql": file("A"),
			"migrations/003_c.sql": file("C"),
		}, false},
		{"duplicate", fstest.MapFS{
			"migrations/001_a.sql": file("A"),
			"migrations/001_b.sql": file("B"),
		}, false},
		{"bad name", fstest.MapFS{
			"migrations/init.sql": file("A"),
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migs, err := loadMigrations(tt.files)
			if (err == nil) != tt.ok {
				t.Fatalf("err = %v, want ok=%v", err, tt.ok)
			}
			if tt.ok && (migs[0].sql != "A" || migs[1].sql != "B") {
				t.Errorf("migrations not sorted by version: %+v", migs)
			}
		})
	}
}
