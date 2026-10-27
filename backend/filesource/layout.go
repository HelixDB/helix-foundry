package filesource

import (
	"cmp"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Listed is one object a listing returned.
type Listed struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	// Archived objects (S3 Glacier Flexible Retrieval and Deep Archive) cannot
	// be read until restored.
	Archived bool
}

// Dataset is one table: files of one format whose keys name the same table.
type Dataset struct {
	Name   string
	Format Format
	Files  []Listed
}

// Skipped is a key or a table that cannot be read, and why.
type Skipped struct{ Name, Reason string }

// MaxTableName is the longest table name, in bytes, a connector table stores.
const MaxTableName = 255

const (
	reasonUnsupported = "unsupported file type"
	reasonArchived    = "archived in Glacier; restore it first"
	reasonNameLong    = "name too long"
	reasonDelta       = "Delta Lake tables are not supported yet"
	reasonHudi        = "Hudi tables are not supported yet"
	reasonIceberg     = "Iceberg tables are not supported yet"
)

var (
	hiveSegment = regexp.MustCompile(`^[^=]+=.*$`)
	dateSegment = regexp.MustCompile(`^[0-9][0-9_.-]*$`)
	uuidRun     = regexp.MustCompile(`[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}`)
	digitRun    = regexp.MustCompile(`[0-9]+`)
	separators  = regexp.MustCompile(`[-_.\s]+`)
	// codecInfixes sit between a stem and its format, as in x.snappy.parquet.
	codecInfixes = []string{".snappy", ".zstd", ".gz", ".lz4", ".brotli"}
	// noiseTokens are words writers add to every file name of a table.
	noiseTokens = map[string]bool{
		"part": true, "c": true, "data": true, "chunk": true, "file": true,
		"run": true, "batch": true, "shard": true, "page": true,
	}
)

// Layout groups the objects under prefix into tables. It is a pure function
// of the keys:
//   - folder markers, empty objects, keys with a segment starting with "_"
//     or "." (other than a Hive k=v segment) and .crc and .tmp files are
//     ignored;
//   - a folder holding a Delta, Hudi or Iceberg table is skipped whole;
//   - an unsupported extension or an archived object is skipped by key;
//   - a table is named by its folder, without Hive and date-only segments,
//     and the file's name family (the stem without digits, UUIDs and words
//     like part or chunk); a folder holding one family is one table, and a
//     folder holding several gives each its own "folder/family" table;
//   - a table mixing formats, or whose name is over [MaxTableName] bytes, is
//     skipped by name.
//
// fallback names the table of nameless files at the prefix root; callers
// pass the prefix's last segment, or the bucket. Tables come back by name,
// with files by key; skipped entries by name.
func Layout(prefix, fallback string, objs []Listed) ([]Dataset, []Skipped) {
	lakes := lakeRoots(prefix, objs)
	type entry struct {
		obj    Listed
		dir    string
		family string
		format Format
		lake   int // 1 + index into lakes; 0 is outside every lake
	}
	var entries []entry
	var skipped []Skipped
	for _, o := range objs {
		rel, ok := strings.CutPrefix(o.Key, prefix)
		if !ok || rel == "" || strings.HasSuffix(rel, "/") || o.Size == 0 || hidden(rel) {
			continue
		}
		base := path.Base(rel)
		if hasSuffixFold(base, ".crc") || hasSuffixFold(base, ".tmp") {
			continue
		}
		stem, ext, ok := splitName(base)
		if !ok {
			skipped = append(skipped, Skipped{Name: rel, Reason: reasonUnsupported})
			continue
		}
		if o.Archived {
			skipped = append(skipped, Skipped{Name: rel, Reason: reasonArchived})
			continue
		}
		dir := tableDir(rel)
		entries = append(entries, entry{obj: o, dir: dir, family: family(stem), format: formatExts[ext], lake: findLake(lakes, rel)})
	}

	families := map[string]map[string]bool{}
	for _, e := range entries {
		if families[e.dir] == nil {
			families[e.dir] = map[string]bool{}
		}
		families[e.dir][e.family] = true
	}
	type group struct {
		files   []Listed
		formats map[Format]bool
		lake    int
	}
	groups := map[string]*group{}
	for _, e := range entries {
		name := tableName(e.dir, e.family, len(families[e.dir]) > 1, fallback)
		g := groups[name]
		if g == nil {
			g = &group{formats: map[Format]bool{}}
			groups[name] = g
		}
		g.files = append(g.files, e.obj)
		g.formats[e.format] = true
		if g.lake == 0 {
			g.lake = e.lake
		}
	}

	var datasets []Dataset
	covered := make([]bool, len(lakes))
	for name, g := range groups {
		switch {
		case g.lake != 0:
			covered[g.lake-1] = true
			skipped = append(skipped, Skipped{Name: name, Reason: lakes[g.lake-1].reason})
		case len(g.formats) > 1:
			names := make([]string, 0, len(g.formats))
			for f := range g.formats {
				names = append(names, f.String())
			}
			slices.Sort(names)
			skipped = append(skipped, Skipped{Name: name, Reason: "mixed formats (" + strings.Join(names, ", ") + ")"})
		case len(name) > MaxTableName:
			skipped = append(skipped, Skipped{Name: name, Reason: reasonNameLong})
		default:
			var format Format
			for f := range g.formats {
				format = f
			}
			slices.SortFunc(g.files, func(a, b Listed) int { return cmp.Compare(a.Key, b.Key) })
			datasets = append(datasets, Dataset{Name: name, Format: format, Files: g.files})
		}
	}
	// A lake whose files are all ignored (only its log, say) still says why
	// nothing under it was read.
	for i, root := range lakes {
		if !covered[i] {
			name := root.dir
			if name == "" {
				name = fallback
			}
			skipped = append(skipped, Skipped{Name: name, Reason: root.reason})
		}
	}
	slices.SortFunc(datasets, func(a, b Dataset) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(skipped, func(a, b Skipped) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Reason, b.Reason))
	})
	return datasets, skipped
}

