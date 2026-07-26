package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pod32g/omni-logging/internal/model"
	"github.com/pod32g/omni-logging/internal/pipeline"
)

// ListPipelines returns every pipeline in execution order.
func (d *DB) ListPipelines(ctx context.Context) ([]pipeline.Spec, error) {
	rows, err := d.ro.QueryContext(ctx, `
		SELECT id, name, description, match_expr, stages, enabled, sort_order, created_at, updated_at
		FROM pipelines ORDER BY sort_order ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list pipelines: %w", err)
	}
	defer rows.Close()

	var out []pipeline.Spec
	for rows.Next() {
		s, serr := scanPipeline(rows)
		if serr != nil {
			return nil, serr
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetPipeline returns one pipeline by ID.
func (d *DB) GetPipeline(ctx context.Context, id string) (pipeline.Spec, error) {
	row := d.ro.QueryRowContext(ctx, `
		SELECT id, name, description, match_expr, stages, enabled, sort_order, created_at, updated_at
		FROM pipelines WHERE id = ?`, id)
	s, err := scanPipeline(row)
	if errors.Is(err, sql.ErrNoRows) {
		return pipeline.Spec{}, pipeline.ErrNotFound
	}
	return s, err
}

func scanPipeline(sc scanner) (pipeline.Spec, error) {
	var (
		s                pipeline.Spec
		stagesJSON       string
		created, updated int64
	)
	if err := sc.Scan(&s.ID, &s.Name, &s.Description, &s.Match, &stagesJSON,
		&s.Enabled, &s.Order, &created, &updated); err != nil {
		return pipeline.Spec{}, err
	}
	if err := json.Unmarshal([]byte(stagesJSON), &s.Stages); err != nil {
		return pipeline.Spec{}, fmt.Errorf("pipeline %s: decode stages: %w", s.ID, err)
	}
	s.CreatedAt = unixOrZero(created)
	s.UpdatedAt = unixOrZero(updated)
	return s, nil
}

// PutPipeline inserts or replaces a pipeline.
func (d *DB) PutPipeline(ctx context.Context, s pipeline.Spec) (pipeline.Spec, error) {
	now := time.Now().UTC()
	if s.ID == "" {
		s.ID = model.NewID()
		s.CreatedAt = now
	} else if existing, err := d.GetPipeline(ctx, s.ID); err == nil {
		s.CreatedAt = existing.CreatedAt
	} else if !errors.Is(err, pipeline.ErrNotFound) {
		return pipeline.Spec{}, err
	}
	s.UpdatedAt = now

	stages, err := json.Marshal(s.Stages)
	if err != nil {
		return pipeline.Spec{}, err
	}
	if _, err := d.db.ExecContext(ctx, `
		INSERT INTO pipelines (id, name, description, match_expr, stages, enabled, sort_order, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  name=excluded.name, description=excluded.description, match_expr=excluded.match_expr,
		  stages=excluded.stages, enabled=excluded.enabled, sort_order=excluded.sort_order,
		  updated_at=excluded.updated_at`,
		s.ID, s.Name, s.Description, s.Match, string(stages), s.Enabled, s.Order,
		nanosOrZero(s.CreatedAt), nanosOrZero(s.UpdatedAt)); err != nil {
		return pipeline.Spec{}, fmt.Errorf("save pipeline: %w", err)
	}
	return s, nil
}

// DeletePipeline removes a pipeline.
func (d *DB) DeletePipeline(ctx context.Context, id string) error {
	res, err := d.db.ExecContext(ctx, `DELETE FROM pipelines WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete pipeline: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return pipeline.ErrNotFound
	}
	return nil
}
