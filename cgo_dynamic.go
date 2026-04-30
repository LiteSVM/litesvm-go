//go:build dynamic && !litesvm_dev

// Dynamic / system mode: link against a system-installed liblitesvm_go.
// Override CGO_CFLAGS / CGO_LDFLAGS as needed to point at where the
// shared library and header live.
//
//	go build -tags dynamic ./...

package litesvm

// #cgo LDFLAGS: -llitesvm_go
import "C"

const litesvmLinkInfo = "dynamic (system)"
