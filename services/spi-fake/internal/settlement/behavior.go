package settlement

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Mode diz como o spi-fake responde a um Settle (contrato: SettleMode).
type Mode int

// Modos.
const (
	// ModeSettle liquida e responde.
	ModeSettle Mode = iota + 1
	// ModeReject recusa e responde.
	ModeReject
	// ModeSettleNoResponse liquida, mas segura a resposta até o prazo do cliente acabar.
	ModeSettleNoResponse
	// ModeNoSettleNoResponse não liquida e segura a resposta até o prazo do cliente acabar.
	ModeNoSettleNoResponse
)

var modeNames = map[Mode]string{
	ModeSettle: "settle", ModeReject: "reject",
	ModeSettleNoResponse: "settle_no_response", ModeNoSettleNoResponse: "no_settle_no_response",
}

// Modes lista os modos, em ordem; usado para validar configuração e expor métricas.
func Modes() []Mode {
	return []Mode{ModeSettle, ModeReject, ModeSettleNoResponse, ModeNoSettleNoResponse}
}

func (m Mode) String() string {
	if n, ok := modeNames[m]; ok {
		return n
	}
	return "unknown"
}

// ParseMode lê o nome de um modo (o mesmo de String).
func ParseMode(s string) (Mode, error) {
	for m, n := range modeNames {
		if n == s {
			return m, nil
		}
	}
	return 0, fmt.Errorf("modo %q desconhecido: use settle, reject, settle_no_response ou no_settle_no_response", s)
}

// MaxLatency limita a latência simulada.
const MaxLatency = 30 * time.Second

// Behavior é o comportamento do simulador.
type Behavior struct {
	Mode Mode
	// FailStatusQueries faz GetSettlement falhar (ErrStatusUnavailable), em qualquer modo.
	FailStatusQueries bool
	// Latency é o atraso antes de decidir cada Settle.
	Latency time.Duration
}

// Validate confere o comportamento.
func (b Behavior) Validate() error {
	var errs []error
	if _, ok := modeNames[b.Mode]; !ok {
		errs = append(errs, errors.New("modo de liquidação desconhecido"))
	}
	if b.Latency < 0 || b.Latency > MaxLatency {
		errs = append(errs, fmt.Errorf("latência deve estar entre 0 e %v", MaxLatency))
	}
	return errors.Join(errs...)
}

// Switch guarda o comportamento corrente; seguro para uso concorrente. Trocado em execução
// pelo SpiAdminService.
type Switch struct {
	mu sync.RWMutex
	b  Behavior
}

// NewSwitch cria o Switch com o comportamento inicial (já validado pela configuração).
func NewSwitch(b Behavior) *Switch { return &Switch{b: b} }

// Set troca o comportamento; inválido, devolve erro sem mudar nada.
func (s *Switch) Set(b Behavior) error {
	if err := b.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.b = b
	s.mu.Unlock()
	return nil
}

// Current devolve o comportamento em vigor.
func (s *Switch) Current() Behavior {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.b
}
