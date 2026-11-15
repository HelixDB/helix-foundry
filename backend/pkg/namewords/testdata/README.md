word_corpus.json is the engine's crates/extraction/tests/data/word_corpus.json,
copied byte for byte, at the contract CONTRACT names (first written by
HelixDB/ontology#103, merged into ontology-v2-dev-3 as 7ecdb11fe). Each line is a
text with what the engine reads in it: `norm` (norm_name of the text), `words`
(the words text::WordIndex reads) and `written` (names, as norm_name gives
them, it writes there), and for texts picked by hand `unwritten` (names it
doesn't). The engine's test fails when the file isn't what it reads.

Every move to a new contract copies it again
(pb/proto/ontology_agent/v1/README.md): in the engine checkout of the
vendored commit, `UPDATE_WORD_CORPUS=1 cargo test -p extraction --test
word_corpus` (it writes nothing new unless the engine's words changed), copy
the file here, set CONTRACT to the new version and run
`go test ./pkg/namewords/`. TestCorpusIsThePinnedContracts fails while
CONTRACT is not `ontologyagent.ContractVersion`.

When the engine's Cargo.lock moves unicode-segmentation, also run
`node scripts/namewords-tables.mjs <its src/tables.rs> <version>`, which
writes incb.go and gcb_test.go from it.

fuzz/FuzzWords holds inputs FuzzWords once failed on, run as seeds.
