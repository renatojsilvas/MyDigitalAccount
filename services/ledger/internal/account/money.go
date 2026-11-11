package account

import "math"

// Money é um valor em centavos de real (regra 2: nunca ponto flutuante). O campo não é
// exportado para que um valor só nasça pelos construtores.
type Money struct {
	cents int64
}

// MoneyFromCents reconstrói um valor já validado, como o saldo lido do banco.
func MoneyFromCents(cents int64) Money {
	return Money{cents: cents}
}

// NewAmount valida o valor de uma movimentação: precisa ser maior que zero.
func NewAmount(cents int64) (Money, error) {
	if cents <= 0 {
		return Money{}, invalid("amount_cents", "must be greater than zero")
	}
	return Money{cents: cents}, nil
}

// Cents devolve o valor em centavos.
func (m Money) Cents() int64 {
	return m.cents
}

// LessThan informa se m é menor que o.
func (m Money) LessThan(o Money) bool {
	return m.cents < o.cents
}

// Add soma, recusando estouro de int64 em vez de dar a volta silenciosamente.
func (m Money) Add(o Money) (Money, error) {
	if (o.cents > 0 && m.cents > math.MaxInt64-o.cents) || (o.cents < 0 && m.cents < math.MinInt64-o.cents) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{cents: m.cents + o.cents}, nil
}

// Sub subtrai, recusando estouro de int64.
func (m Money) Sub(o Money) (Money, error) {
	if (o.cents < 0 && m.cents > math.MaxInt64+o.cents) || (o.cents > 0 && m.cents < math.MinInt64+o.cents) {
		return Money{}, ErrMoneyOverflow
	}
	return Money{cents: m.cents - o.cents}, nil
}
