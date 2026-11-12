package redisvelocity

import (
	"time"

	"github.com/redis/go-redis/v9"
)

// NewClient cria o cliente do Redis com prazos curtos em tudo (seção 4.3 do plano): um Redis
// lento ou fora do ar deve virar erro rápido, para o antifraude decidir degradado em vez de
// segurar o Pix (ADR-0008, seção 3).
//
//   - Sem retry no cliente (MaxRetries -1): repetir gastaria o prazo inteiro do Evaluate; quem
//     decide repetir é o transfers, que chama de novo com o mesmo Pix.
//   - ContextTimeoutEnabled: o prazo do context vale também para leitura e escrita no socket.
func NewClient(addr string, timeout time.Duration) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:                  addr,
		DialTimeout:           timeout,
		ReadTimeout:           timeout,
		WriteTimeout:          timeout,
		PoolTimeout:           timeout,
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	})
}
