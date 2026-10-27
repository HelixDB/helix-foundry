package store

// FoundryDocumentSetSelect is what every read of sets selects, for the tests
// that explain its plan.
const FoundryDocumentSetSelect = "SELECT " + foundryDocumentSetColumns + foundryDocumentSets
