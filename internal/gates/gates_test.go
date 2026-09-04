package gates

import (
	"sort"
	"testing"

	"github.com/gagliardetto/solana-go"
)

// wantTotal is the number of unique feature-gate addresses registered in
// agave-feature-set 4.0.0's FEATURE_NAMES map, as measured by gen/gen.py.
// FEATURE_NAMES registers 274 module names, but two of them
// (create_slashing_program and enshrine_slashing_program) declare the same
// address, sProgVaNWkYdP2eTRAy1CPrgb3b9p8yXCASrPEqo6VJ, and are deduplicated
// into a single catalog entry: 274 registered names -> 273 unique addresses.
const wantTotal = 273

// wantMainnetActive is the number of entries gen/gen.py parsed from
// MAINNET_ACTIVE_FEATURES in litesvm-0.13.0 src/features.rs, the snapshot of
// gates active on mainnet-beta as of 2026-06-16. All 227 entries resolve to
// distinct addresses.
const wantMainnetActive = 227

func TestCatalogSize(t *testing.T) {
	if got := len(All); got != wantTotal {
		t.Fatalf("len(All) = %d, want %d", got, wantTotal)
	}
}

func TestNoDuplicateAddresses(t *testing.T) {
	seen := make(map[solana.PublicKey]string, len(All))
	for _, g := range All {
		if prev, ok := seen[g.Address]; ok {
			t.Errorf("address %s declared by both %q and %q", g.Address, prev, g.Name)
		}
		seen[g.Address] = g.Name
	}
}

func TestSortedByName(t *testing.T) {
	if !sort.SliceIsSorted(All, func(i, j int) bool { return All[i].Name < All[j].Name }) {
		t.Fatal("All is not sorted by Name")
	}
}

func TestMainnetActiveCount(t *testing.T) {
	got := 0
	for _, g := range All {
		if g.MainnetActive {
			got++
		}
	}
	if got != wantMainnetActive {
		t.Fatalf("mainnet-active count = %d, want %d", got, wantMainnetActive)
	}
}

// TestSpotChecks pins three well-known gates to the addresses declared in
// agave-feature-set 4.0.0 src/lib.rs.
func TestSpotChecks(t *testing.T) {
	cases := []struct {
		name          string
		address       string
		mainnetActive bool
	}{
		{"curve25519_syscall_enabled", "7rcw5UtqgDTBBv2EcynNfYckgdAaH1MAsCjKgXMkN7Ri", true},
		{"secp256k1_program_enabled", "E3PHP7w8kB7np3CTQ1qQ2tW3KCtjRSXBQgW9vM2mWv2Y", true},
		// alpenglow's production key (the dev-context-only-utils test key is
		// excluded by the generator); not yet active on mainnet.
		{"alpenglow", "mustRekeyVm2QHYB3JPefBiU4BY3Z6JkW2k3Scw5GWP", false},
	}
	byName := make(map[string]Gate, len(All))
	for _, g := range All {
		byName[g.Name] = g
	}
	for _, c := range cases {
		g, ok := byName[c.name]
		if !ok {
			t.Errorf("gate %q not found in All", c.name)
			continue
		}
		if want := solana.MustPublicKeyFromBase58(c.address); !g.Address.Equals(want) {
			t.Errorf("gate %q address = %s, want %s", c.name, g.Address, want)
		}
		if g.MainnetActive != c.mainnetActive {
			t.Errorf("gate %q MainnetActive = %v, want %v", c.name, g.MainnetActive, c.mainnetActive)
		}
	}
}
