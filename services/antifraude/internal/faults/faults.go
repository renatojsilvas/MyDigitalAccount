// Package faults injeta latência e erros no antifraude, para os testes de resiliência do
// transfers (ADR-0008, seção 5). Os valores vêm do ambiente no boot e podem ser trocados em
// tempo de execução pelo AntifraudeAdminService.
package faults

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// MaxLatency limita a latência injetada: o bastante para estourar qualquer prazo do transfers,
// sem permitir travar o serviço por engano.
const MaxLatency = 30 * time.Second

// ErrInjected é a falha sorteada pela taxa de erro; a borda a traduz para UNAVAILABLE.
var ErrInjected = errors.New("faults: falha injetada")

// Config são as falhas em vigor.
type Config struct {
	Latency   time.Duration
	ErrorRate float64 // de 0 a 1
}

// Validate confere os limites.
func (c Config) Validate() error {
	var errs []error
	if c.Latency < 0 || c.Latency > MaxLatency {
		errs = append(errs, fmt.Errorf("latência deve estar entre 0 e %v", MaxLatency))
	}
	if math.IsNaN(c.ErrorRate) || c.ErrorRate < 0 || c.ErrorRate > 1 {
		errs = append(errs, errors.New("taxa de erro deve estar entre 0 e 1"))
	}
	return errors.Join(errs...)
}

// Injector guarda a configuração corrente; seguro para uso concorrente.
type Injector struct {
	mu   sync.RWMutex
	cfg  Config
	roll func() float64
}

// New cria o injetor com a configuração inicial (já validada pela configuração do serviço).
func New(cfg Config) *Injector {
	//nolint:gosec // sorteio de falha simulada, não segurança
	return &Injector{cfg: cfg, roll: rand.Float64}
}

// WithRoll troca o sorteio, para testes determinísticos.
func (i *Injector) WithRoll(roll func() float64) *Injector {
	i.roll = roll
	return i
}

// Set troca a configuração; devolve erro sem mudar nada se ela for inválida.
func (i *Injector) Set(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	i.mu.Lock()
	i.cfg = cfg
	i.mu.Unlock()
	return nil
}

// Current devolve a configuração em vigor.
func (i *Injector) Current() Config {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.cfg
}

// Apply espera a latência injetada (ou até ctx acabar) e sorteia a falha.
func (i *Injector) Apply(ctx context.Context) error {
	cfg := i.Current()
	if cfg.Latency > 0 {
		t := time.NewTimer(cfg.Latency)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if cfg.ErrorRate > 0 && i.roll() < cfg.ErrorRate {
		return ErrInjected
	}
	return nil
}
