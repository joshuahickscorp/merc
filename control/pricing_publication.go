package main

// Catalogue publication vs the advertised buyer surface.
//
// BuildCataloguePriceSchedule / ApplyRepricing can (and on the staging plane
// did) publish an r6 schedule while GET /pricing/board.json still answers 503.
// handlePriceBoardData → loadCurrentPublicCatalogue walks activation.advertised,
// not the models table. An empty advertised set is therefore a published
// catalogue that cannot be shown.
//
// CapabilityDigest deliberately excludes benchmark_authority, so resealing a
// cell from r4 to r6 keeps the same digest. syncActivationPolicy will not
// overwrite an existing document seed. The r4 seed is then applied and
// storedRoutableEntryHasCurrentGlobalAuthority refuses it (promotion_receipt
// no longer equals the document). The previous reader treated that as
// QUARANTINE, which emptied advertised and 503'd the board.
//
// A drifted document seed is the same class of staleness as a digest mismatch:
// drop it and fall back to the current document. Operator and non-document
// rollback rows still quarantine — that is the gate-v4 rule this must not
// loosen.

// documentSourcedActivationFallsBackToDocument reports whether a stored
// statement that is no longer exact current global authority should be dropped
// (document fallback) rather than written into the overlay as QUARANTINED.
func documentSourcedActivationFallsBackToDocument(entry ActivationPolicyEntry) bool {
	if entry.Source == activationSourceDocument {
		return true
	}
	return entry.Source == activationSourceRollback &&
		entry.RestoredSource == activationSourceDocument
}

// documentActivationSeedDrifted reports that the latest stored statement is
// still labelled as a document seed but no longer equals the document. Those
// rows must be rewritten forward on migrate; operator rows are left alone.
func documentActivationSeedDrifted(stored, want ActivationPolicyEntry) bool {
	if stored.Source != activationSourceDocument {
		return false
	}
	return stored.ProfileRevision != want.ProfileRevision ||
		stored.CapabilityDigest != want.CapabilityDigest ||
		stored.Lifecycle != want.Lifecycle ||
		stored.PromotionReceipt != want.PromotionReceipt
}
