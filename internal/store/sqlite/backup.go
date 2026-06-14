package sqlite

import (
	"context"
	"fmt"
	"strings"
)

// BackupTo writes a consistent snapshot of the database to dstPath using
// SQLite's online "VACUUM INTO". This is safe to run on a live WAL database
// (unlike copying the file, which can capture a torn WAL state). The
// destination file must not already exist.
func (d *DB) BackupTo(ctx context.Context, dstPath string) error {
	// VACUUM INTO does not reliably accept a bound parameter for the target in
	// all builds, so embed the path as a quoted SQL string literal. The value
	// is operator-supplied (a deploy path), not untrusted request input; we
	// still escape single quotes defensively.
	lit := "'" + strings.ReplaceAll(dstPath, "'", "''") + "'"
	if _, err := d.db.ExecContext(ctx, "VACUUM INTO "+lit); err != nil {
		return fmt.Errorf("backup (VACUUM INTO %s): %w", dstPath, err)
	}
	return nil
}

// IntegrityCheck runs "PRAGMA integrity_check" and reports whether the database
// is sound. When not ok, problems holds the reported issues. A healthy database
// returns ok=true with a single "ok" row.
func (d *DB) IntegrityCheck(ctx context.Context) (ok bool, problems []string, err error) {
	rows, err := d.db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return false, nil, fmt.Errorf("integrity_check: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return false, nil, err
		}
		problems = append(problems, line)
	}
	if err := rows.Err(); err != nil {
		return false, nil, err
	}
	if len(problems) == 1 && strings.EqualFold(problems[0], "ok") {
		return true, nil, nil
	}
	return false, problems, nil
}
