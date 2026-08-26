package restore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCRIULogTailReadsRestoreLog(t *testing.T) {
	work := t.TempDir()
	body := "(00.123) Error (criu/memfd.c:210): memfd: Can't restore inode\n"
	if err := os.WriteFile(filepath.Join(work, CRIURestoreLogName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := CRIULogTail(work, 8192)
	if !strings.Contains(got, "Can't restore inode") {
		t.Fatalf("tail did not include the log body: %q", got)
	}
	if !strings.Contains(got, CRIURestoreLogName) {
		t.Fatalf("tail did not name the log file: %q", got)
	}
}

func TestCRIULogTailTruncatesToLastBytes(t *testing.T) {
	work := t.TempDir()
	head := strings.Repeat("x", 4096)
	tail := "THE-INTERESTING-PART"
	if err := os.WriteFile(filepath.Join(work, CRIURestoreLogName), []byte(head+tail), 0o644); err != nil {
		t.Fatal(err)
	}

	got := CRIULogTail(work, 64)
	if !strings.Contains(got, tail) {
		t.Fatalf("tail dropped the end of the file: %q", got)
	}
	if len(got) > 512 {
		t.Fatalf("tail returned %d bytes, want the last 64 plus a short prefix", len(got))
	}
}

// A CRIU that dies before opening its log leaves nothing to read. The
// inventory is what makes that case distinguishable from "CRIU wrote a log
// and it was empty" after the fact.
func TestCRIULogTailFallsBackToWorkDirInventory(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "restore-output.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := CRIULogTail(work, 8192)
	if !strings.Contains(got, "restore-output.log(5)") {
		t.Fatalf("inventory missing the file and its size: %q", got)
	}
	if !strings.Contains(got, "no criu "+CRIURestoreLogName) {
		t.Fatalf("inventory did not say the log was absent: %q", got)
	}
}

func TestCRIULogTailUnreadableWorkDir(t *testing.T) {
	got := CRIULogTail(filepath.Join(t.TempDir(), "does-not-exist"), 8192)
	if !strings.Contains(got, "unreadable") {
		t.Fatalf("want an unreadable-work-dir message, got %q", got)
	}
}
