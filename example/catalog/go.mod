module github.com/mind-vm/sqlb/example/catalog

go 1.25.7

replace github.com/mind-vm/sqlb => ../../

require (
	github.com/jackc/pgx/v5 v5.10.0
	github.com/mind-vm/sqlb v0.0.0-00010101000000-000000000000
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
