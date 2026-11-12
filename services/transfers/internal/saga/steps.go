package saga

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/resilience"
)

// step executa a etapa do estado atual (ADR-0007, seção 3).
func (r *Runner) step(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	switch t.State {
	case pix.StateAccepted:
		return r.validate(ctx, t, lease)
	case pix.StateValidated:
		return r.debit(ctx, t, lease)
	case pix.StateDebited:
		return r.move(ctx, t, lease, pix.Transition{To: pix.StateSettling})
	case pix.StateSettling:
		return r.settle(ctx, t, lease)
	case pix.StateSettled:
		return r.credit(ctx, t, lease)
	case pix.StateReversing:
		return r.reverse(ctx, t, lease)
	default:
		return outcome{kind: kindStop}
	}
}

// rejection é uma recusa de negócio na validação; vira REJECTED com o motivo.
type rejection struct {
	reason pix.Reason
	cause  error
}

func (e *rejection) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("recusa %s: %v", e.reason, e.cause)
	}
	return "recusa " + e.reason.String()
}

func (e *rejection) Unwrap() error { return e.cause }

// validate é o fan-out (ADR-0007, seção 3): antifraude, reserva do limite e conta de destino ao
// mesmo tempo, com errgroup. A primeira recusa cancela as outras chamadas; se a reserva já tinha
// sido feita, a transição para REJECTED a devolve na mesma transação.
func (r *Runner) validate(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	fctx, cancel := context.WithTimeout(ctx, r.cfg.FanOutTimeout)
	defer cancel()
	g, gctx := errgroup.WithContext(fctx)
	var dest uuid.UUID
	reserved := false

	g.Go(func() error {
		approved, err := r.antifraud.Evaluate(gctx, t.ID, t.SourceAccountID, t.AmountCents)
		switch {
		case err != nil:
			// Antifraude fora do ar: recusa; nada foi movido (ADR-0008, seção 4).
			return &rejection{reason: pix.ReasonDependencyUnavailable, cause: fmt.Errorf("antifraude: %w", err)}
		case !approved:
			return &rejection{reason: pix.ReasonAntifraudDenied}
		}
		return nil
	})
	g.Go(func() error {
		err := r.store.ReserveLimit(gctx, lease, r.cfg.DailyLimitCents, r.instant())
		switch {
		case errors.Is(err, pix.ErrLimitExceeded):
			return &rejection{reason: pix.ReasonLimitExceeded}
		case err != nil:
			return err // banco ou lease: a etapa inteira é refeita
		}
		reserved = true
		return nil
	})
	g.Go(func() error {
		id, err := r.ledger.AccountByDocument(gctx, t.DestinationKey.Value())
		switch {
		case errors.Is(err, ErrAccountNotFound):
			return &rejection{reason: pix.ReasonDestinationInvalid}
		case err != nil:
			return &rejection{reason: pix.ReasonDependencyUnavailable, cause: fmt.Errorf("ledger: %w", err)}
		case id == t.SourceAccountID:
			return &rejection{reason: pix.ReasonSameAccount}
		}
		dest = id
		return nil
	})

	err := g.Wait()
	if reserved {
		t.LimitReserved = true
	}
	var rej *rejection
	switch {
	case errors.As(err, &rej):
		out := r.move(ctx, t, lease, pix.Transition{To: pix.StateRejected, Reason: rej.reason})
		if out.err == nil {
			out.err = rej.cause
		}
		return out
	case errors.Is(err, pix.ErrLeaseLost):
		r.observer.LeaseLost(ctx)
		return outcome{kind: kindStop, err: err}
	case err != nil:
		return r.retryLater(err)
	}
	return r.move(ctx, t, lease, pix.Transition{To: pix.StateValidated, DestinationAccountID: dest})
}

// stepKey é a chave de idempotência de cada escrita no ledger: derivada do Pix e da etapa, para
// que uma retomada depois de uma queda nunca debite, credite ou estorne duas vezes.
func stepKey(t *pix.Transfer, step string) string {
	return "transfer:" + t.ID.String() + ":" + step
}

func (r *Runner) debit(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	entry, err := r.ledger.Debit(ctx, stepKey(t, "debit"), t.SourceAccountID, t.AmountCents, t.ID.String(),
		describe("Pix enviado", t.Description))
	switch {
	case errors.Is(err, ErrInsufficientFunds):
		return r.move(ctx, t, lease, pix.Transition{To: pix.StateRejected, Reason: pix.ReasonInsufficientFunds})
	case errors.Is(err, ErrAccountNotFound):
		return r.move(ctx, t, lease, pix.Transition{To: pix.StateRejected, Reason: pix.ReasonSourceAccountInvalid})
	case err != nil:
		// Sem resposta definitiva o débito pode ter acontecido: nunca recusar aqui. A mesma chave
		// resolve na próxima rodada (ADR-0007, seção 3).
		return r.retryLater(err)
	}
	return r.move(ctx, t, lease, pix.Transition{To: pix.StateDebited, DebitEntryID: entry})
}

