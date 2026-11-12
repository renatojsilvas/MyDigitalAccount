// Package accountotel é o adaptador de OpenTelemetry da porta account.Observer: transforma
// os eventos de escrita do ledger em métricas de negócio e em atributos do span da chamada.
//
// Regra de cardinalidade (seção 7.2 do plano): rótulos de métrica só com valores de conjunto
// pequeno e fixo (operação, motivo, desfecho, tipo de recusa). O ID da conta vai apenas para
// o span, onde a cardinalidade não custa nada.
package accountotel

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"conta.local/ledger/internal/account"
)

const meterName = "conta.local/ledger/account"

// Chaves dos atributos, prefixadas pelo serviço para não colidir com as da semconv.
const (
	keyOperation = attribute.Key("ledger.operation")
	keyReason    = attribute.Key("ledger.reason")
	keyOutcome   = attribute.Key("ledger.outcome")
	keyRejection = attribute.Key("ledger.rejection")
	keyAccountID = attribute.Key("ledger.account_id")
)

// Observer implementa account.Observer.
type Observer struct {
	entries      metric.Int64Counter
	amount       metric.Int64Counter
	rejections   metric.Int64Counter
	replays      metric.Int64Counter
	keyConflicts metric.Int64Counter
	opened       metric.Int64Counter
}

var _ account.Observer = (*Observer)(nil)

// New cria os instrumentos no MeterProvider dado.
func New(mp metric.MeterProvider) (*Observer, error) {
	m := mp.Meter(meterName)
	var o Observer
	var err error
	if o.entries, err = m.Int64Counter("ledger.entries",
		metric.WithUnit("{entry}"),
		metric.WithDescription("Lançamentos gravados, por operação e motivo.")); err != nil {
		return nil, err
	}
	if o.amount, err = m.Int64Counter("ledger.posted_amount",
		metric.WithUnit("{cent}"),
		metric.WithDescription("Valor movimentado em centavos, por operação e motivo.")); err != nil {
		return nil, err
	}
	if o.rejections, err = m.Int64Counter("ledger.rejections",
		metric.WithUnit("{request}"),
		metric.WithDescription("Escritas recusadas pelo negócio (saldo insuficiente, conta inexistente, CPF já cadastrado).")); err != nil {
		return nil, err
	}
	if o.replays, err = m.Int64Counter("ledger.idempotency.replays",
		metric.WithUnit("{request}"),
		metric.WithDescription("Repetições pela mesma chave, respondidas com o resultado gravado.")); err != nil {
		return nil, err
	}
	if o.keyConflicts, err = m.Int64Counter("ledger.idempotency.conflicts",
		metric.WithUnit("{request}"),
		metric.WithDescription("Chave de idempotência reutilizada com outro pedido.")); err != nil {
		return nil, err
	}
	if o.opened, err = m.Int64Counter("ledger.accounts.opened",
		metric.WithUnit("{account}"),
		metric.WithDescription("Contas abertas.")); err != nil {
		return nil, err
	}
	return &o, nil
}

// WriteFinished registra a escrita nas métricas e no span corrente.
func (o *Observer) WriteFinished(ctx context.Context, ev account.WriteEvent) {
	rejection := rejectionLabel(ev.Rejection)

	span := trace.SpanFromContext(ctx)
	span.SetAttributes(
		keyOperation.String(ev.Operation),
		keyOutcome.String(string(ev.Outcome)),
		keyAccountID.String(ev.AccountID.String()),
	)
	if ev.Reason != "" {
		span.SetAttributes(keyReason.String(ev.Reason))
	}

	op := metric.WithAttributes(keyOperation.String(ev.Operation))
	switch ev.Outcome {
	case account.OutcomeExecuted:
		if ev.Operation == "open_account" {
			o.opened.Add(ctx, 1)
			return
		}
		attrs := metric.WithAttributes(keyOperation.String(ev.Operation), keyReason.String(ev.Reason))
		o.entries.Add(ctx, 1, attrs)
		o.amount.Add(ctx, ev.Amount.Cents(), attrs)
	case account.OutcomeRejected:
		span.SetAttributes(keyRejection.String(rejection))
		o.rejections.Add(ctx, 1, metric.WithAttributes(keyOperation.String(ev.Operation), keyRejection.String(rejection)))
	case account.OutcomeReplayed:
		o.replays.Add(ctx, 1, op)
	case account.OutcomeKeyConflict:
		o.keyConflicts.Add(ctx, 1, op)
	}
}

// rejectionLabel reduz o erro de domínio a um nome de conjunto fixo.
func rejectionLabel(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, account.ErrInsufficientFunds):
		return "insufficient_funds"
	case errors.Is(err, account.ErrAccountNotFound):
		return "account_not_found"
	case errors.Is(err, account.ErrAccountAlreadyExists):
		return "account_already_exists"
	default:
		return "other"
	}
}
