package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckpointSQLite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.db")
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(p)+"?_pragma=journal_mode(wal)")
	if err != nil {
		t.Fatal(err)
	}
	db.Exec(`CREATE TABLE t (x TEXT)`)
	db.Exec(`INSERT INTO t VALUES ('hello')`)
	db.Close()
	if err := CheckpointSQLite(p); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p + "-wal"); err == nil && st.Size() > 0 {
		t.Fatalf("WAL not merged: %d bytes", st.Size())
	}
	db, _ = sql.Open("sqlite3", "file:"+filepath.ToSlash(p))
	defer db.Close()
	var x string
	if err := db.QueryRow(`SELECT x FROM t`).Scan(&x); err != nil || x != "hello" {
		t.Fatal(x, err)
	}
}
