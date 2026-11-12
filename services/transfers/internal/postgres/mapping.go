package postgres

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"conta.local/transfers/internal/pix"
	"conta.local/transfers/internal/postgres/sqlcgen"
)

// hmacEqual compara em tempo constante: a comparação não vaza, pelo tempo, quantos bytes
// batem.
func hmacEqual(a, b []byte) bool { return hmac.Equal(a, b) }

func dayToDB(d pix.Day) pgtype.Date {
	return pgtype.Date{Time: time.Date(d.Year, d.Month, d.D, 0, 0, 0, 0, time.UTC), Valid: true}
}

func dayFromDB(d pgtype.Date) pix.Day {
	y, m, dd := d.Time.Date()
	return pix.Day{Year: y, Month: m, D: dd}
}

func uuidToDB(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != uuid.UUID{}}
}

func uuidFromDB(u pgtype.UUID) uuid.UUID {
	if !u.Valid {
		return uuid.UUID{}
	}
	return u.Bytes
}

// transferFromDB reconstrói a transferência passando os campos pelos construtores do domínio:
// um dado gravado fora das regras vira erro aqui.
func transferFromDB(r sqlcgen.GetTransferRow) (pix.Transfer, error) {
	if r.DestinationKeyType != "cpf" {
		return pix.Transfer{}, fmt.Errorf("%w: tipo de chave %q", errCorruptRow, r.DestinationKeyType)
	}
	key, err := pix.NewKey(pix.KeyCPF, r.DestinationKey)
	if err != nil {
		return pix.Transfer{}, fmt.Errorf("%w: transferência %s: chave inválida", errCorruptRow, r.ID)
	}
	state, err := pix.ParseState(r.State)
	if err != nil {
		return pix.Transfer{}, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
	}
	reason, err := pix.ParseReason(r.Reason.String)
	if err != nil {
		return pix.Transfer{}, fmt.Errorf("%w: %s", errCorruptRow, err.Error())
	}
	var trace map[string]string
	if err := json.Unmarshal(r.TraceCarrier, &trace); err != nil {
		return pix.Transfer{}, fmt.Errorf("%w: contexto de trace de %s: %s", errCorruptRow, r.ID, err.Error())
	}
	return pix.Transfer{
		ID: r.ID, SourceAccountID: r.SourceAccountID, DestinationKey: key,
		DestinationAccountID: uuidFromDB(r.DestinationAccountID), AmountCents: r.AmountCents, Description: r.Description,
		State: state, Reason: reason, SettlementID: r.SettlementID, DebitEntryID: uuidFromDB(r.DebitEntryID),
		LimitDay: dayFromDB(r.LimitDay), LimitReserved: r.LimitReserved, SettlementMaybeSent: r.SettlementMaybeSent,
		ConfirmationOverdue: r.ConfirmationOverdue, Trace: trace,
		CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
	}, nil
}
