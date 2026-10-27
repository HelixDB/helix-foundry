package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/helixdb/helix-foundry/backend/filesource"
)

// S3Settings are an s3 connector's stored settings. Exactly one of Role and
// Keys is set; the secret access key is the connector's secret.
type S3Settings struct {
	Bucket string `json:"bucket"`
	// Prefix is the folder read; empty reads the whole bucket. Prepared, it
	// ends with "/".
	Prefix string `json:"prefix"`
	// Region is the bucket's; a test fills it in when empty.
	Region string   `json:"region,omitempty"`
	Role   *S3Role  `json:"role,omitempty"`
	Keys   *S3Keys  `json:"keys,omitempty"`
	Tables []string `json:"tables,omitempty"`
}

// S3Role is access through the customer's role, which trusts Helix's.
type S3Role struct {
	ARN string `json:"arn"`
}

// S3Keys is access with an access key, the only way to reach an
// S3-compatible store.
type S3Keys struct {
	AccessKeyID string `json:"access_key_id"`
	// Endpoint is the HTTPS origin of an S3-compatible store; empty is AWS.
	Endpoint string `json:"endpoint,omitempty"`
}

// RoleSetup is a kind whose customers grant access by trusting Helix's role.
type RoleSetup interface {
	// Setup is the principal a workspace's role must trust and the external
	// ID it must require; ok is false when role access is off.
	Setup(workspaceID string) (principalARN, externalID string, ok bool)
}

// The s3 kind's limits that are not configuration.
const (
	maxS3Keys        = 10_000
	maxS3Tables      = 200
	maxUnavailable   = 50
	maxPrefixBytes   = 1024
	downloadsAtOnce  = 4
	objectReadTries  = 3
	helixOwnedDomain = "helix-db.com"
)

// The customer's messages; upstream text never goes in one.
const (
	msgBucket      = "enter the bucket name, like acme-exports"
	msgRegion      = "enter the bucket's region, like eu-west-2"
	msgRoleARN     = "enter the role ARN from your AWS account, like arn:aws:iam::123456789012:role/helix-foundry"
	msgAccess      = "choose IAM role or access key access"
	msgSecret      = "enter the secret access key"
	msgTemporary   = "temporary keys (ASIA…) expire; use an IAM user's key or a role"
	msgEndpoint    = "enter the HTTPS address of your S3-compatible storage, without a path"
	msgAWSEndpoint = "for AWS, leave the endpoint empty"
	msgPrivate     = "Helix can only reach public storage outside Helix; check the endpoint"
	msgNoRole      = "IAM role access is not available here; use an access key"
	msgKeyRejected = "S3 rejected the access key; check the key ID and secret"
	msgTooManyKeys = "this folder holds more than 10,000 files; choose a narrower folder"
	msgTooManyTbls = "this folder holds more than 200 tables; choose a narrower folder"
	msgThrottled   = "S3 is throttling requests; Helix will retry"
	msgUnreadable  = "Helix could not read the bucket; Helix will retry"
	msgBusy        = "a test of this connection is already running; try again in a moment"
	msgKeyID       = "enter the access key ID"
	msgSampleDeny  = "access denied (s3:GetObject, kms:Decrypt)"
)

