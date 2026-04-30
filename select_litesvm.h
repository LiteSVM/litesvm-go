/* select_litesvm.h - chooses between the vendored header and a system header.
 *
 * Per-platform build files (cgo_*.go) define USE_VENDORED_LITESVM via the
 * #cgo CFLAGS line; this redirects the include to the header shipped in
 * litesvm_vendor/. With -tags litesvm_dev or -tags dynamic the macro is
 * left undefined and we fall back to either the in-tree include/ or a
 * system-installed header. */

#ifndef SELECT_LITESVM_H
#define SELECT_LITESVM_H

#ifdef USE_VENDORED_LITESVM
#include "litesvm_vendor/litesvm.h"
#else
#include "litesvm.h"
#endif

#endif /* SELECT_LITESVM_H */
