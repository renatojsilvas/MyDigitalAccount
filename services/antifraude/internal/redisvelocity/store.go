// Package redisvelocity é o adaptador dos contadores de velocidade do antifraude sobre o Redis
// (ADR-0008, seção 2): incremento atômico e idempotente por Pix, com expiração, num script Lua.
package redisvelocity

import (
	"context"
	_ "embed"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"conta.local/antifraude/internal/risk"
)

// Prefixo de todas as chaves do antifraude: uso ("af") e versão do formato ("v1"). Mudar o
// formato é trocar a versão, sem colidir com chaves antigas, que expiram sozinhas.
const prefix = "af:v1:"

// ttl mantém a janela atual e serve de janela anterior para a seguinte; a marca de "já contado"
// vive o mesmo tempo, mais do que qualquer retry de um Pix.
const ttl = 2 * risk.Window

//go:embed record.lua
var recordLua string

// recordScript roda por EVALSHA; se o Redis não tiver o script em cache (reinício), o go-redis
// cai para EVAL e o carrega.
var recordScript = redis.NewScript(recordLua)

// Store implementa risk.VelocityStore.
type Store struct {
	rdb redis.Scripter
}

var _ risk.VelocityStore = (*Store)(nil)

// New cria o Store sobre um cliente já configurado (timeouts e instrumentação são do main).
func New(rdb redis.Scripter) *Store {
	return &Store{rdb: rdb}
}

// Keys são as chaves de um Pix numa janela. Exportado para os testes conferirem a expiração.
func Keys(req risk.Request, now time.Time) []string {
	account := req.AccountID.String()
	cur := strconv.FormatInt(risk.WindowStart(now).Unix(), 10)
	prev := strconv.FormatInt(risk.WindowStart(now).Add(-risk.Window).Unix(), 10)
	return []string{
		prefix + "seen:" + req.TransferID.String(),
		prefix + "cnt:" + account + ":" + cur,
		prefix + "sum:" + account + ":" + cur,
		prefix + "cnt:" + account + ":" + prev,
		prefix + "sum:" + account + ":" + prev,
	}
}

// Record conta o Pix (uma vez só por TransferID) e devolve os contadores das duas janelas.
func (s *Store) Record(ctx context.Context, req risk.Request, now time.Time) (risk.Counters, error) {
	raw, err := recordScript.Run(ctx, s.rdb, Keys(req, now),
		req.AmountCents, int64(ttl/time.Second)).StringSlice()
	if err != nil {
		return risk.Counters{}, fmt.Errorf("redisvelocity: registrando Pix: %w", err)
	}
	if len(raw) != 4 {
		return risk.Counters{}, fmt.Errorf("redisvelocity: script devolveu %d valores, esperava 4", len(raw))
	}
	var vals [4]int64
	for i, r := range raw {
		if vals[i], err = strconv.ParseInt(r, 10, 64); err != nil {
			return risk.Counters{}, fmt.Errorf("redisvelocity: contador ilegível %q: %w", r, err)
		}
	}
	return risk.Counters{
		CurrentCount: vals[0], CurrentSum: vals[1],
		PreviousCount: vals[2], PreviousSum: vals[3],
	}, nil
}
