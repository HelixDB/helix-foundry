package store

// FoundryDocumentSetSelect is what every read of sets selects, for the tests
// that explain its plan.
const FoundryDocumentSetSelect = "SELECT " + foundryDocumentSetColumns + foundryDocumentSets

// SetAfterHealPublishBatch runs fn after each batch a heal publish
// commits, until the test ends.
func SetAfterHealPublishBatch(t interface{ Cleanup(func()) }, fn func()) {
	afterHealPublishBatch = fn
	t.Cleanup(func() { afterHealPublishBatch = func() {} })
}
