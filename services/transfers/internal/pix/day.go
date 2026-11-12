package pix

import (
	"fmt"
	"time"

	// O binário final roda em imagem distroless, sem a base de fusos do sistema: sem a base
	// embutida, LoadLocation falharia só em produção.
	_ "time/tzdata"
)

// LimitZone é o fuso do "dia" do limite diário.
const LimitZone = "America/Sao_Paulo"

// Day é uma data civil (sem hora), no fuso do limite.
type Day struct {
	Year  int
	Month time.Month
	D     int
}

func (d Day) String() string { return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.D) }

// Calendar converte instantes em dias do limite.
type Calendar struct {
	loc *time.Location
}

// NewCalendar carrega o fuso do limite.
func NewCalendar() (Calendar, error) {
	loc, err := time.LoadLocation(LimitZone)
	if err != nil {
		return Calendar{}, fmt.Errorf("pix: carregando o fuso %s: %w", LimitZone, err)
	}
	return Calendar{loc: loc}, nil
}

// DayOf devolve o dia, no fuso do limite, do instante t.
func (c Calendar) DayOf(t time.Time) Day {
	y, m, d := t.In(c.loc).Date()
	return Day{Year: y, Month: m, D: d}
}

// ResetsAt devolve o instante em que o dia acaba (meia-noite seguinte no fuso do limite).
func (c Calendar) ResetsAt(d Day) time.Time {
	return time.Date(d.Year, d.Month, d.D+1, 0, 0, 0, 0, c.loc)
}

// Limits é o limite diário de uma conta num dia.
type Limits struct {
	DailyCents     int64
	UsedCents      int64
	AvailableCents int64
	ResetsAt       time.Time
}

// NewLimits calcula o disponível; nunca negativo, mesmo se o limite configurado baixar.
func NewLimits(daily, used int64, resetsAt time.Time) Limits {
	return Limits{DailyCents: daily, UsedCents: used, AvailableCents: max(daily-used, 0), ResetsAt: resetsAt}
}
