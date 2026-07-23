module github.com/augmem/cortext-cpa-plugin/plugin

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v7 v7.2.96
	github.com/tidwall/gjson v1.18.0
	github.com/tidwall/sjson v1.2.5
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.0 // indirect
)

// Optional native engine dependency (build with -tags cortext_native).
// Kept as a require so go mod tidy can resolve via the local replace below.
require github.com/gabrielwillen/cortext/bindings/go v0.0.0

// Local development: sibling checkouts.
replace github.com/router-for-me/CLIProxyAPI/v7 => ../../cortext-proxy

replace github.com/gabrielwillen/cortext/bindings/go => ../../cortext/bindings/go
