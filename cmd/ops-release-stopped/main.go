package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yuluo688/credit-manager/internal/lockfile"
	"github.com/yuluo688/credit-manager/internal/store"
)

const (
	writerLockTimeout = 2 * time.Second
)

type releaseResult struct {
	Database               string   `json:"database"`
	ReleasedCount          int      `json:"released_count"`
	ReleasedReservationIDs []string `json:"released_reservation_ids"`
	FinishedCount          int      `json:"finished_count"`
	FinishedReservationIDs []string `json:"finished_reservation_ids"`
}

func main() {
	database := flag.String("database", "", "Path to the stopped CPA credit-manager SQLite database")
	confirmStopped := flag.Bool("confirm-stopped", false, "Confirm that the CPA host is stopped")
	flag.Parse()
	if !*confirmStopped {
		fatal(errors.New("refusing to modify reservations without --confirm-stopped"))
	}
	result, err := releaseStopped(*database)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatal(fmt.Errorf("encode result: %w", err))
	}
	fmt.Println(string(encoded))
}

func releaseStopped(databasePath string) (releaseResult, error) {
	var err error
	databasePath, err = canonicalDatabasePath(databasePath)
	if err != nil {
		return releaseResult{}, err
	}

	lockCtx, cancelLock := context.WithTimeout(context.Background(), writerLockTimeout)
	defer cancelLock()
	unlock, err := lockfile.New().Lock(lockCtx, store.LockPath(databasePath))
	if err != nil {
		return releaseResult{}, fmt.Errorf("database writer lock is held; stop CPA before recovery: %w", err)
	}
	defer func() { _ = unlock() }()

	ctx := context.Background()
	s, err := store.Open(ctx, databasePath, store.OpenOptions{BusyTimeout: 5 * time.Second})
	if err != nil {
		return releaseResult{}, fmt.Errorf("open stopped-host database: %w", err)
	}
	defer s.Close()

	unfinished, err := heldReservationIDs(ctx, s, true)
	if err != nil {
		return releaseResult{}, err
	}
	finished, err := heldReservationIDs(ctx, s, false)
	if err != nil {
		return releaseResult{}, err
	}
	released := append([]string{}, unfinished...)
	released = append(released, finished...)
	if err := s.ReleaseHeldReservations(ctx, released, "host_stopped_recovery"); err != nil {
		return releaseResult{}, fmt.Errorf("release stopped-host reservations: %w", err)
	}
	return releaseResult{
		Database:               databasePath,
		ReleasedCount:          len(released),
		ReleasedReservationIDs: released,
		FinishedCount:          len(finished),
		FinishedReservationIDs: append([]string{}, finished...),
	}, nil
}

func canonicalDatabasePath(databasePath string) (string, error) {
	databasePath = filepath.Clean(strings.TrimSpace(databasePath))
	if databasePath == "." || databasePath == "" {
		return "", errors.New("database path is required")
	}
	absPath, err := filepath.Abs(databasePath)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		return "", fmt.Errorf("resolve database symlinks: %w", err)
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("stat database: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("database path is a directory: %s", resolvedPath)
	}
	return resolvedPath, nil
}

func heldReservationIDs(ctx context.Context, s *store.Store, unfinished bool) ([]string, error) {
	query := `SELECT id FROM reservations WHERE status = 'held'`
	if unfinished {
		query += ` AND execution_finished_at_unix_ms IS NULL`
	} else {
		query += ` AND execution_finished_at_unix_ms IS NOT NULL`
	}
	query += ` ORDER BY created_at_unix_ms, id`
	rows, err := s.DB().QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list held reservations: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan held reservation: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate held reservations: %w", err)
	}
	return ids, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "ops-release-stopped:", err)
	os.Exit(1)
}
