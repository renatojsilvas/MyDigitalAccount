package risk

import "time"

// Window é a janela dos contadores de velocidade: a "última hora".
const Window = time.Hour

// Counters são os contadores brutos de duas janelas fixas consecutivas, como guardados no
// Redis: a atual (que contém o Pix avaliado) e a anterior.
type Counters struct {
	CurrentCount, CurrentSum   int64
	PreviousCount, PreviousSum int64
}

// Velocity é a estimativa da última hora (contando o Pix avaliado).
type Velocity struct {
	Count    int64
	SumCents int64
}

// WindowStart devolve o início da janela fixa que contém t.
func WindowStart(t time.Time) time.Time {
	return t.Truncate(Window)
}

// Estimate aplica a janela deslizante aproximada (sliding window counter, ADR-0008, seção 2):
// a janela anterior entra com o peso da fração dela que ainda está dentro da última hora.
//
//	estimativa = atual + anterior × (tempo que falta para a janela atual acabar) / janela
//
// Tudo em inteiros: a soma é dinheiro (regra 2). O produto cabe em int64 para somas de até
// ~2,5 × 10^15 centavos, muito acima de qualquer conta.
func (c Counters) Estimate(now time.Time) Velocity {
	remaining := int64(WindowStart(now).Add(Window).Sub(now) / time.Second)
	total := int64(Window / time.Second)
	return Velocity{
		Count:    c.CurrentCount + c.PreviousCount*remaining/total,
		SumCents: c.CurrentSum + c.PreviousSum*remaining/total,
	}
}
