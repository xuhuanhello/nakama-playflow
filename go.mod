module github.com/xuhuanhello/nakama-playflow

go 1.27.1

require (
	github.com/heroiclabs/nakama-common v1.48.0
	github.com/jackc/pgx/v5 v5.11.0
)

// Nakama 3.41.0 shares these packages with the plugin. Keep the selected
// versions identical to Nakama's go.mod, including indirect dependencies.
require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
)
