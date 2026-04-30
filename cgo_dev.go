//go:build litesvm_dev

// Development mode: link against the local cargo build output instead of
// the vendored static archive. Use this when iterating on src/lib.rs:
//
//	cargo build              # produces target/debug/liblitesvm_go.{a,dylib,so}
//	go test -tags litesvm_dev ./...
//
// To link against a release build instead, set CGO_LDFLAGS="-L$(pwd)/target/release -llitesvm_go".

package litesvm

// #cgo CFLAGS: -I${SRCDIR}/include
// #cgo darwin  LDFLAGS: -L${SRCDIR}/target/debug -llitesvm_go -framework Security -framework CoreFoundation -liconv
// #cgo linux   LDFLAGS: -L${SRCDIR}/target/debug -llitesvm_go -lm -ldl -lpthread -lrt
// #cgo windows LDFLAGS: -L${SRCDIR}/target/debug -llitesvm_go -lws2_32 -luserenv -lbcrypt -lntdll -ladvapi32 -lkernel32
import "C"

const litesvmLinkInfo = "dynamic dev (target/debug)"