var (
	bucketRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	regionRE    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d{1,2}$`)
	roleARNRE   = regexp.MustCompile(`^arn:aws:iam::(\d{12}):role/[\w+=,.@/-]{1,512}$`)
	noRegionFor = []string{"cn-", "us-gov-", "us-iso"}
)

func (k *s3Kind) Name() string        { return "s3" }
func (k *s3Kind) DisplayName() string { return "Amazon S3" }

func (k *s3Kind) Setup(workspaceID string) (string, string, bool) {
	return k.opts.RoleARN, S3ExternalID(workspaceID), k.opts.RoleARN != ""
}

// Hint names the access: a role, or the key's last four characters.
func (k *s3Kind) Hint(secret string) string {
	if secret == "" {
		return "IAM role"
	}
	if len(secret) < 8 {
		return "secret …"
	}
	return "secret …" + secret[len(secret)-4:]
}

// Prepare checks the settings without the network: the bucket, folder and
// region's shape, and exactly one of a role (no secret) and a key.
func (k *s3Kind) Prepare(src Source) (json.RawMessage, error) {
	var s S3Settings
	if err := json.Unmarshal(orEmpty(src.Settings), &s); err != nil {
		return nil, invalid("S3 settings are malformed")
	}
	if err := k.prepareSettings(&s, src.Secret); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

// prepareSettings checks and normalises a bucket's settings in place; the
// PostHog kind's export bucket goes through it too.
func (k *s3Kind) prepareSettings(s *S3Settings, secret string) error {
	s.Bucket = strings.TrimSpace(s.Bucket)
	if !bucketRE.MatchString(s.Bucket) || strings.Contains(s.Bucket, "..") || isIPv4(s.Bucket) {
		return invalid(msgBucket)
	}
	if slices.Contains(k.opts.Reserved, s.Bucket) {
		return invalid("%s is a Helix bucket", s.Bucket)
	}
	s.Prefix = strings.TrimLeft(strings.TrimSpace(s.Prefix), "/")
	if s.Prefix != "" && !strings.HasSuffix(s.Prefix, "/") {
		s.Prefix += "/"
	}
	if len(s.Prefix) > maxPrefixBytes {
		return invalid("the folder must be at most %d bytes", maxPrefixBytes)
	}
	s.Region = strings.TrimSpace(s.Region)
	if s.Region != "" && (!regionRE.MatchString(s.Region) || slices.ContainsFunc(noRegionFor, func(p string) bool { return strings.HasPrefix(s.Region, p) })) {
		return invalid(msgRegion)
	}
	switch {
	case (s.Role == nil) == (s.Keys == nil):
		return invalid(msgAccess)
	case s.Role != nil:
		if err := k.prepareRole(s.Role, secret); err != nil {
			return err
		}
	default:
		if err := prepareKeys(s.Keys, secret); err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	tables := s.Tables[:0]
	for _, t := range s.Tables {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			seen[t] = true
			tables = append(tables, t)
		}
	}
	s.Tables = tables
	return nil
}

func (k *s3Kind) prepareRole(r *S3Role, secret string) error {
	r.ARN = strings.TrimSpace(r.ARN)
	if k.opts.RoleARN == "" {
		return invalid(msgNoRole)
	}
	m := roleARNRE.FindStringSubmatch(r.ARN)
	// A role in Helix's own account could read Helix's buckets.
	if m == nil || m[1] == accountOf(k.opts.RoleARN) {
		return invalid(msgRoleARN)
	}
	if secret != "" {
		return invalid(msgAccess)
	}
	return nil
}

func prepareKeys(keys *S3Keys, secret string) error {
	keys.AccessKeyID = strings.TrimSpace(keys.AccessKeyID)
	if n := len(keys.AccessKeyID); n < 3 || n > 128 || strings.ContainsAny(keys.AccessKeyID, " \t\r\n") {
		return invalid(msgKeyID)
	}
	if secret == "" {
		return invalid(msgSecret)
	}
	endpoint := strings.TrimSpace(keys.Endpoint)
	if endpoint == "" {
		keys.Endpoint = ""
		if strings.HasPrefix(keys.AccessKeyID, "ASIA") {
			return invalid(msgTemporary)
		}
		return nil
	}
	u, err := url.Parse(endpoint)
	// A trailing dot names the same host but would slip past the suffix
	// checks below, so the endpoint is refused rather than rewritten.
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || strings.HasSuffix(u.Hostname(), ".") {
		return invalid(msgEndpoint)
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case host == "amazonaws.com" || strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn"):
		return invalid(msgAWSEndpoint)
	case host == helixOwnedDomain || strings.HasSuffix(host, "."+helixOwnedDomain):
		return invalid(msgPrivate)
	}
	keys.Endpoint = "https://" + u.Host
	return nil
}

func isIPv4(s string) bool {
	addr, err := netip.ParseAddr(s)
	return err == nil && addr.Is4()
}

// accountOf is the account of an IAM ARN, or "" for anything else.
func accountOf(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[4]
}

// Discover lists the folder, groups its files into tables and, for a test,
// samples them for columns and a preview. A sync's discovery only lists.
func (k *s3Kind) Discover(ctx context.Context, src Source) (Discovery, error) {
	s, err := parseS3Settings(src.Settings)
	if err != nil {
		return Discovery{}, err
	}
	release, ok := k.claimDiscover(src.WorkspaceID)
	if !ok {
		return Discovery{}, refused(nil, msgBusy)
	}
	defer release()
	deadline := k.timeouts.test
	if src.ForSync {
		deadline = k.timeouts.sync
	}
	hardCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	sess, err := k.open(hardCtx, s, src, true)
	if err != nil {
		return Discovery{}, err
	}
	objs, err := sess.list(hardCtx)
	if err != nil {
		return Discovery{}, err
	}
	fallback := s.Bucket
	if s.Prefix != "" {
		fallback = path.Base(strings.TrimSuffix(s.Prefix, "/"))
	}
	datasets, skipped := filesource.Layout(s.Prefix, fallback, objs)
	switch {
	case len(datasets) > maxS3Tables:
		return Discovery{}, invalid(msgTooManyTbls)
	case len(datasets) == 0:
		return Discovery{}, invalid("no files Helix can read under s3://%s/%s; Helix reads Parquet, JSON, JSON Lines and CSV", s.Bucket, s.Prefix)
	}
	d := Discovery{MaxBytes: k.opts.MaxConnectorBytes}
	// byAge is each table's files oldest first, the order a test samples in.
	var byAge [][]Object
	for _, ds := range datasets {
		t := Table{Name: ds.Name}
		var size int64
		for _, f := range ds.Files {
			t.Files = append(t.Files, Object{Key: f.Key, Size: f.Size, ETag: f.ETag, Format: ds.Format})
			size += f.Size
		}
		if size > k.opts.MaxSourceBytes {
			d.Unavailable = append(d.Unavailable, Unavailable{Name: ds.Name,
				Reason: fmt.Sprintf("is %s, over the %s limit", FormatBytes(size), FormatBytes(k.opts.MaxSourceBytes))})
			continue
		}
		listed := slices.Clone(ds.Files)
		slices.SortStableFunc(listed, func(a, b filesource.Listed) int { return a.LastModified.Compare(b.LastModified) })
		aged := make([]Object, len(listed))
		for i, f := range listed {
			aged[i] = Object{Key: f.Key, Size: f.Size, ETag: f.ETag, Format: ds.Format}
		}
		d.Tables = append(d.Tables, t)
		byAge = append(byAge, aged)
	}
	for _, sk := range skipped {
		d.Unavailable = append(d.Unavailable, Unavailable{Name: sk.Name, Reason: sk.Reason})
	}
	for _, name := range s.Tables {
		if !slices.ContainsFunc(d.Tables, func(t Table) bool { return t.Name == name }) &&
			!slices.ContainsFunc(d.Unavailable, func(u Unavailable) bool { return u.Name == name }) {
			return Discovery{}, invalid("table %q is not in this folder any more", name)
		}
	}
	if sess.region != s.Region {
		s.Region = sess.region
		if d.Settings, err = json.Marshal(s); err != nil {
			return Discovery{}, err
		}
	}
	if src.ForSync {
		// A sync must see every unreadable table it was asked for, so the
		// list is capped only for a test's response.
		var stored int64
		for _, t := range d.Tables {
			if len(s.Tables) == 0 || slices.Contains(s.Tables, t.Name) {
				for _, f := range t.Files {
					stored += f.Size
				}
			}
		}
		if stored > k.opts.MaxConnectorBytes {
			return Discovery{}, invalid("the tables chosen hold %s, over the %s limit for one connection; choose fewer tables or a narrower folder",
				FormatBytes(stored), FormatBytes(k.opts.MaxConnectorBytes))
		}
		return d, nil
	}
	if n := len(d.Unavailable) - maxUnavailable; n > 0 {
		d.Unavailable = append(d.Unavailable[:maxUnavailable], Unavailable{Name: fmt.Sprintf("…and %d more", n)})
	}
	k.sampleTables(ctx, sess, d.Tables, byAge)
	return d, nil
}

func parseS3Settings(raw json.RawMessage) (S3Settings, error) {
	var s S3Settings
	if err := json.Unmarshal(raw, &s); err != nil || (s.Role == nil) == (s.Keys == nil) {
		return s, invalid("S3 settings are malformed")
	}
	return s, nil
}

// claimDiscover lets one discovery of a workspace run at a time in this
// process: a test samples under a memory budget shared by every customer.
func (k *s3Kind) claimDiscover(workspaceID string) (release func(), ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.running[workspaceID] {
		return nil, false
	}
	k.running[workspaceID] = true
	return func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		delete(k.running, workspaceID)
	}, true
}

// Sync downloads the table's files, reads them once for their exact columns
// and key, then again to write every row with every column.
func (k *s3Kind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	s, err := parseS3Settings(src.Settings)
	if err != nil {
		return Table{}, err
	}
	// Discover proved the role's trust for this sync; a table needs no probe.
	sess, err := k.open(ctx, s, src, false)
	if err != nil {
		return Table{}, err
	}
	dir, err := os.MkdirTemp(k.opts.TempDir, "foundry-s3-*")
	if err != nil {
		return Table{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }() // best-effort: the worker sweeps leftovers when it starts
	files, err := k.download(ctx, sess, table, dir)
	if err != nil {
		return Table{}, err
	}
	lim := filesource.DefaultLimits
	lim.MaxDecompressedBytes = 2 * k.opts.MaxTableBytes
	schema, err := filesource.Scan(ctx, files, lim)
	if err != nil {
		return Table{}, readFailure(err, files)
	}
	for row, err := range schema.Rows(ctx) {
		if err != nil {
			return Table{}, readFailure(err, files)
		}
		if err := w.WriteJSON(row); err != nil {
			return Table{}, err
		}
	}
	countRead(schema, files)
	written := Table{Name: table.Name, PrimaryKey: schema.PrimaryKey()}
	for _, c := range schema.Columns() {
		written.Columns = append(written.Columns, Column{Name: c.Name, Type: c.Type.String(), Nullable: c.Nullable})
	}
	return written, nil
}

// countRead records files whose rows were all read.
func countRead(schema *filesource.Schema, files []filesource.File) {
	for reason, n := range schema.Nulled() {
		s3ValuesNulled.WithLabelValues(reason).Add(float64(n))
	}
	for _, f := range files {
		s3Files.WithLabelValues(f.Format.String(), "ok").Inc()
	}
}

// readFailure gives unreadable data the customer's message, naming the file
// and record; the sync cannot fix it by retrying.
func readFailure(err error, files []filesource.File) error {
	var e *filesource.Error
	if !errors.As(err, &e) {
		return err
	}
	format := "unknown"
	if i := slices.IndexFunc(files, func(f filesource.File) bool { return f.Name == e.File }); i >= 0 {
		format = files[i].Format.String()
	}
	s3Files.WithLabelValues(format, "malformed").Inc()
	return invalid("%s", e.Error())
}

// download copies the table's files, four at a time, to dir.
func (k *s3Kind) download(ctx context.Context, sess *s3Session, table Table, dir string) ([]filesource.File, error) {
	files := make([]filesource.File, len(table.Files))
	var total struct {
		sync.Mutex
		bytes int64
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(downloadsAtOnce)
	for i, o := range table.Files {
		files[i] = filesource.File{
			Path: filepath.Join(dir, fmt.Sprintf("%05d", i)), Name: o.Key, Format: o.Format,
			Partitions: filesource.Partitions(strings.TrimPrefix(o.Key, sess.settings.Prefix)),
		}
		g.Go(func() error {
			n, err := sess.fetch(gctx, o, files[i].Path, func(n int64) error {
				total.Lock()
				defer total.Unlock()
				total.bytes += n
				if total.bytes > k.opts.MaxSourceBytes {
					return errGrew
				}
				return nil
			})
			if err != nil {
				return err
			}
			s3Bytes.WithLabelValues(o.Format.String()).Add(float64(n))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return files, nil
}
