word_corpus.json is the engine's crates/extraction/tests/data/word_corpus.json,
copied byte for byte, at engine commit dccf14d48 (branch
word-corpus-for-foundry on ontology-v2-dev-3, contract 4.23.0). Each line is a
text with what the engine reads in it: `norm` (norm_name of the text), `words`
(the words text::WordIndex reads) and `written` (names, as norm_name gives
them, it writes there), and for texts picked by hand `unwritten` (names it
doesn't). The engine's test fails when the file isn't what it reads.

To refresh it, after the engine's words or normalization change: in the
engine, `UPDATE_WORD_CORPUS=1 cargo test -p extraction --test word_corpus`,
then copy the file here, update the commit above and run
`go test ./pkg/namewords/`.

fuzz/FuzzWords holds inputs FuzzWords once failed on, run as seeds.