// hidden reports a key with a segment that starts with "_" or "." and is
// not a Hive partition: _SUCCESS, .DS_Store, _delta_log/, _temporary/.
func hidden(rel string) bool {
	for _, s := range strings.Split(rel, "/") {
		if (strings.HasPrefix(s, "_") || strings.HasPrefix(s, ".")) && !hiveSegment.MatchString(s) {
			return true
		}
	}
	return false
}

// tableDir is a key's folder without Hive partitions and date-only folders,
// which split one table rather than name one.
func tableDir(rel string) string {
	segs := strings.Split(rel, "/")
	kept := segs[:0:0]
	for _, s := range segs[:len(segs)-1] {
		if s != "" && !hiveSegment.MatchString(s) && !dateSegment.MatchString(s) {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, "/")
}

// family is what a table's file names share: the stem in lower case without
// codec infixes, UUIDs, digits and writer noise words.
func family(stem string) string {
	for peeled := true; peeled; {
		peeled = false
		for _, c := range codecInfixes {
			if hasSuffixFold(stem, c) {
				stem, peeled = stem[:len(stem)-len(c)], true
			}
		}
	}
	s := digitRun.ReplaceAllString(uuidRun.ReplaceAllString(strings.ToLower(stem), ""), "")
	var tokens []string
	for _, t := range separators.Split(s, -1) {
		if t != "" && !noiseTokens[t] {
			tokens = append(tokens, t)
		}
	}
	return strings.Join(tokens, "_")
}

// tableName names a file's table from its folder and family. shared is a
// folder holding several families.
func tableName(dir, family string, shared bool, fallback string) string {
	switch {
	case dir == "" && family == "":
		return fallback
	case dir == "":
		return family
	case shared && family != "":
		return dir + "/" + family
	}
	return dir
}

// lakeRoot is a folder holding a table format Helix does not read.
type lakeRoot struct {
	dir    string
	reason string
}

// lakeRoots finds the Delta (_delta_log/), Hudi (.hoodie/) and Iceberg
// (metadata/*.metadata.json beside data/) folders under prefix.
func lakeRoots(prefix string, objs []Listed) []lakeRoot {
	found := map[string]string{}
	var icebergs []string
	dirs := map[string]bool{}
	for _, o := range objs {
		rel, ok := strings.CutPrefix(o.Key, prefix)
		if !ok {
			continue
		}
		segs := strings.Split(rel, "/")
		for i, s := range segs[:len(segs)-1] {
			dirs[strings.Join(segs[:i+1], "/")] = true
			switch {
			case s == "_delta_log":
				found[strings.Join(segs[:i], "/")] = reasonDelta
			case s == ".hoodie":
				found[strings.Join(segs[:i], "/")] = reasonHudi
			case s == "metadata" && i == len(segs)-2 && strings.HasSuffix(segs[i+1], ".metadata.json"):
				icebergs = append(icebergs, strings.Join(segs[:i], "/"))
			}
		}
	}
	for _, root := range icebergs {
		if dirs[path.Join(root, "data")] {
			found[root] = reasonIceberg
		}
	}
	roots := make([]lakeRoot, 0, len(found))
	for dir, reason := range found {
		roots = append(roots, lakeRoot{dir: dir, reason: reason})
	}
	slices.SortFunc(roots, func(a, b lakeRoot) int { return cmp.Compare(a.dir, b.dir) })
	return roots
}

// findLake is 1 + the index of the lake holding rel, or 0 outside every lake.
func findLake(lakes []lakeRoot, rel string) int {
	for i, r := range lakes {
		if r.dir == "" || strings.HasPrefix(rel, r.dir+"/") {
			return i + 1
		}
	}
	return 0
}

// Partition is one Hive k=v folder of a key; a nil Value is Hive's default
// (null) partition.
type Partition struct {
	Key   string
	Value *string
}

const hiveDefaultPartition = "__HIVE_DEFAULT_PARTITION__"

// Partitions are a key's Hive k=v folders in key order, URL-decoded as Hive
// encodes them.
func Partitions(relKey string) []Partition {
	segs := strings.Split(relKey, "/")
	var out []Partition
	for _, s := range segs[:len(segs)-1] {
		if !hiveSegment.MatchString(s) {
			continue
		}
		k, v, _ := strings.Cut(s, "=")
		p := Partition{Key: unescape(k)}
		if v != hiveDefaultPartition {
			value := unescape(v)
			p.Value = &value
		}
		out = append(out, p)
	}
	return out
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}
