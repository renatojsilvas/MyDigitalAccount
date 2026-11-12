package saga

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/resilience"
)

// Config são os parâmetros do runner (ADR-0007, seções 4, 5 e 7).
type Config struct {
	// Owner identifica a instância no lease (hostname).
	Owner    string
	LeaseTTL time.Duration
	// Workers é quantas transferências andam ao mesmo tempo nesta instância.
	Workers int
	// PollInterval é a varredura que cobre avisos perdidos e retomadas depois de uma queda.
	PollInterval time.Duration
	// RetryDelay é a espera antes de tentar de novo uma etapa que não teve resposta definitiva
	// (ledger fora do ar, por exemplo); a transferência fica no mesmo estado.
	RetryDelay      time.Duration
	DailyLimitCents int64
	// FanOutTimeout limita a validação em paralelo (antifraude, limite e destino).
	FanOutTimeout time.Duration
	// Settle: tentativas com o mesmo settlement_id e espera entre elas.
	SettleAttempts int
	SettleBackoff  resilience.Policy
	// ConfirmAfter é a espera até a primeira consulta de status de um Pix pendente.
	ConfirmAfter time.Duration
}

// runnable são os estados que o runner leva adiante. PENDING_CONFIRMATION é do worker de
// confirmação; os terminais, de ninguém.
var runnable = []pix.State{
	pix.StateAccepted, pix.StateValidated, pix.StateDebited, pix.StateSettling, pix.StateSettled, pix.StateReversing,
}

// Runner leva as transferências pela saga.
type Runner struct {
	cfg       Config
	store     Store
	antifraud Antifraud
	ledger    Ledger
	spi       SPI
	tracing   Tracing
	observer  Observer
	logger    *slog.Logger
	now       func() time.Time
	wake      chan struct{}
}

// Deps são as dependências do runner.
type Deps struct {
	Store     Store
	Antifraud Antifraud
	Ledger    Ledger
	SPI       SPI
	Tracing   Tracing
	Observer  Observer
	Logger    *slog.Logger
	Now       func() time.Time
}

// NewRunner monta o runner.
func NewRunner(cfg Config, d Deps) *Runner {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Runner{
		cfg: cfg, store: d.Store, antifraud: d.Antifraud, ledger: d.Ledger, spi: d.SPI,
		tracing: d.Tracing, observer: d.Observer, logger: d.Logger, now: d.Now,
		wake: make(chan struct{}, 1),
	}
}

