//go:build linux

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPGDataPublishPreservesExistingAndUsesNoReplace(t *testing.T) {
	w := t.TempDir()
	c := Config{PGData: filepath.Join(w, "data"), Repository: filepath.Join(w, "repo"), RuntimeDir: filepath.Join(w, "run"), PGHost: filepath.Join(w, "socket"), PGPort: 25474, RestoreToPGData: true}
	r := Runner{c: c}
	stage, final, publish, err := r.combineOutput(c.PGData, false)
	if err != nil {
		t.Fatal(err)
	}
	if stage == final || publish == nil {
		t.Fatal("restore did not stage independently")
	}
	if err = os.WriteFile(filepath.Join(stage, "proof"), []byte("verified-copy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = publish(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(final, "proof")); err != nil || string(b) != "verified-copy" {
		t.Fatal(string(b), err)
	}
	if _, _, _, err = r.combineOutput(c.PGData, false); err == nil {
		t.Fatal("existing PGDATA accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(final, "proof")); string(b) != "verified-copy" {
		t.Fatal("existing data changed")
	}
	other := filepath.Join(w, "other")
	occupied := filepath.Join(w, "occupied")
	os.Mkdir(other, 0700)
	os.Mkdir(occupied, 0700)
	if err = renameNoReplace(other, occupied); err == nil {
		t.Fatal("rename replaced a concurrent empty directory")
	}
}

func TestPGDataRestoreRefusesListeningSocketAndPostmasterMarker(t *testing.T) {
	w := t.TempDir()
	sock := filepath.Join(w, "socket")
	os.Mkdir(sock, 0700)
	c := Config{PGData: filepath.Join(w, "data"), PGHost: sock, PGPort: 25474}
	listener, err := net.Listen("unix", filepath.Join(sock, ".s.PGSQL.25474"))
	if err != nil {
		t.Fatal(err)
	}
	if err = requireEmptyStoppedPGData(c); err == nil {
		t.Fatal("listening database accepted")
	}
	listener.Close()
	os.Mkdir(c.PGData, 0700)
	os.WriteFile(filepath.Join(c.PGData, "postmaster.pid"), []byte("stale or live"), 0600)
	if err = requireEmptyStoppedPGData(c); err == nil {
		t.Fatal("unchecked postmaster marker accepted")
	}
}
