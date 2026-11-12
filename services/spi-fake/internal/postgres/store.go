// Package postgres é o adaptador de persistência do spi-fake: traduz entre o domínio
// (settlement) e a tabela settlements, usando as queries geradas pelo sqlc.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"conta.local/spi-fake/internal/postgres/sqlcgen"
	"conta.local/spi-fake/internal/settlement"
)

// errCorruptRow indica uma linha fora das regras do domínio: dado gravado errado.
var errCorruptRow = errors.New("postgres: linha inválida no banco")

// Store implementa settlement.Store.
type Store struct {
	q *sqlcgen.Queries
}

var _ settlement.Store = (*Store)(nil)

// NewStore cria o Store sobre um pool já aberto (o dono do pool é o main).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{q: sqlcgen.New(pool)}
}

// Insert grava a liquidação se o ID for novo. Um comando só, atômico: dois pedidos
// concorrentes com o mesmo ID nunca gravam dois resultados.
func (s *Store) Insert(ctx context.Context, st settlement.Settlement) (bool, error) {
	status, reason, err := toDB(st)
	if err != nil {
		return false, err
	}
	n, err := s.q.InsertSettlement(ctx, sqlcgen.InsertSettlementParams{
		SettlementID: st.ID, TransferID: st.TransferID, PayerAccountID: st.Payer, PayeeAccountID: st.Payee,
		AmountCents: st.AmountCents, Status: status, RejectReason: reason, DecidedAt: st.DecidedAt,
	})
	if err != nil {
		return false, fmt.Errorf("postgres: gravando liquidação %s: %w", st.ID, err)
	}
	return n == 1, nil
}

// Get lê uma liquidação. settlement.ErrNotFound se não existir.
func (s *Store) Get(ctx context.Context, id uuid.UUID) (settlement.Settlement, error) {
	row, err := s.q.GetSettlement(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return settlement.Settlement{}, settlement.ErrNotFound
	}
	if err != nil {
		return settlement.Settlement{}, fmt.Errorf("postgres: lendo liquidação %s: %w", id, err)
	}
	return fromDB(row)
}

func toDB(st settlement.Settlement) (string, pgtype.Text, error) {
	switch st.Status {
	case settlement.Settled:
		return "settled", pgtype.Text{}, nil
	case settlement.Rejected:
		if st.RejectReason != settlement.RejectSimulated {
			return "", pgtype.Text{}, fmt.Errorf("postgres: motivo de recusa desconhecido: %d", st.RejectReason)
		}
		return "rejected", pgtype.Text{String: "simulated", Valid: true}, nil
	default:
		return "", pgtype.Text{}, fmt.Errorf("postgres: status desconhecido: %d", st.Status)
	}
}

func fromDB(r sqlcgen.Settlement) (settlement.Settlement, error) {
	st := settlement.Settlement{
		Request: settlement.Request{
			ID: r.SettlementID, TransferID: r.TransferID, Payer: r.PayerAccountID, Payee: r.PayeeAccountID,
			AmountCents: r.AmountCents,
		},
		DecidedAt: r.DecidedAt.UTC(),
	}
	switch {
	case r.Status == "settled" && !r.RejectReason.Valid:
		st.Status = settlement.Settled
	case r.Status == "rejected" && r.RejectReason.String == "simulated":
		st.Status, st.RejectReason = settlement.Rejected, settlement.RejectSimulated
	default:
		return settlement.Settlement{}, fmt.Errorf("%w: liquidação %s com status %q", errCorruptRow, r.SettlementID, r.Status)
	}
	return st, nil
}
