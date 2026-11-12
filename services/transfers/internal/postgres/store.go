// Package postgres é o adaptador de persistência do transfers (ADR-0007): aceite idempotente,
// consulta com histórico, limite diário e as escritas da saga, todas com compare-and-set e
// fencing token. Usa as queries geradas pelo sqlc.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres/sqlcgen"
)

var errCorruptRow = errors.New("postgres: linha inválida no banco")

// Store implementa pix.Store e as escritas da saga.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

var _ pix.Store = (*Store)(nil)

// NewStore cria o Store sobre um pool já aberto (o dono do pool é o main).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: sqlcgen.New(pool)}
}

// withTx executa fn numa transação: commit se fn devolver nil, rollback caso contrário ou em
// panic. As constraints adiadas (FK da chave de idempotência) são conferidas no COMMIT.
func (s *Store) withTx(ctx context.Context, fn func(q *sqlcgen.Queries) error) (err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: iniciando transação: %w", err)
	}
	defer func() {
		rctx := context.WithoutCancel(ctx)
		if p := recover(); p != nil {
			_ = tx.Rollback(rctx)
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(rctx)
		}
	}()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

// Accept grava o aceite (ADR-0007, seção 4): reserva a chave primeiro; se ela já existe,
// compara o HMAC e devolve a transferência existente ou ErrIdempotencyKeyReused.
func (s *Store) Accept(ctx context.Context, rec pix.AcceptRecord) (pix.Transfer, bool, error) {
	t := rec.Transfer
	var existing uuid.UUID
	err := s.withTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.ClaimIdempotencyKey(ctx, sqlcgen.ClaimIdempotencyKeyParams{
			AccountID: t.SourceAccountID, Key: string(rec.Key), RequestHmac: rec.RequestHMAC, TransferID: t.ID,
		})
		if err != nil {
			return fmt.Errorf("postgres: reservando chave: %w", err)
		}
		if n == 0 {
			row, err := q.GetIdempotencyKey(ctx, sqlcgen.GetIdempotencyKeyParams{AccountID: t.SourceAccountID, Key: string(rec.Key)})
			if err != nil {
				return fmt.Errorf("postgres: lendo chave: %w", err)
			}
			if !hmacEqual(row.RequestHmac, rec.RequestHMAC) {
				return pix.ErrIdempotencyKeyReused
			}
			existing = row.TransferID
			return nil
		}
		carrier, err := json.Marshal(t.Trace)
		if err != nil {
			return fmt.Errorf("postgres: contexto de trace: %w", err)
		}
		if t.Trace == nil {
			carrier = []byte("{}")
		}
		if err := q.InsertTransfer(ctx, sqlcgen.InsertTransferParams{
			ID: t.ID, SourceAccountID: t.SourceAccountID,
			DestinationKeyType: t.DestinationKey.Type().String(), DestinationKey: t.DestinationKey.Value(),
			AmountCents: t.AmountCents, Description: t.Description, State: t.State.String(),
			SettlementID: t.SettlementID, LimitDay: dayToDB(t.LimitDay), TraceCarrier: carrier,
			NextRunAt: rec.NextRunAt, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		}); err != nil {
			return fmt.Errorf("postgres: gravando transferência: %w", err)
		}
		return q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{
			TransferID: t.ID, ToState: t.State.String(), At: t.CreatedAt,
		})
	})
	if err != nil {
		return pix.Transfer{}, false, err
	}
	if existing != (uuid.UUID{}) {
		got, _, err := s.Get(ctx, t.SourceAccountID, existing)
		return got, false, err
	}
	return t, true, nil
}

// Get devolve a transferência da conta e o histórico. ErrNotFound se não existir ou se for de
// outra conta: não revela que existe.
func (s *Store) Get(ctx context.Context, accountID, id uuid.UUID) (pix.Transfer, []pix.Change, error) {
	row, err := s.q.GetTransfer(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return pix.Transfer{}, nil, pix.ErrNotFound
	}
	if err != nil {
		return pix.Transfer{}, nil, fmt.Errorf("postgres: lendo transferência %s: %w", id, err)
	}
	if row.SourceAccountID != accountID {
		return pix.Transfer{}, nil, pix.ErrNotFound
	}
	t, err := transferFromDB(row)
	if err != nil {
		return pix.Transfer{}, nil, err
	}
	rows, err := s.q.ListHistory(ctx, id)
	if err != nil {
		return pix.Transfer{}, nil, fmt.Errorf("postgres: lendo histórico de %s: %w", id, err)
	}
	history := make([]pix.Change, 0, len(rows))
	for _, r := range rows {
		c := pix.Change{At: r.At.UTC()}
		if r.FromState.Valid {
			if c.From, err = pix.ParseState(r.FromState.String); err != nil {
				return pix.Transfer{}, nil, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
			}
		}
		if c.To, err = pix.ParseState(r.ToState); err != nil {
			return pix.Transfer{}, nil, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
		}
		if c.Reason, err = pix.ParseReason(r.Reason.String); err != nil {
			return pix.Transfer{}, nil, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
		}
		history = append(history, c)
	}
	return t, history, nil
}

// UsedOn devolve o limite reservado da conta no dia.
func (s *Store) UsedOn(ctx context.Context, accountID uuid.UUID, day pix.Day) (int64, error) {
	used, err := s.q.UsedOn(ctx, sqlcgen.UsedOnParams{AccountID: accountID, Day: dayToDB(day)})
	if err != nil {
		return 0, fmt.Errorf("postgres: lendo limite usado: %w", err)
	}
	return used, nil
}

