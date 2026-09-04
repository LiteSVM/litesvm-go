package litesvm

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/Overclock-Validator/mithril/pkg/features"
	"github.com/gagliardetto/solana-go"

	"github.com/LiteSVM/litesvm-go/internal/gates"
)

// FeatureSet describes which runtime features are enabled: active feature
// ids with their activation slot, plus the set of known-but-inactive
// feature ids. It is plain Go state backed by the gates catalog — use
// NewFeatureSet or NewFeatureSetAllEnabled to obtain the feature catalog,
// mutate it with Activate/Deactivate, then install it with
// (*LiteSVM).SetFeatureSet.
type FeatureSet struct {
	active   map[solana.PublicKey]uint64
	inactive map[solana.PublicKey]struct{}
}

// NewFeatureSet returns the default feature set: every feature in the gates
// catalog, inactive.
func NewFeatureSet() (*FeatureSet, error) {
	fs := &FeatureSet{
		active:   make(map[solana.PublicKey]uint64),
		inactive: make(map[solana.PublicKey]struct{}, len(gates.All)),
	}
	for _, g := range gates.All {
		fs.inactive[g.Address] = struct{}{}
	}
	return fs, nil
}

// NewFeatureSetAllEnabled returns a FeatureSet with every known feature
// activated at slot 0.
func NewFeatureSetAllEnabled() (*FeatureSet, error) {
	fs := &FeatureSet{
		active:   make(map[solana.PublicKey]uint64, len(gates.All)),
		inactive: make(map[solana.PublicKey]struct{}),
	}
	for _, g := range gates.All {
		fs.active[g.Address] = 0
	}
	return fs, nil
}

// Close is a no-op: FeatureSet is plain Go state and holds nothing to
// release.
//
// Deprecated: there is nothing to release; calls can be removed.
func (fs *FeatureSet) Close() {}

// IsActive reports whether `featureID` is currently active.
func (fs *FeatureSet) IsActive(featureID solana.PublicKey) bool {
	if fs == nil {
		return false
	}
	_, ok := fs.active[featureID]
	return ok
}

// ActivatedSlot returns the slot at which `featureID` was activated.
// The bool is false if the feature is not active.
func (fs *FeatureSet) ActivatedSlot(featureID solana.PublicKey) (uint64, bool) {
	if fs == nil {
		return 0, false
	}
	slot, ok := fs.active[featureID]
	return slot, ok
}

// Activate marks `featureID` as active at `slot`. The error result is
// non-nil only for a nil receiver and is retained for backward
// compatibility.
func (fs *FeatureSet) Activate(featureID solana.PublicKey, slot uint64) error {
	if fs == nil {
		return fmt.Errorf("%w: Activate: nil feature set", ErrLiteSVM)
	}
	if fs.active == nil {
		fs.active = make(map[solana.PublicKey]uint64)
	}
	delete(fs.inactive, featureID)
	fs.active[featureID] = slot
	return nil
}

// Deactivate marks `featureID` as inactive. The error result is non-nil
// only for a nil receiver and is retained for backward compatibility.
func (fs *FeatureSet) Deactivate(featureID solana.PublicKey) error {
	if fs == nil {
		return fmt.Errorf("%w: Deactivate: nil feature set", ErrLiteSVM)
	}
	if fs.inactive == nil {
		fs.inactive = make(map[solana.PublicKey]struct{})
	}
	delete(fs.active, featureID)
	fs.inactive[featureID] = struct{}{}
	return nil
}

// ActiveCount returns the number of active features.
func (fs *FeatureSet) ActiveCount() int {
	if fs == nil {
		return 0
	}
	return len(fs.active)
}

// InactiveCount returns the number of inactive features.
func (fs *FeatureSet) InactiveCount() int {
	if fs == nil {
		return 0
	}
	return len(fs.inactive)
}

// ActiveFeatures returns the full list of active feature pubkeys. Output is
// sorted for deterministic ordering.
func (fs *FeatureSet) ActiveFeatures() []solana.PublicKey {
	if fs == nil || len(fs.active) == 0 {
		return nil
	}
	out := make([]solana.PublicKey, 0, len(fs.active))
	for id := range fs.active {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b solana.PublicKey) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// InactiveFeatures returns the full list of inactive feature pubkeys. Output
// is sorted for deterministic ordering.
func (fs *FeatureSet) InactiveFeatures() []solana.PublicKey {
	if fs == nil || len(fs.inactive) == 0 {
		return nil
	}
	out := make([]solana.PublicKey, 0, len(fs.inactive))
	for id := range fs.inactive {
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b solana.PublicKey) int { return bytes.Compare(a[:], b[:]) })
	return out
}

// SetFeatureSet installs `f` as the SVM's active feature set. The engine's
// runtime feature set is rebuilt from scratch, so feature ids missing from
// `f`'s active list become inactive.
//
// Mithril keys its Features map on {Name, Address} pairs with its own gate
// names, so activation is mapped by address onto mithril's gate catalog
// (features.AllFeatureGates). Gates mithril does not know are silently
// ignored — mithril has no execution path keyed on them, which matches the
// Rust engine's behavior of ignoring feature accounts it never consults.
//
// The on-chain feature-gate accounts written by New() are NOT rewritten:
// Rust's set_feature_set only swaps the runtime feature set (lib.rs:666-669)
// and leaves the accounts from with_feature_accounts untouched.
func (s *LiteSVM) SetFeatureSet(f *FeatureSet) error {
	if f == nil {
		return fmt.Errorf("%w: SetFeatureSet: nil feature set", ErrLiteSVM)
	}
	feats := features.NewFeaturesDefault()
	for _, gate := range features.AllFeatureGates {
		if slot, ok := f.active[solana.PublicKey(gate.Address)]; ok {
			feats.EnableFeature(gate, slot)
		}
	}
	s.feats = feats
	return nil
}