// settle é o pivô (ADR-0007, seção 5): até N tentativas, sempre com o mesmo settlement_id.
// Antes de cada tentativa que de fato sai, o marcador settlement_maybe_sent é gravado; ele
// nunca volta a falso. Só "o circuito recusou em todas as tentativas e o marcador continua
// falso" prova que não liquidou.
func (r *Runner) settle(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	req := SettleRequest{
		SettlementID: t.SettlementID, TransferID: t.ID, Payer: t.SourceAccountID, Payee: t.DestinationAccountID,
		AmountCents: t.AmountCents,
	}
	markSent := func() error {
		if err := r.store.MarkMaybeSent(ctx, lease, r.instant()); err != nil {
			return err
		}
		t.SettlementMaybeSent = true
		return nil
	}
	var lastErr error
	for attempt := range max(r.cfg.SettleAttempts, 1) {
		if attempt > 0 {
			//nolint:gosec // jitter de espera, não segurança
			if err := sleep(ctx, r.cfg.SettleBackoff.Backoff(attempt-1, rand.Float64)); err != nil {
				return outcome{kind: kindStop, err: err}
			}
		}
		actx, cancel := ctx, context.CancelFunc(func() {})
		if r.cfg.SettleBackoff.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, r.cfg.SettleBackoff.Timeout)
		}
		status, err := r.spi.Settle(actx, req, markSent)
		cancel()
		switch {
		case err == nil && status == Settled:
			r.observer.SettleAttempt(ctx, "settled")
			return r.move(ctx, t, lease, pix.Transition{To: pix.StateSettled})
		case err == nil && status == Rejected:
			r.observer.SettleAttempt(ctx, "rejected")
			return r.move(ctx, t, lease, pix.Transition{To: pix.StateReversing, Reason: pix.ReasonSettlementRejected})
		case errors.Is(err, pix.ErrLeaseLost):
			r.observer.LeaseLost(ctx)
			return outcome{kind: kindStop, err: err}
		case errors.Is(err, resilience.ErrNotSent):
			r.observer.SettleAttempt(ctx, "not_sent")
		default:
			// Timeout, UNAVAILABLE, conexão cortada: pode ter liquidado.
			r.observer.SettleAttempt(ctx, "ambiguous")
		}
		lastErr = err
	}
	if !t.SettlementMaybeSent {
		return r.move(ctx, t, lease, pix.Transition{To: pix.StateReversing, Reason: pix.ReasonSettlementNotSent})
	}
	// Nunca estornar nem concluir sem confirmação (regra 10): fica pendente, com o débito.
	out := r.move(ctx, t, lease, pix.Transition{
		To: pix.StatePendingConfirmation, NextRunAt: r.now().Add(r.cfg.ConfirmAfter),
	})
	if out.err == nil {
		out.err = lastErr
	}
	return out
}

// credit é forward-only (depois do pivô): repete até o ledger responder; nunca compensa.
func (r *Runner) credit(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	err := r.ledger.Credit(ctx, stepKey(t, "credit"), t.DestinationAccountID, t.AmountCents, t.ID.String(),
		describe("Pix recebido", t.Description))
	if err != nil {
		return r.retryLater(err)
	}
	return r.move(ctx, t, lease, pix.Transition{To: pix.StateCompleted})
}

// reverse estorna o débito (compensação). Também só anda para frente: repete até conseguir.
func (r *Runner) reverse(ctx context.Context, t *pix.Transfer, lease pix.Lease) outcome {
	err := r.ledger.Reverse(ctx, stepKey(t, "reversal"), t.DebitEntryID, t.ID.String(), "Estorno do Pix")
	if err != nil {
		return r.retryLater(err)
	}
	return r.move(ctx, t, lease, pix.Transition{To: pix.StateReversed, Reason: t.Reason})
}

// maxLedgerDescription é o limite de texto do extrato no ledger: passar dele seria uma recusa
// por formato, que a etapa repetiria para sempre.
const maxLedgerDescription = 140

// describe monta o texto do extrato, cortado no limite do ledger (em runas, não em bytes).
func describe(prefix, description string) string {
	s := prefix
	if description != "" {
		s += ": " + description
	}
	if r := []rune(s); len(r) > maxLedgerDescription {
		s = string(r[:maxLedgerDescription-1]) + "…"
	}
	return s
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
