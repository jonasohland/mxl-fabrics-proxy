// The repository was renamed to mxl-replicator; this module path deliberately was not. The
// path is the import prefix for every file under legacy/go, so changing it would rewrite an
// 38 import lines across 15 files of a tree that exists to be deleted at parity — and make
// git log and git blame useless on it during exactly the window where it is still the
// production implementation (M1a). Nothing fetches this module; the workspace resolves it by
// directory. Delete the tree at parity rather than renaming it.
module github.com/jonasohland/mxl-fabrics-proxy/legacy/go

go 1.26.0

require (
	github.com/alecthomas/kong v1.14.0
	github.com/dpotapov/slogpfx v0.0.0-20230917063348-41a73c95c536
	github.com/google/uuid v1.6.0
	github.com/kr/pretty v0.3.1
	github.com/lmittmann/tint v1.1.3
	github.com/samber/lo v1.53.0
	github.com/stretchr/testify v1.8.4
	golang.org/x/exp v0.0.0-20260312153236-7ab1446f8b90
	sigs.k8s.io/yaml v1.6.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/rogpeppe/go-internal v1.9.0 // indirect
	go.yaml.in/yaml/v2 v2.4.2 // indirect
	golang.org/x/text v0.22.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