// Acquire toma o lease de uma transferência não terminal, se ele estiver livre ou expirado.
// ErrLeaseLost se outro runner a tem ou se ela já terminou.
func (s *Store) Acquire(ctx context.Context, id uuid.UUID, owner string, ttl time.Duration) (pix.Lease, error) {
	token, err := s.q.AcquireLease(ctx, sqlcgen.AcquireLeaseParams{
		Owner: pgtype.Text{String: owner, Valid: true}, Ttl: pgtype.Interval{Microseconds: ttl.Microseconds(), Valid: true}, ID: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return pix.Lease{}, pix.ErrLeaseLost
	}
	if err != nil {
		return pix.Lease{}, fmt.Errorf("postgres: tomando lease de %s: %w", id, err)
	}
	return pix.Lease{TransferID: id, Token: token}, nil
}

// ReserveLimit reserva o valor do Pix no limite do dia gravado na transferência (ADR-0007,
// seção 6), de forma atômica: dois Pix simultâneos nunca passam juntos do limite. Idempotente:
// se já reservou, não reserva de novo. ErrLimitExceeded se não couber; ErrLeaseLost se o lease
// passou para outro runner.
func (s *Store) ReserveLimit(ctx context.Context, lease pix.Lease, dailyLimitCents int64, at time.Time) error {
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		row, err := lockLeased(ctx, q, lease)
		if err != nil || row.LimitReserved {
			return err
		}
		if err := q.EnsureUsageRow(ctx, sqlcgen.EnsureUsageRowParams{AccountID: row.SourceAccountID, Day: row.LimitDay}); err != nil {
			return fmt.Errorf("postgres: criando uso do dia: %w", err)
		}
		n, err := q.ReserveUsage(ctx, sqlcgen.ReserveUsageParams{
			AmountCents: row.AmountCents, AccountID: row.SourceAccountID, Day: row.LimitDay, DailyLimitCents: dailyLimitCents,
		})
		if err != nil {
			return fmt.Errorf("postgres: reservando limite: %w", err)
		}
		if n == 0 {
			return pix.ErrLimitExceeded
		}
		return markReserved(ctx, q, lease, true, at)
	})
}

// Transition aplica uma transição da saga: compare-and-set no estado e no fencing token, linha
// no histórico e, se o estado de chegada devolve o limite (recusa ou estorno), devolução ao dia
// da reserva, tudo na mesma transação.
func (s *Store) Transition(ctx context.Context, lease pix.Lease, tr pix.Transition) error {
	if err := tr.Validate(); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		row, err := lockLeased(ctx, q, lease)
		if err != nil {
			return err
		}
		if row.State != tr.From.String() {
			// O lease é nosso, mas o estado não é o que achamos: nunca deveria acontecer.
			return fmt.Errorf("%w: estado gravado %s, esperado %s", pix.ErrInvalidTransition, row.State, tr.From)
		}
		if tr.ReleasesLimit() && row.LimitReserved {
			if _, err := q.ReleaseUsage(ctx, sqlcgen.ReleaseUsageParams{
				AmountCents: row.AmountCents, AccountID: row.SourceAccountID, Day: row.LimitDay,
			}); err != nil {
				return fmt.Errorf("postgres: devolvendo limite: %w", err)
			}
			if err := markReserved(ctx, q, lease, false, tr.At); err != nil {
				return err
			}
		}
		reason := pgtype.Text{String: tr.Reason.String(), Valid: tr.Reason != 0}
		n, err := q.TransitionTransfer(ctx, sqlcgen.TransitionTransferParams{
			ToState: tr.To.String(), Reason: reason,
			DestinationAccountID: uuidToDB(tr.DestinationAccountID), DebitEntryID: uuidToDB(tr.DebitEntryID),
			PendingSince: pendingSince(tr),
			NextRunAt:    tr.NextRunAt, UpdatedAt: tr.At,
			ID: lease.TransferID, FromState: tr.From.String(), LeaseToken: lease.Token,
		})
		if err != nil {
			return fmt.Errorf("postgres: transição %s -> %s: %w", tr.From, tr.To, err)
		}
		if n != 1 {
			return pix.ErrLeaseLost
		}
		return q.InsertHistory(ctx, sqlcgen.InsertHistoryParams{
			TransferID: lease.TransferID, FromState: pgtype.Text{String: tr.From.String(), Valid: true},
			ToState: tr.To.String(), Reason: reason, At: tr.At,
		})
	})
}

func lockLeased(ctx context.Context, q *sqlcgen.Queries, lease pix.Lease) (sqlcgen.LockLeasedRow, error) {
	row, err := q.LockLeased(ctx, sqlcgen.LockLeasedParams{ID: lease.TransferID, LeaseToken: lease.Token})
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlcgen.LockLeasedRow{}, pix.ErrLeaseLost
	}
	if err != nil {
		return sqlcgen.LockLeasedRow{}, fmt.Errorf("postgres: travando %s: %w", lease.TransferID, err)
	}
	return row, nil
}

func markReserved(ctx context.Context, q *sqlcgen.Queries, lease pix.Lease, reserved bool, at time.Time) error {
	n, err := q.MarkLimitReserved(ctx, sqlcgen.MarkLimitReservedParams{
		LimitReserved: reserved, UpdatedAt: at, ID: lease.TransferID, LeaseToken: lease.Token,
	})
	if err != nil {
		return fmt.Errorf("postgres: marcando reserva: %w", err)
	}
	if n != 1 {
		return pix.ErrLeaseLost
	}
	return nil
}

// pendingSince marca a entrada em PENDING_CONFIRMATION; nas outras transições, mantém o gravado.
func pendingSince(tr pix.Transition) pgtype.Timestamptz {
	if tr.To != pix.StatePendingConfirmation {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: tr.At, Valid: true}
}
