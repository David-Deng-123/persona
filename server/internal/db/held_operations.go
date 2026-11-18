package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ---- held_operations (#10661, internal/devices held.go) ----

// HeldKindMessage is a held_operations row's kind for the owner's message to an existing Agent (a session.send).
const HeldKindMessage = "message"

// A HeldOperation's states.
const (
	HeldWaiting = "waiting" // sent by itself once its computer is back
	HeldSending = "sending" // being sent now; a manager that stopped meanwhile left it so (RecoverHeldOperations)
	HeldFailed  = "failed"  // given up: the owner retries or deletes it
)

// Why a HeldOperation failed.
const (
	HeldUnclear = "unclear" // a try whose outcome is unknown: it may have arrived
	HeldRefused = "refused" // the computer refused it, nothing typed (Error says why)
	HeldExpired = "expired" // its computer was away for a whole day
)

// HeldOperation is a row of held_operations: an operation the owner sent to a computer while it was away, held by its
// manager. Call is the devicewire.NodeCall as the hub sent it, caller and submission included. SentAt is when the
// owner sent it, HeldAt when it began to wait (a Retry starts it again), TriedAt when its latest try began (zero:
// never tried).
type HeldOperation struct {
	Seq       int64
	Kind      string
	OpID      string
	MachineID string
	SessionID string
	Call      json.RawMessage
	Lang      string
	State     string
	Failure   string
	Error     string
	SentAt    time.Time
	HeldAt    time.Time
	TriedAt   time.Time
	Attempts  int
}

const heldColumns = `seq, kind, op_id, machine_id, session_id, call, lang, state, failure, error, sent_at, held_at, tried_at, attempts`

func scanHeld(row pgx.Row) (HeldOperation, error) {
	var o HeldOperation
	var tried *time.Time
	err := row.Scan(&o.Seq, &o.Kind, &o.OpID, &o.MachineID, &o.SessionID, &o.Call, &o.Lang, &o.State, &o.Failure, &o.Error,
		&o.SentAt, &o.HeldAt, &tried, &o.Attempts)
	if tried != nil {
		o.TriedAt = *tried
	}
	return o, err
}

// HoldOperation keeps op, waiting. The same operation held already (kind, machine, session and op id) is not held
// again: that row comes back, with fresh false. A zero SentAt is now, a zero HeldAt its SentAt.
func (d *DB) HoldOperation(ctx context.Context, op HeldOperation) (HeldOperation, bool, error) {
	if op.SentAt.IsZero() {
		op.SentAt = time.Now()
	}
	if op.HeldAt.IsZero() {
		op.HeldAt = op.SentAt
	}
	held, err := scanHeld(d.pool.QueryRow(ctx, `INSERT INTO held_operations
		(kind, op_id, machine_id, session_id, call, lang, sent_at, held_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (kind, machine_id, session_id, op_id) DO NOTHING RETURNING `+heldColumns,
		op.Kind, op.OpID, op.MachineID, op.SessionID, string(op.Call), op.Lang, op.SentAt.UTC(), op.HeldAt.UTC()))
	if err == nil {
		return held, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return HeldOperation{}, false, err
	}
	held, err = scanHeld(d.pool.QueryRow(ctx, `SELECT `+heldColumns+` FROM held_operations
		WHERE kind = $1 AND machine_id = $2 AND session_id = $3 AND op_id = $4`, op.Kind, op.MachineID, op.SessionID, op.OpID))
	return held, false, err
}

// HeldOperations are every held operation of kind, in the order they were held.
func (d *DB) HeldOperations(ctx context.Context, kind string) ([]HeldOperation, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+heldColumns+` FROM held_operations WHERE kind = $1 ORDER BY seq`, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (HeldOperation, error) { return scanHeld(r) })
}

// RecoverHeldOperations fails, as unclear, every operation of kind a manager left being sent: it stopped during the
// try, so whether it arrived is not known, and it is never sent again by itself. It says how many there were.
func (d *DB) RecoverHeldOperations(ctx context.Context, kind string) (int64, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE held_operations SET state = 'failed', failure = 'unclear', error = ''
		WHERE kind = $1 AND state = 'sending'`, kind)
	return tag.RowsAffected(), err
}

// ClaimHeldOperation marks waiting operation seq as being sent from at, before its try: false when it is not waiting
// (gone, or taken already), and then it must not be sent.
func (d *DB) ClaimHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE held_operations SET state = 'sending', tried_at = $2, attempts = attempts + 1
		WHERE seq = $1 AND state = 'waiting'`, seq, at.UTC())
	return tag.RowsAffected() == 1, err
}

// SettleHeldOperation moves operation seq from state from to state (with why it failed and the computer's words):
// false when it was not in from.
func (d *DB) SettleHeldOperation(ctx context.Context, seq int64, from, state, failure, reason string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE held_operations SET state = $3, failure = $4, error = $5
		WHERE seq = $1 AND state = $2`, seq, from, state, failure, reason)
	return tag.RowsAffected() == 1, err
}

// RetryHeldOperation is the owner's Retry on operation seq: waiting again, a new wait from at. False when it is gone.
func (d *DB) RetryHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE held_operations SET state = 'waiting', failure = '', error = '', held_at = $2
		WHERE seq = $1`, seq, at.UTC())
	return tag.RowsAffected() == 1, err
}

// RenewHeldOperation starts waiting operation seq's wait again from at: one held behind a message that may have
// arrived waits for the owner, not for its computer. False when it is not waiting.
func (d *DB) RenewHeldOperation(ctx context.Context, seq int64, at time.Time) (bool, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE held_operations SET held_at = $2 WHERE seq = $1 AND state = 'waiting'`, seq, at.UTC())
	return tag.RowsAffected() == 1, err
}

// DeleteHeldOperation drops operation seq, only while in one of states when any are given. False when no such row
// was there.
func (d *DB) DeleteHeldOperation(ctx context.Context, seq int64, states ...string) (bool, error) {
	var tag pgconn.CommandTag
	var err error
	if len(states) == 0 {
		tag, err = d.pool.Exec(ctx, `DELETE FROM held_operations WHERE seq = $1`, seq)
	} else {
		tag, err = d.pool.Exec(ctx, `DELETE FROM held_operations WHERE seq = $1 AND state = ANY($2)`, seq, states)
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
