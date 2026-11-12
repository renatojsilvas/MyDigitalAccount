package account

import "context"

// Outcome é o desfecho de uma escrita. Os valores são um conjunto pequeno e fixo, porque
// viram rótulo de métrica.
type Outcome string

// Desfechos de uma escrita.
const (
	// OutcomeExecuted: a operação rodou agora e teve efeito.
	OutcomeExecuted Outcome = "executed"
	// OutcomeRejected: a operação rodou agora e foi recusada pelo negócio (gravada).
	OutcomeRejected Outcome = "rejected"
	// OutcomeReplayed: a chave já existia com o mesmo pedido; devolveu o resultado gravado.
	OutcomeReplayed Outcome = "replayed"
	// OutcomeKeyConflict: a chave já existia com outro pedido.
	OutcomeKeyConflict Outcome = "key_conflict"
)

// WriteEvent descreve uma escrita concluída, para métricas de negócio e atributos de trace.
// Erros de infraestrutura não geram evento: aparecem nas métricas RED da borda.
type WriteEvent struct {
	Operation string // open_account, credit ou debit
	Outcome   Outcome
	// Rejection é o erro de domínio quando Outcome é OutcomeRejected.
	Rejection error
	// Reason é o motivo do lançamento (opening, external); vazio na abertura de conta.
	Reason    string
	Amount    Money
	AccountID ID
}

// Observer recebe os eventos de escrita. O adaptador de telemetria o implementa; o domínio
// não conhece OpenTelemetry.
type Observer interface {
	WriteFinished(ctx context.Context, ev WriteEvent)
}

type noopObserver struct{}

func (noopObserver) WriteFinished(context.Context, WriteEvent) {}

// WithObserver liga o Service a um Observer (por padrão, nenhum).
func WithObserver(o Observer) Option {
	return func(s *Service) { s.observer = o }
}

// String devolve o nome estável do motivo, usado em métricas e traces.
func (r CreditReason) String() string {
	switch r {
	case CreditOpening:
		return "opening"
	case CreditExternal:
		return "external"
	default:
		return "unknown"
	}
}

// String devolve o nome estável do motivo, usado em métricas e traces.
func (r DebitReason) String() string {
	if r == DebitExternal {
		return "external"
	}
	return "unknown"
}

// finish emite o evento da escrita. Sem desfecho (erro de infraestrutura), não emite.
func (s *Service) finish(ctx context.Context, ev WriteEvent, outcome Outcome, err error) {
	if outcome == "" {
		return
	}
	ev.Outcome = outcome
	if outcome == OutcomeRejected {
		ev.Rejection = err
	}
	s.observer.WriteFinished(ctx, ev)
}