// Notify acorda o runner (Pix novo): é o caminho rápido. Nunca bloqueia; um aviso perdido é
// coberto pela varredura.
func (r *Runner) Notify(uuid.UUID) {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run pega e processa transferências até ctx ser cancelado. No desligamento, para de pegar
// trabalho e espera as que estão em andamento terminarem a etapa corrente (cada uma limitada
// pelo lease). Uma queda no meio não perde nada: o lease expira e outra instância retoma do
// estado gravado.
func (r *Runner) Run(ctx context.Context) error {
	sem := make(chan struct{}, r.cfg.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		if free := cap(sem) - len(sem); free > 0 {
			jobs, err := r.store.Claim(ctx, r.cfg.Owner, r.cfg.LeaseTTL, runnable, free)
			if err != nil && ctx.Err() == nil {
				r.logger.ErrorContext(ctx, "saga: tomando trabalho", "error", err.Error())
			}
			for _, j := range jobs {
				sem <- struct{}{}
				wg.Go(func() {
					defer func() { <-sem }()
					r.process(context.WithoutCancel(ctx), j)
				})
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-r.wake:
		}
	}
}

// RunOnce pega e processa, em sequência, tudo o que estiver vencido. Para testes e para
// rodar a saga sob controle.
func (r *Runner) RunOnce(ctx context.Context) (int, error) {
	jobs, err := r.store.Claim(ctx, r.cfg.Owner, r.cfg.LeaseTTL, runnable, 100)
	for _, j := range jobs {
		r.process(ctx, j)
	}
	return len(jobs), err
}

// outcome é o que a etapa decidiu.
type outcome struct {
	kind   outcomeKind
	waitAt time.Time // para kindWait
	err    error     // para o span e o log
}

type outcomeKind int

const (
	kindAdvanced outcomeKind = iota // transitou: segue para a próxima etapa
	kindWait                        // sem resposta definitiva: solta o lease e volta em waitAt
	kindStop                        // lease perdido ou estado parado (terminal, pendente)
)

func (r *Runner) process(ctx context.Context, j Job) {
	ctx, cancel := context.WithTimeout(ctx, r.cfg.LeaseTTL)
	defer cancel()
	t, lease := j.Transfer, j.Lease
	for !t.State.Terminal() && t.State != pix.StatePendingConfirmation {
		if err := r.store.Renew(ctx, lease, r.cfg.LeaseTTL); err != nil {
			r.leaseProblem(ctx, t, err)
			return
		}
		state, start := t.State, time.Now()
		sctx, end := r.tracing.Start(ctx, t.Trace, "pix.saga."+state.String(), t.ID.String())
		out := r.step(sctx, &t, lease)
		end(out.err)
		r.observer.StepFinished(ctx, state, resultOf(out), time.Since(start))
		if out.err != nil && out.kind != kindAdvanced {
			r.logger.WarnContext(sctx, "saga: etapa sem resposta definitiva",
				"pix_id", t.ID.String(), "state", state.String(), "error", out.err.Error())
		}
		switch out.kind {
		case kindAdvanced:
			continue
		case kindWait:
			if err := r.store.Release(ctx, lease, out.waitAt); err != nil {
				r.leaseProblem(ctx, t, err)
			}
			return
		case kindStop:
			return
		}
	}
}

func resultOf(o outcome) string {
	switch o.kind {
	case kindAdvanced:
		return "advanced"
	case kindWait:
		return "retry_later"
	default:
		return "stopped"
	}
}

func (r *Runner) leaseProblem(ctx context.Context, t pix.Transfer, err error) {
	if errors.Is(err, pix.ErrLeaseLost) {
		r.observer.LeaseLost(ctx)
		r.logger.WarnContext(ctx, "saga: lease perdido; outro runner segue com o Pix", "pix_id", t.ID.String())
		return
	}
	// O lease expira sozinho e outra rodada retoma do estado gravado.
	r.logger.ErrorContext(ctx, "saga: escrevendo no banco", "pix_id", t.ID.String(), "error", err.Error())
}

func (r *Runner) instant() time.Time { return r.now().UTC().Truncate(time.Microsecond) }

// retryLater mantém a transferência no estado atual e volta depois.
func (r *Runner) retryLater(err error) outcome {
	return outcome{kind: kindWait, waitAt: r.now().Add(r.cfg.RetryDelay), err: err}
}

// move aplica a transição e atualiza a cópia em memória. Uma falha de escrita vira espera (a
// etapa será refeita, idempotente) ou parada (lease perdido).
func (r *Runner) move(ctx context.Context, t *pix.Transfer, lease pix.Lease, tr pix.Transition) outcome {
	now := r.instant()
	tr.From, tr.At = t.State, now
	if tr.NextRunAt.IsZero() {
		tr.NextRunAt = now
	}
	if err := r.store.Transition(ctx, lease, tr); err != nil {
		if errors.Is(err, pix.ErrLeaseLost) {
			r.observer.LeaseLost(ctx)
			return outcome{kind: kindStop, err: err}
		}
		return r.retryLater(err)
	}
	r.observer.Transitioned(ctx, tr.From, tr.To, tr.Reason)
	if tr.ReleasesLimit() && t.LimitReserved {
		r.observer.Compensated(ctx, "limit_release")
		t.LimitReserved = false
	}
	if tr.To == pix.StateReversed {
		r.observer.Compensated(ctx, "reversal")
	}
	t.State, t.Reason, t.UpdatedAt = tr.To, tr.Reason, now
	if tr.DestinationAccountID != (uuid.UUID{}) {
		t.DestinationAccountID = tr.DestinationAccountID
	}
	if tr.DebitEntryID != (uuid.UUID{}) {
		t.DebitEntryID = tr.DebitEntryID
	}
	if tr.To == pix.StatePendingConfirmation {
		// Soltar o lease: a partir daqui, quem olha é o worker de confirmação.
		if err := r.store.Release(ctx, lease, tr.NextRunAt); err != nil {
			return outcome{kind: kindStop, err: err}
		}
		return outcome{kind: kindStop}
	}
	return outcome{kind: kindAdvanced}
}
