//go:build linux && arm64 && musl && !litesvm_dev && !dynamic

package litesvm

// #cgo CFLAGS: -I${SRCDIR}/litesvm_vendor -DUSE_VENDORED_LITESVM
// #cgo LDFLAGS: ${SRCDIR}/litesvm_vendor/liblitesvm_go_musl_linux_arm64.a -lm -ldl -lpthread -lrt
import "C"

const litesvmLinkInfo = "static musl_linux_arm64"
