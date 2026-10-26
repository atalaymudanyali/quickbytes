// Package payprovider holds the SQL files that build the mock payment
// provider's database. It's a separate set from the API's (in the parent
// folder), applied to a separate database: the provider plays an outside
// company, so it shares no tables with the API.
package payprovider

import "embed"

//go:embed *.sql
var FS embed.FS
