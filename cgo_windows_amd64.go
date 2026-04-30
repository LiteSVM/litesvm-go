//go:build windows && amd64 && !litesvm_dev && !dynamic

package litesvm

// #cgo CFLAGS: -I${SRCDIR}/litesvm_vendor -DUSE_VENDORED_LITESVM
// #cgo LDFLAGS: ${SRCDIR}/litesvm_vendor/liblitesvm_go_windows_amd64.a -lws2_32 -luserenv -lbcrypt -lntdll -ladvapi32 -lkernel32
import "C"

const litesvmLinkInfo = "static windows_amd64"
