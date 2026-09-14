package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yuluo688/credit-manager/internal/lockfile"
)

func TestOpenLockedHandsDatabaseToNewPluginInstance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credit-manager.db")
	opts := OpenOptions{BusyTimeout: time.Second}

	first, err := OpenLocked(ctx, path, opts, lockfile.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if _, err := first.CreateCaller(ctx, CallerSpec{ID: "handover", QuotaMicroUSD: 0, Enabled: true}); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	second, err := OpenLocked(ctx, path, opts, lockfile.New())
	if err != nil {
		t.Fatalf("open replacement store: %v", err)
	}
	defer second.Close()
	if elapsed := time.Since(started); elapsed >= storeOpenHandoverTimeout {
		t.Fatalf("database handover took %v", elapsed)
	}

	if _, err := first.GetCaller(ctx, "handover"); err == nil {
		t.Fatal("retired store still serves queries")
	}
	got, err := second.GetCaller(ctx, "handover")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "handover" {
		t.Fatalf("replacement store lost persisted caller: %+v", got)
	}
	if second.lease == nil || !second.lease.current() {
		t.Fatal("replacement store does not own the handover lease")
	}
}

func TestOpenLockedAcquiresImmediatelyWhenUnlocked(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credit-manager.db")
	started := time.Now()
	st, err := OpenLocked(ctx, path, OpenOptions{BusyTimeout: time.Second}, lockfile.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// SQLite migration and CI scheduling can take longer than half of the
	// handover timeout even when no lock is contended. The full timeout remains
	// the meaningful boundary between an immediate acquire and handover wait.
	if elapsed := time.Since(started); elapsed >= storeOpenHandoverTimeout {
		t.Fatalf("uncontended open took %v", elapsed)
	}
}

func TestOpenLockedIgnoresForeignProcessHandover(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credit-manager.db")
	st, err := OpenLocked(ctx, path, OpenOptions{BusyTimeout: time.Second}, lockfile.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.CreateCaller(ctx, CallerSpec{ID: "keep", QuotaMicroUSD: 0, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HandoverPath(path), []byte("99999-deadbeef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * storeLeasePollInterval)
	if _, err := st.GetCaller(ctx, "keep"); err != nil {
		t.Fatalf("foreign handover closed the store: %v", err)
	}
}

func TestOpenLockedHandoverTimesOutAgainstLegacyHolder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "credit-manager.db")
	unlock, err := lockfile.New().Lock(ctx, LockPath(path))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	started := time.Now()
	_, err = OpenLocked(ctx, path, OpenOptions{BusyTimeout: time.Second}, lockfile.New())
	if err == nil {
		t.Fatal("expected handover timeout against a legacy lock holder")
	}
	if elapsed := time.Since(started); elapsed < storeOpenHandoverTimeout {
		t.Fatalf("handover returned too quickly: %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "handover timed out") {
		t.Fatalf("error = %v, want handover timeout", err)
	}
}

func TestCanonicalDatabasePathResolvesDirectoryAndFileSymlinks(t *testing.T) {
	realDir := t.TempDir()
	realPath := filepath.Join(realDir, "credit-manager.db")
	if err := os.WriteFile(realPath, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliasDir := filepath.Join(t.TempDir(), "database-dir")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	aliasPath := filepath.Join(aliasDir, "credit-manager.db")
	gotReal, err := CanonicalDatabasePath(realPath)
	if err != nil {
		t.Fatal(err)
	}
	gotAlias, err := CanonicalDatabasePath(aliasPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotAlias != gotReal {
		t.Fatalf("canonical alias = %q, want %q", gotAlias, gotReal)
	}

	fileAlias := filepath.Join(t.TempDir(), "credit-manager-alias.db")
	if err := os.Symlink(realPath, fileAlias); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	gotFileAlias, err := CanonicalDatabasePath(fileAlias)
	if err != nil {
		t.Fatal(err)
	}
	if gotFileAlias != gotReal {
		t.Fatalf("canonical file alias = %q, want %q", gotFileAlias, gotReal)
	}
}
