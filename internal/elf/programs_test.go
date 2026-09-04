package elf

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// Expected sizes and sha256 digests pin the exact ELF blobs shipped by the
// Rust litesvm 0.13.0 crate (src/programs/elf/). If a blob is re-vendored
// from a different litesvm version, these values must be re-measured.
var blobChecks = []struct {
	name   string
	size   int
	sha256 string
}{
	{
		name:   "spl-token (p-token)",
		size:   100312,
		sha256: "53e587eee80c786bcfba1450397d92179a0f01def776d19fb4ddf690d64431cc",
	},
	{
		name:   "spl-token-2022",
		size:   615936,
		sha256: "495e9d7680dd555cb126a6a8e5464af5be9b01f02f2cd70634352722d22e3cad",
	},
	{
		name:   "spl-memo v1",
		size:   17280,
		sha256: "67053ed6b0c4b2ff0cead4dc469b2dd30847264b28164ef5e5263d5cf1258c6e",
	},
	{
		name:   "spl-memo v3",
		size:   74800,
		sha256: "f520eaf096361abbb9639ea4dc3e5388a87b9330e121f476607b87c46ef67954",
	},
	{
		name:   "spl-associated-token-account",
		size:   105032,
		sha256: "6804554e69fd3a58caa191dc4a58f4c67223d30ca28ab8987f39fc18d2f7374d",
	},
	{
		name:   "address-lookup-table (core BPF)",
		size:   170144,
		sha256: "e264e1537c5ee1252aae1fa476c25000b641357bc6af4efab65f314160a99570",
	},
	{
		name:   "stake (core BPF)",
		size:   202280,
		sha256: "c9cba7f3d9fe0fac1f32e7a5e012284104a25d27ec97d6eb6d99a3afcd2352a8",
	},
}

func TestDefaults(t *testing.T) {
	if len(Defaults) != 7 {
		t.Fatalf("len(Defaults) = %d, want 7", len(Defaults))
	}
	for i, p := range Defaults {
		check := blobChecks[i]
		if p.Name != check.name {
			t.Errorf("Defaults[%d].Name = %q, want %q", i, p.Name, check.name)
		}
		if len(p.Elf) == 0 {
			t.Errorf("Defaults[%d] (%s): Elf is empty", i, p.Name)
			continue
		}
		if len(p.Elf) != check.size {
			t.Errorf("Defaults[%d] (%s): len(Elf) = %d, want %d", i, p.Name, len(p.Elf), check.size)
		}
		sum := sha256.Sum256(p.Elf)
		if got := hex.EncodeToString(sum[:]); got != check.sha256 {
			t.Errorf("Defaults[%d] (%s): sha256 = %s, want %s", i, p.Name, got, check.sha256)
		}
	}
}

func TestDefaultsLoaders(t *testing.T) {
	want := []Loader{
		LoaderV3, // spl-token (p-token)
		LoaderV3, // spl-token-2022
		LoaderV1, // spl-memo v1
		LoaderV2, // spl-memo v3
		LoaderV2, // spl-associated-token-account
		LoaderV3, // address-lookup-table
		LoaderV3, // stake
	}
	for i, p := range Defaults {
		if p.Loader != want[i] {
			t.Errorf("Defaults[%d] (%s): Loader = %d, want %d", i, p.Name, p.Loader, want[i])
		}
	}
}

func TestNativeMintData(t *testing.T) {
	data := NativeMintData()
	if len(data) != 82 {
		t.Fatalf("len(NativeMintData()) = %d, want 82", len(data))
	}
	if data[44] != 9 {
		t.Errorf("decimals byte at offset 44 = %d, want 9", data[44])
	}
	if data[45] != 1 {
		t.Errorf("is_initialized byte at offset 45 = %d, want 1", data[45])
	}
	for i, b := range data {
		if i == 44 || i == 45 {
			continue
		}
		if b != 0 {
			t.Errorf("byte at offset %d = %d, want 0", i, b)
		}
	}
}
