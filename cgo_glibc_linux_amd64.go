//go:build linux && amd64 && !litesvm_dev && !dynamic && !musl

package litesvm

// #cgo CFLAGS: -I${SRCDIR}/litesvm_vendor -DUSE_VENDORED_LITESVM
// #cgo LDFLAGS: ${SRCDIR}/litesvm_vendor/liblitesvm_go_glibc_linux_amd64.a -lm -ldl -lpthread -lrt
import "C"

const litesvmLinkInfo = "static glibc_linux_amd64"
