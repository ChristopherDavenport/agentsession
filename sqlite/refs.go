package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Refs are the rows of two tables: refs holds each name's target, and
// ref_log every update the store accepted. An update is one immediate
// transaction, the compare-and-swap an UPDATE ... WHERE on the expected
// target and the log row in the same transaction, so a ref and its log
// agree to every reader and a crash leaves both or neither. The tables
// are created by applySchema, so a database an earlier release wrote
// gains them at its first open.
//
// refs.session carries no foreign key: a ref outlives the deletion of
// its session, and resolves to [agentsession.ErrNoSession] while the
// session is gone.

func target(session, entry string) agentsession.RefTarget {
	return agentsession.RefTarget{Session: session, Entry: entry}
}

// ResolveRef implements [agentsession.RefStore].
func (s *Store) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	if err := agentsession.ValidRefName(name); err != nil {
		return agentsession.RefTarget{}, err
	}
	var sess, entry string
	var exists int
	err := s.r.QueryRowContext(ctx, `SELECT refs.session, refs.entry, EXISTS(SELECT 1 FROM sessions WHERE id = refs.session)
		FROM refs WHERE refs.name = ?`, name).Scan(&sess, &entry, &exists)
	if errors.Is(err, sql.ErrNoRows) {
		return agentsession.RefTarget{}, fmt.Errorf("%w: %s", agentsession.ErrNoRef, name)
	}
	if err != nil {
		return agentsession.RefTarget{}, fmt.Errorf("sqlite: resolve ref: %w", err)
	}
	t := target(sess, entry)
	if exists == 0 {
		return t, fmt.Errorf("%w: %s, which ref %s names", agentsession.ErrNoSession, sess, name)
	}
	return t, nil
}

// UpdateRef implements [agentsession.RefStore].
func (s *Store) UpdateRef(ctx context.Context, name string, expected, next agentsession.RefTarget, reason string) error {
	if err := agentsession.ValidRefName(name); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: update ref: %w", err)
	}
	defer tx.Rollback()
	var cur agentsession.RefTarget
	var curSession, curEntry string
	switch err := tx.QueryRowContext(ctx, `SELECT session, entry FROM refs WHERE name = ?`, name).Scan(&curSession, &curEntry); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("sqlite: update ref: %w", err)
	default:
		cur = target(curSession, curEntry)
	}
	if cur != expected {
		return &agentsession.RefMovedError{Name: name, Current: cur}
	}
	if !next.IsZero() {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, next.Session).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", agentsession.ErrNoSession, next.Session)
		}
		if err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
		if next.Entry != "" {
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM entries WHERE session_id = ? AND id = ?`, next.Session, next.Entry).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s in session %s", agentsession.ErrNoEntry, next.Entry, next.Session)
			}
			if err != nil {
				return fmt.Errorf("sqlite: update ref: %w", err)
			}
		}
	}
	if cur == next {
		return nil
	}
	now := stamp(time.Now())
	switch {
	case cur.IsZero():
		rows, err := tx.QueryContext(ctx, `SELECT name FROM refs`)
		if err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
		var names []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return fmt.Errorf("sqlite: update ref: %w", err)
			}
			names = append(names, n)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
		if err := agentsession.RefConflict(name, names); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO refs (name, session, entry, updated_at) VALUES (?, ?, ?, ?)`,
			name, next.Session, next.Entry, now); err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
	case next.IsZero():
		res, err := tx.ExecContext(ctx, `DELETE FROM refs WHERE name = ? AND session = ? AND entry = ?`, name, cur.Session, cur.Entry)
		if err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
		if err := swapped(res); err != nil {
			return err
		}
	default:
		res, err := tx.ExecContext(ctx, `UPDATE refs SET session = ?, entry = ?, updated_at = ? WHERE name = ? AND session = ? AND entry = ?`,
			next.Session, next.Entry, now, name, cur.Session, cur.Entry)
		if err != nil {
			return fmt.Errorf("sqlite: update ref: %w", err)
		}
		if err := swapped(res); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ref_log (name, old_session, old_entry, new_session, new_entry, at, reason) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		name, cur.Session, cur.Entry, next.Session, next.Entry, now, reason); err != nil {
		return fmt.Errorf("sqlite: update ref: log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: update ref: %w", err)
	}
	return nil
}

// swapped checks that a compare-and-swap changed the row it compared.
// The immediate transaction already holds the write lock from the read
// of the target, so it always did; a count of anything else means the
// transaction was not what this code takes it for.
func swapped(res sql.Result) error {
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return fmt.Errorf("sqlite: update ref: the compare-and-swap changed %d rows (%v)", n, err)
	}
	return nil
}

// ListRefs implements [agentsession.RefStore].
func (s *Store) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	return func(yield func(agentsession.Ref, error) bool) {
		if err := agentsession.ValidRefPrefix(prefix); err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		rows, err := s.r.QueryContext(ctx, `SELECT name, session, entry FROM refs WHERE substr(name, 1, ?) = ? ORDER BY name`, len(prefix), prefix)
		if err != nil {
			yield(agentsession.Ref{}, fmt.Errorf("sqlite: list refs: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var r agentsession.Ref
			var sess, entry string
			if err := rows.Scan(&r.Name, &sess, &entry); err != nil {
				yield(agentsession.Ref{}, fmt.Errorf("sqlite: list refs: %w", err))
				return
			}
			r.Target = target(sess, entry)
			if !yield(r, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(agentsession.Ref{}, fmt.Errorf("sqlite: list refs: %w", err))
		}
	}
}

// RefLog implements [agentsession.RefStore].
func (s *Store) RefLog(ctx context.Context, name string) iter.Seq2[agentsession.RefUpdate, error] {
	return func(yield func(agentsession.RefUpdate, error) bool) {
		if err := agentsession.ValidRefName(name); err != nil {
			yield(agentsession.RefUpdate{}, err)
			return
		}
		rows, err := s.r.QueryContext(ctx, `SELECT old_session, old_entry, new_session, new_entry, at, reason FROM ref_log WHERE name = ? ORDER BY id DESC`, name)
		if err != nil {
			yield(agentsession.RefUpdate{}, fmt.Errorf("sqlite: ref log: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			u := agentsession.RefUpdate{Name: name}
			var os, oe, ns, ne, at string
			if err := rows.Scan(&os, &oe, &ns, &ne, &at, &u.Reason); err != nil {
				yield(agentsession.RefUpdate{}, fmt.Errorf("sqlite: ref log: %w", err))
				return
			}
			u.Old, u.New = target(os, oe), target(ns, ne)
			u.Time, _ = time.Parse(time.RFC3339Nano, at)
			if !yield(u, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(agentsession.RefUpdate{}, fmt.Errorf("sqlite: ref log: %w", err))
		}
	}
}
