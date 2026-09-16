package runv4

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"main/utils/structs"
)

// TestDecryptFailureLeavesNoDeliverable pins the reason the decrypted output is
// staged in a temp file and renamed into place: a decrypt that fails must leave
// nothing at the deliverable path, in EITHER memory mode.
//
// Before this, the streaming branch (taken for every file above
// max-memory-limit — i.e. every hi-res track at the shipped 64 MB limit) wrote
// straight to `outfile` as it decrypted, so a run that died mid-file left a
// TRUNCATED .m4a at the real path. Nothing downstream can tell that apart from
// a finished track: the torrent builder and the FLAC transcode both glob *.m4a.
func TestDecryptFailureLeavesNoDeliverable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		limitMB  int
		totalLen int64
	}{
		{"streaming (file above the limit)", 1, 5 << 20},
		{"buffered (file below the limit)", 64, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "01. Track.m4a")
			cfg := structs.ConfigSet{}
			cfg.MaxMemoryLimit = tc.limitMB
			cfg.LiteServer = "http://127.0.0.1:1" // never reached: input fails first
			err := downloadAndDecryptFile(cfg.LiteServer,
				bytes.NewReader([]byte("this is not an mp4 init segment")),
				out, "1", nil, tc.totalLen, "", cfg)
			if err == nil {
				t.Fatal("expected the garbage input to fail the decrypt")
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatalf("a failed decrypt left something at the deliverable path: %v", statErr)
			}
			entries, rerr := os.ReadDir(dir)
			if rerr != nil {
				t.Fatal(rerr)
			}
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".amdl-part-") {
					t.Fatalf("failed decrypt left a temp part behind: %s", e.Name())
				}
			}
		})
	}
}

// TestCleanStaleParts pins the three behaviours that make the cleanup of
// abandoned temp files safe: a part left by a killed run (SIGKILL/SIGTERM skip
// the deferred remove) goes, a part another amdl may still be writing stays
// (age guard), and anything that is not a part — including a directory that
// merely shares the prefix — is never touched.
func TestCleanStaleParts(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, ".amdl-part-old")
	fresh := filepath.Join(dir, ".amdl-part-fresh")
	real := filepath.Join(dir, "05. Clocks.m4a")
	for _, p := range []string{old, fresh, real} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, ".amdl-part-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	cleanStaleParts(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale part survived cleanup: %v", err)
	}
	for _, p := range []string{fresh, real} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was deleted but must not be: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".amdl-part-dir")); err != nil {
		t.Fatalf("a directory was deleted: %v", err)
	}
}
