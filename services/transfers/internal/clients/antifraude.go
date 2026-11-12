package clients

import (
	"context"

	"github.com/google/uuid"

	antifraudev1 "conta.local/gen/go/conta/antifraude/v1"
	"conta.local/transfers/internal/saga"
)

// Antifraude implementa saga.Antifraud.
type Antifraude struct {
	c   antifraudev1.AntifraudeServiceClient
	dep *Dependency
}

var _ saga.Antifraud = (*Antifraude)(nil)

// NewAntifraude cria o adaptador.
func NewAntifraude(c antifraudev1.AntifraudeServiceClient, dep *Dependency) *Antifraude {
	return &Antifraude{c: c, dep: dep}
}

// Evaluate avalia o Pix. Repetir é seguro: o antifraude conta cada transfer_id uma vez só.
func (a *Antifraude) Evaluate(ctx context.Context, transferID, accountID uuid.UUID, amountCents int64) (bool, error) {
	var resp *antifraudev1.EvaluateResponse
	err := a.dep.call(ctx, func(ctx context.Context) error {
		r, err := a.c.Evaluate(ctx, &antifraudev1.EvaluateRequest{
			TransferId: transferID.String(), AccountId: accountID.String(), AmountCents: amountCents,
		})
		resp = r
		return err
	})
	if err != nil {
		return false, a.dep.unavailable(err)
	}
	return resp.GetDecision() == antifraudev1.Decision_DECISION_APPROVE, nil
}
