module github.com/dootdotrun/doot-ai

// Patch-level, and not by choice: superfly/sprites-go requires go >= 1.25.8, so
// this cannot be relaxed to plain "go 1.25". The Dockerfile's builder image has
// to satisfy it — see the note there.
go 1.25.8

require (
	github.com/go-chi/chi/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/microcosm-cc/bluemonday v1.0.27
	github.com/openai/openai-go v1.12.0
	github.com/superfly/sprites-go v0.1.0
	github.com/yuin/goldmark v1.8.5
	golang.org/x/crypto v0.55.0
)

require (
	github.com/Masterminds/semver/v3 v3.2.1 // indirect
	github.com/aymerick/douceur v0.2.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/gorilla/css v1.0.1 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_golang v1.20.5 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/superfly/client-signals/go v0.4.4 // indirect
	github.com/tidwall/gjson v1.14.4 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)
