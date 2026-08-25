module github.com/mind-vm/sqlb/example/tasks2

go 1.25.7

replace github.com/mind-vm/sqlb => ../../

require (
	github.com/danielgtaylor/huma/v2 v2.39.0
	github.com/jackc/pgx/v5 v5.10.0
	github.com/mind-vm/sqlb v0.0.0-00010101000000-000000000000
	github.com/pressly/goose/v3 v3.27.3
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
)
