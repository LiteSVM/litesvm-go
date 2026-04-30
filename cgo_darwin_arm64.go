//go:build darwin && arm64 && !litesvm_dev && !dynamic

package litesvm

// #cgo CFLAGS: -I${SRCDIR}/litesvm_vendor -DUSE_VENDORED_LITESVM
// #cgo LDFLAGS: ${SRCDIR}/litesvm_vendor/liblitesvm_go_darwin_arm64.a -framework Security -framework CoreFoundation -liconv
import "C"

const litesvmLinkInfo = "static darwin_arm64"
