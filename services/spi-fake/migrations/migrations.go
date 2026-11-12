// Package migrations embute no binário as migrations SQL do spi-fake, para que a imagem final
// (sem shell nem ferramentas) consiga aplicá-las com `spi-fake migrate`.
package migrations

import "embed"

// FS contém os arquivos de migration, aplicados em ordem pelo prefixo numérico.
//
//go:embed *.sql
var FS embed.FS
