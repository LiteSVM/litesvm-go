#!/usr/bin/env python3
"""Generates ../gates.go, the Go feature-gate catalog.

Sources (pinned crates in the local cargo registry):
  1. agave-feature-set 4.0.0 src/lib.rs
       - `pub mod <name> { solana_pubkey::declare_id!("...") }` module tree
         (nested modules such as full_inflation::mainnet::certusone::vote are
         joined with "::")
       - the FEATURE_NAMES lazy map, which registers the canonical set of
         feature gates
  2. litesvm 0.13.0 src/features.rs
       - MAINNET_ACTIVE_FEATURES, a snapshot of feature gates active on
         mainnet-beta as of 2026-06-16

Rules applied:
  - Migration buffer addresses (module leaf named "buffer" or "*_buffer",
    e.g. upgrade_bpf_stake_program_to_v5::buffer and
    vote_state_v4::stake_program_buffer) are NOT feature gates and are
    excluded.
  - The alpenglow module declares two cfg-gated ids; the
    `#[cfg(feature = "dev-context-only-utils")]` test key is skipped and the
    production key is kept.
  - Only modules registered in FEATURE_NAMES become catalog entries.
  - Entries sharing one address (create_slashing_program /
    enshrine_slashing_program both declare
    sProgVaNWkYdP2eTRAy1CPrgb3b9p8yXCASrPEqo6VJ) are deduplicated to a single
    entry named after the alphabetically-first module, with the other module
    names noted in a comment.

Usage:
  python3 gen.py [path/to/agave-feature-set/src/lib.rs path/to/litesvm/src/features.rs]

Regenerate whenever the pinned agave-feature-set or litesvm crates change,
then update the hardcoded counts in ../gates_test.go to the values this
script prints.
"""

import os
import re
import sys
from collections import OrderedDict

# Default source locations inside the local cargo registry; override via
# argv (see main) when the crates live elsewhere.
CARGO_REGISTRY = os.path.expanduser(
    "~/.cargo/registry/src/index.crates.io-1949cf8c6b5b557f"
)
DEFAULT_AGAVE_LIB = os.path.join(
    CARGO_REGISTRY, "agave-feature-set-4.0.0", "src", "lib.rs"
)
DEFAULT_LITESVM_FEATURES = os.path.join(
    CARGO_REGISTRY, "litesvm-0.13.0", "src", "features.rs"
)

BASE58_RE = r'"([1-9A-HJ-NP-Za-km-z]{32,44})"'
DEV_ONLY_CFG = 'feature = "dev-context-only-utils"'


def sanitize_for_braces(line):
    """Strips string literals, char literals and comments so that only
    structural braces remain for depth tracking."""
    line = re.sub(r"/\*.*?\*/", "", line)  # single-line block comments
    out = []
    i = 0
    in_str = False
    while i < len(line):
        c = line[i]
        if in_str:
            if c == "\\":
                i += 2
                continue
            if c == '"':
                in_str = False
            i += 1
            continue
        if c == '"':
            in_str = True
            i += 1
            continue
        if c == "/" and line[i : i + 2] == "//":
            break
        out.append(c)
        i += 1
    return "".join(out)


def parse_modules(src):
    """Returns (module_path -> address, total declare_id count).

    Skips declare_id! occurrences guarded by
    #[cfg(feature = "dev-context-only-utils")].
    """
    depth = 0
    stack = []  # (name, body_depth)
    decls = OrderedDict()  # path -> [address, ...]
    total_declares = 0
    pending_cfg = None

    for raw in src.splitlines():
        stripped = raw.strip()
        clean = sanitize_for_braces(raw)

        mod_open = re.match(r"^(?:pub\s+)?mod\s+(\w+)\s*\{", stripped)
        if mod_open:
            stack.append((mod_open.group(1), depth + 1))

        decl = re.search(r"declare_id!\s*\(\s*" + BASE58_RE + r"\s*\)", raw)
        if decl:
            total_declares += 1
            path = "::".join(name for name, _ in stack)
            if not path:
                raise SystemExit("declare_id! outside any module: %s" % stripped)
            if pending_cfg == DEV_ONLY_CFG:
                pass  # dev-context-only test key (alpenglow): skip
            else:
                decls.setdefault(path, []).append(decl.group(1))

        cfg = re.match(r"^#\[cfg\((.*)\)\]$", stripped)
        if cfg:
            pending_cfg = cfg.group(1)
        elif stripped and not stripped.startswith("#["):
            pending_cfg = None

        depth += clean.count("{") - clean.count("}")
        while stack and depth < stack[-1][1]:
            stack.pop()

    if depth != 0:
        raise SystemExit("unbalanced braces while parsing lib.rs (depth=%d)" % depth)

    addr_of = {}
    for path, addrs in decls.items():
        if len(addrs) != 1:
            raise SystemExit(
                "module %s has %d declare_id! after cfg filtering: %s"
                % (path, len(addrs), addrs)
            )
        addr_of[path] = addrs[0]
    return addr_of, total_declares


def parse_feature_names(src):
    """Returns the ordered list of module paths registered in FEATURE_NAMES."""
    start = src.index("pub static FEATURE_NAMES")
    end = src.index("pub static ID", start)
    block = src[start:end]
    paths = re.findall(r"([A-Za-z_][A-Za-z0-9_:]*)::id\(\)", block)
    if len(paths) != len(set(paths)):
        dupes = sorted(p for p in set(paths) if paths.count(p) > 1)
        raise SystemExit("duplicate module paths in FEATURE_NAMES: %s" % dupes)
    return paths


