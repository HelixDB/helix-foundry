package events

import "fmt"

// BatchKey is where a send's batch object lives in the Foundry data bucket,
// named by its batch's id. Each send takes a new id before it writes, so an
// object of a send that never committed can never replace a committed one.
// Batch ids are ULIDs and need no escaping.
func BatchKey(workspaceID, connectorID, batchID string) string {
	return BatchPrefix(workspaceID, connectorID) + batchID + ".jsonl.gz"
}

// BatchPrefix is the folder of every batch object of a source, the orphans
// of sends that never committed included: the purge of a deleted source
// empties it.
func BatchPrefix(workspaceID, connectorID string) string {
	return fmt.Sprintf("ws/%s/events/%s/", workspaceID, connectorID)
}
