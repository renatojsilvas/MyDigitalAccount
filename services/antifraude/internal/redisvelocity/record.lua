-- Conta um Pix nos contadores de velocidade da conta, uma única vez por Pix (ADR-0008).
-- O Redis executa o script inteiro sem intercalar outro comando: marca, incrementos e
-- leitura são atômicos entre si.
--
-- KEYS[1] marca "já contado" do Pix   KEYS[2..3] quantidade e soma da janela atual
-- KEYS[4..5] quantidade e soma da janela anterior
-- ARGV[1] valor em centavos           ARGV[2] TTL em segundos
--
-- Num Redis Cluster, estas chaves cairiam em slots diferentes; o projeto usa uma instância só
-- (seção 4.3 do plano). Num cluster, a conta iria numa hash tag ({account}) e a marca também.

if redis.call('SET', KEYS[1], '1', 'NX', 'EX', ARGV[2]) then
  redis.call('INCR', KEYS[2])
  redis.call('EXPIRE', KEYS[2], ARGV[2])
  redis.call('INCRBY', KEYS[3], ARGV[1])
  redis.call('EXPIRE', KEYS[3], ARGV[2])
end

-- Devolve os valores como texto, sem tonumber: número no Lua é double, e a soma é dinheiro
-- (regra 2). A conversão para int64 é feita no Go.
local function raw(key)
  return redis.call('GET', key) or '0'
end

return { raw(KEYS[2]), raw(KEYS[3]), raw(KEYS[4]), raw(KEYS[5]) }