def parse_mainnet_active(src):
    """Returns the list of module paths in litesvm's MAINNET_ACTIVE_FEATURES."""
    start = src.index("MAINNET_ACTIVE_FEATURES")
    end = src.index("];", start)
    block = src[start:end]
    paths = re.findall(r"agave_feature_set::([A-Za-z0-9_:]+)::ID\b", block)
    if len(paths) != len(set(paths)):
        dupes = sorted(p for p in set(paths) if paths.count(p) > 1)
        raise SystemExit("duplicate entries in MAINNET_ACTIVE_FEATURES: %s" % dupes)
    return paths


def is_buffer_module(path):
    leaf = path.rsplit("::", 1)[-1]
    return leaf == "buffer" or leaf.endswith("_buffer")


def main():
    if len(sys.argv) == 3:
        agave_lib, litesvm_features = sys.argv[1], sys.argv[2]
    elif len(sys.argv) == 1:
        agave_lib, litesvm_features = DEFAULT_AGAVE_LIB, DEFAULT_LITESVM_FEATURES
    else:
        raise SystemExit(__doc__)

    with open(agave_lib) as f:
        agave_src = f.read()
    with open(litesvm_features) as f:
        litesvm_src = f.read()

    addr_of, total_declares = parse_modules(agave_src)
    print("declare_id! occurrences in lib.rs: %d" % total_declares)

    buffers = sorted(p for p in addr_of if is_buffer_module(p))
    print("excluded buffer modules (%d): %s" % (len(buffers), ", ".join(buffers)))
    gate_addr_of = {p: a for p, a in addr_of.items() if not is_buffer_module(p)}

    registered = parse_feature_names(agave_src)
    print("FEATURE_NAMES registered module paths: %d" % len(registered))

    missing = sorted(p for p in registered if p not in gate_addr_of)
    if missing:
        raise SystemExit("registered in FEATURE_NAMES but not declared: %s" % missing)
    unregistered = sorted(p for p in gate_addr_of if p not in set(registered))
    if unregistered:
        print(
            "note: declared but not in FEATURE_NAMES (excluded): %s"
            % ", ".join(unregistered)
        )

    # Group registered modules by address; dedupe shared addresses.
    by_addr = OrderedDict()
    for path in sorted(registered):
        by_addr.setdefault(gate_addr_of[path], []).append(path)

    entries = []  # (name, address, shared_with)
    for addr, paths in by_addr.items():
        entries.append((paths[0], addr, paths[1:]))
    entries.sort(key=lambda e: e[0])

    for name, addr, shared in entries:
        if shared:
            print(
                "deduplicated address %s shared by modules: %s"
                % (addr, ", ".join([name] + shared))
            )
    print("catalog entries (unique addresses): %d" % len(entries))

    mainnet_paths = parse_mainnet_active(litesvm_src)
    mainnet_addrs = set()
    for path in mainnet_paths:
        if path not in gate_addr_of:
            raise SystemExit(
                "MAINNET_ACTIVE_FEATURES entry not a known gate: %s" % path
            )
        mainnet_addrs.add(gate_addr_of[path])
    if len(mainnet_addrs) != len(mainnet_paths):
        raise SystemExit("MAINNET_ACTIVE_FEATURES entries collide on an address")
    print(
        "mainnet-active features: %d entries, %d unique addresses"
        % (len(mainnet_paths), len(mainnet_addrs))
    )

    out_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "gates.go")
    lines = []
    lines.append(
        "// Code generated by gen/gen.py from agave-feature-set 4.0.0 and"
        " litesvm 0.13.0. DO NOT EDIT."
    )
    lines.append("")
    lines.append("// Package gates catalogs the Agave runtime feature gates: every feature")
    lines.append("// registered in agave-feature-set's FEATURE_NAMES map, with the pubkey that")
    lines.append("// activates it and whether it was active on mainnet-beta per the litesvm")
    lines.append("// 0.13 snapshot (2026-06-16).")
    lines.append("package gates")
    lines.append("")
    lines.append('import "github.com/gagliardetto/solana-go"')
    lines.append("")
    lines.append("// Gate is a single Agave runtime feature gate.")
    lines.append("type Gate struct {")
    lines.append("\tName          string // agave-feature-set module name")
    lines.append("\tAddress       solana.PublicKey")
    lines.append(
        "\tMainnetActive bool // active on mainnet per litesvm 0.13 snapshot (2026-06-16)"
    )
    lines.append("}")
    lines.append("")
    lines.append("// All lists every feature gate registered in FEATURE_NAMES, sorted by")
    lines.append("// module name. Addresses are unique: modules that share an address are")
    lines.append("// collapsed into one entry (see inline comments).")
    lines.append("var All = []Gate{")
    for name, addr, shared in entries:
        if shared:
            lines.append(
                "\t// Address also declared by module%s %s; deduplicated to this entry."
                % ("s" if len(shared) > 1 else "", ", ".join(shared))
            )
        # name is a Rust module path and addr is base58: both are plain ASCII
        # with no quotes or backslashes, so direct quoting is safe.
        lines.append(
            '\t{Name: "%s", Address: solana.MustPublicKeyFromBase58("%s"), MainnetActive: %s},'
            % (name, addr, "true" if addr in mainnet_addrs else "false")
        )
    lines.append("}")
    lines.append("")

    with open(out_path, "w") as f:
        f.write("\n".join(lines))
    print("wrote %s" % os.path.normpath(out_path))


if __name__ == "__main__":
    main()
