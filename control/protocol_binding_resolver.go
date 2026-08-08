package main

import (
	"errors"
	"fmt"

	"merc/control/protocol"
)

// ProtocolEvidenceBindingSnapshot is the one-way handoff from Merc's existing
// append-only evidence validation boundary to protocol V1. The caller must
// construct it from a validated binding index; this adapter never reads or
// mutates evidence files and cannot make an UNBOUND artifact current.
type ProtocolEvidenceBindingSnapshot struct {
	Artifact protocol.EvidenceBindingArtifact
	Current  bool
}

// ProtocolEvidenceBindingResolver is an immutable in-memory view used by
// monolith shadow/review decisions. It is deliberately not a service, cache,
// or registry writer; persistence remains the existing evidence boundary.
type ProtocolEvidenceBindingResolver struct {
	byDigest map[string]protocol.EvidenceBindingResolution
}

func NewProtocolEvidenceBindingResolver(
	snapshots []ProtocolEvidenceBindingSnapshot,
) (*ProtocolEvidenceBindingResolver, error) {
	if len(snapshots) == 0 {
		return nil, errors.New("protocol evidence binding resolver requires snapshots")
	}
	resolver := &ProtocolEvidenceBindingResolver{
		byDigest: make(map[string]protocol.EvidenceBindingResolution, len(snapshots)),
	}
	for _, snapshot := range snapshots {
		if err := snapshot.Artifact.Validate(); err != nil {
			return nil, fmt.Errorf("invalid protocol binding snapshot: %w", err)
		}
		ref := protocol.Ref(snapshot.Artifact.Header)
		if _, exists := resolver.byDigest[ref.CanonicalSHA256]; exists {
			return nil, fmt.Errorf("duplicate protocol binding artifact %q", ref.ID)
		}
		resolver.byDigest[ref.CanonicalSHA256] = protocol.EvidenceBindingResolution{
			Artifact: snapshot.Artifact,
			Current:  snapshot.Current,
		}
	}
	return resolver, nil
}

func (r *ProtocolEvidenceBindingResolver) ResolveEvidenceBinding(
	ref protocol.ContractRef,
) (protocol.EvidenceBindingResolution, error) {
	if r == nil {
		return protocol.EvidenceBindingResolution{}, errors.New("protocol evidence binding resolver is nil")
	}
	if err := ref.Validate(protocol.KindEvidenceBinding); err != nil {
		return protocol.EvidenceBindingResolution{}, err
	}
	value, ok := r.byDigest[ref.CanonicalSHA256]
	if !ok {
		return protocol.EvidenceBindingResolution{}, fmt.Errorf("binding artifact %q is absent from immutable resolver view", ref.ID)
	}
	return value, nil
}
