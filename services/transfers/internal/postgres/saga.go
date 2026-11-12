package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres/sqlcgen"
	"conta.local/transfers/internal/saga"
)

var _ saga.Store = (*Store)(nil)

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// Claim toma até max transferências vencidas nos estados dados.
func (s *Store) Claim(ctx context.Context, owner string, ttl time.Duration, states []pix.State, maxJobs int) ([]saga.Job, error) {
	names := make([]string, len(states))
	for i, st := range states {
		names[i] = st.String()
	}
	rows, err := s.q.ClaimDue(ctx, sqlcgen.ClaimDueParams{
		States: names, MaxJobs: int32(min(maxJobs, 1000)), //nolint:gosec // limitado acima
		Owner: pgtype.Text{String: owner, Valid: true}, Ttl: interval(ttl),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: tomando trabalho: %w", err)
	}
	jobs := make([]saga.Job, 0, len(rows))
	for _, r := range rows {
		t, err := transferFromDB(sqlcgen.GetTransferRow{
			ID: r.ID, SourceAccountID: r.SourceAccountID, DestinationKeyType: r.DestinationKeyType,
			DestinationKey: r.DestinationKey, DestinationAccountID: r.DestinationAccountID, AmountCents: r.AmountCents,
			Description: r.Description, State: r.State, Reason: r.Reason, SettlementID: r.SettlementID,
			DebitEntryID: r.DebitEntryID, LimitDay: r.LimitDay, LimitReserved: r.LimitReserved,
			SettlementMaybeSent: r.SettlementMaybeSent, ConfirmationOverdue: r.ConfirmationOverdue,
			TraceCarrier: r.TraceCarrier, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		})
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, saga.Job{Transfer: t, Lease: pix.Lease{TransferID: r.ID, Token: r.LeaseToken}})
	}
	return jobs, nil
}

// Renew estende o lease (heartbeat). ErrLeaseLost se o token não é mais o atual.
func (s *Store) Renew(ctx context.Context, lease pix.Lease, ttl time.Duration) error {
	n, err := s.q.RenewLease(ctx, sqlcgen.RenewLeaseParams{Ttl: interval(ttl), ID: lease.TransferID, LeaseToken: lease.Token})
	return leaseResult(n, err, "renovando lease")
}

// Release solta o lease e agenda a próxima rodada.
func (s *Store) Release(ctx context.Context, lease pix.Lease, nextRunAt time.Time) error {
	n, err := s.q.ReleaseLease(ctx, sqlcgen.ReleaseLeaseParams{NextRunAt: nextRunAt, ID: lease.TransferID, LeaseToken: lease.Token})
	return leaseResult(n, err, "soltando lease")
}

// MarkMaybeSent grava o marcador irreversível antes de a liquidação sair (ADR-0007, seção 5).
func (s *Store) MarkMaybeSent(ctx context.Context, lease pix.Lease, at time.Time) error {
	n, err := s.q.MarkMaybeSent(ctx, sqlcgen.MarkMaybeSentParams{UpdatedAt: at, ID: lease.TransferID, LeaseToken: lease.Token})
	return leaseResult(n, err, "marcando liquidação possivelmente enviada")
}

func leaseResult(n int64, err error, what string) error {
	if err != nil {
		return fmt.Errorf("postgres: %s: %w", what, err)
	}
	if n != 1 {
		return pix.ErrLeaseLost
	}
	return nil
}
