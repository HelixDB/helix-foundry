package connector

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/filesource"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
)

// S3Options configure the s3 kind.
type S3Options struct {
	// Base is the process's own AWS identity. It is used only to assume
	// RoleARN, never to read a customer's bucket.
	Base aws.Config
	// RoleARN is the foundry-connector role customers' roles trust; empty
	// turns role access off.
	RoleARN string
	// MaxSourceBytes caps one table's stored bytes, MaxConnectorBytes those
	// of the tables one sync reads, and MaxTableBytes a table's snapshot,
	// which bounds decompression at twice it.
	MaxSourceBytes, MaxConnectorBytes, MaxTableBytes int64
	// Reserved are Helix's own buckets, refused as sources.
	Reserved []string
	// TempDir holds a sync's downloads; empty is the system temp dir.
	TempDir string
}

// NewS3Options reads the s3 kind's options from config; base is the
// process's AWS identity.
func NewS3Options(cfg *config.Config, base aws.Config) S3Options {
	var reserved []string
	if cfg.Foundry.Sync.DataBucket != "" {
		reserved = append(reserved, cfg.Foundry.Sync.DataBucket)
	}
	return S3Options{
		Base: base, RoleARN: cfg.Foundry.Connectors.S3RoleARN, Reserved: reserved,
		MaxSourceBytes: cfg.Foundry.Sync.MaxSourceBytes, MaxConnectorBytes: cfg.Foundry.Sync.MaxConnectorBytes,
		MaxTableBytes: cfg.Foundry.Sync.MaxTableBytes,
	}
}

// S3ExternalID is the external ID a workspace's role must require. It is
// derived from the workspace, never from settings, so it cannot be borrowed.
func S3ExternalID(workspaceID string) string { return "helix-foundry-" + workspaceID }

// s3Timeouts bound every wait on a bucket; tests shrink them.
type s3Timeouts struct {
	// header is how long a response's headers may take.
	header time.Duration
	// idle is how long a body may send nothing; a download also gets
	// object plus perMiB for each MiB.
	idle, object, perMiB time.Duration
	// expiry is how early a chained role session is renewed.
	expiry time.Duration
	// test and sync bound credentials, region and listing; sample is a
	// test's soft bound on sampling.
	test, sync, sample time.Duration
	// retryMax is the longest wait between retries.
	retryMax time.Duration
}

var defaultS3Timeouts = s3Timeouts{
	header: 30 * time.Second, idle: 60 * time.Second, object: 5 * time.Minute, perMiB: time.Second,
	expiry: 5 * time.Minute, test: 25 * time.Second, sync: 60 * time.Second, sample: 15 * time.Second,
	retryMax: 20 * time.Second,
}

// Sampling bounds: the backend serving tests is one small pod.
const (
	maxSampleGets   = 100
	fetchesAtOnce   = 8
	decodesAtOnce   = 4
	filesPerTable   = 5
	sampleRangeSize = 1 << 20
	maxPreviewBytes = 256 << 10
	maxColumnBytes  = 1 << 20
)

type s3Kind struct {
	opts      S3Options
	guard     Guard
	endpoints Endpoints
	timeouts  s3Timeouts
	// hop1 is the foundry-connector role's session; nil without RoleARN.
	hop1 aws.CredentialsProvider
	// tlsConfig trusts an S3-compatible store's certificate in tests; nil
	// trusts the system's roots.
	tlsConfig *tls.Config
	// decodes caps the samples decoded at once in the process.
	decodes chan struct{}
	mu      sync.Mutex
	running map[string]bool
}

func newS3Kind(endpoints Endpoints, guard Guard, opts S3Options) *s3Kind {
	k := &s3Kind{
		opts: opts, guard: guard, endpoints: endpoints, timeouts: defaultS3Timeouts,
		decodes: make(chan struct{}, decodesAtOnce), running: map[string]bool{},
	}
	if opts.RoleARN != "" {
		k.hop1 = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(k.sts(opts.Base.Credentials), opts.RoleARN,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "foundry-connector" }))
	}
	return k
}

// retryer is built per client: the SDK's shared token bucket would end a
// sync after about a hundred retries and let one bucket's errors starve others.
func (k *s3Kind) retryer() aws.Retryer {
	return retry.NewStandard(func(o *retry.StandardOptions) {
		o.RateLimiter = ratelimit.None
		o.MaxAttempts = 6
		o.MaxBackoff = k.timeouts.retryMax
	})
}

func (k *s3Kind) awsHTTP() aws.HTTPClient {
	return awshttp.NewBuildableClient().WithTransportOptions(func(t *http.Transport) {
		t.ResponseHeaderTimeout = k.timeouts.header
	})
}

func (k *s3Kind) sts(creds aws.CredentialsProvider) *sts.Client {
	region := k.opts.Base.Region
	if region == "" {
		region = "us-east-1"
	}
	o := sts.Options{Region: region, Credentials: creds, HTTPClient: k.awsHTTP(), Retryer: k.retryer()}
	if k.endpoints.STS != "" {
		o.BaseEndpoint = aws.String(k.endpoints.STS)
	}
	return sts.New(o)
}

// s3Session is one discovery's or sync's access to a bucket.
type s3Session struct {
	k        *s3Kind
	settings S3Settings
	creds    aws.CredentialsProvider
	region   string
	client   *s3.Client
}

// open builds the session's credentials, finds the bucket's region and
// makes its client. Its errors carry the customer's message.
func (k *s3Kind) open(ctx context.Context, s S3Settings, src Source, probe bool) (*s3Session, error) {
	sess := &s3Session{k: k, settings: s, region: s.Region}
	switch {
	case s.Role != nil:
		creds, err := k.roleCredentials(ctx, s.Role.ARN, src.WorkspaceID, probe)
		if err != nil {
			return nil, err
		}
		sess.creds = creds
	default:
		sess.creds = credentials.NewStaticCredentialsProvider(s.Keys.AccessKeyID, src.Secret, "")
	}
	// A custom endpoint without a region signs for us-east-1, which every
	// S3-compatible store accepts; AWS says where a bucket lives.
	if sess.region == "" && (s.Keys == nil || s.Keys.Endpoint == "") {
		region, err := sess.bucketRegion(ctx)
		if err != nil {
			return nil, err
		}
		sess.region = region
	}
	sess.client = sess.newClient(sess.region)
	return sess, nil
}

// roleCredentials chains the foundry-connector role into the customer's,
// after proving the customer's trust requires this workspace's external ID:
// a role that accepts a random one would be readable from any workspace
// that learned its ARN.
func (k *s3Kind) roleCredentials(ctx context.Context, arn, workspaceID string, probe bool) (aws.CredentialsProvider, error) {
	if k.hop1 == nil {
		return nil, invalid(msgNoRole)
	}
	// Helix's own hop comes first: its AccessDenied would otherwise read as
	// the customer's trust refusing Helix.
	if _, err := k.hop1.Retrieve(ctx); err != nil {
		s3AssumeRole.WithLabelValues("error").Inc()
		return nil, refused(err, msgUnreadable)
	}
	client := k.sts(k.hop1)
	externalID := S3ExternalID(workspaceID)
	if probe {
		// The probe's ID is a new workspace's, so a trust that admits Helix
		// workspaces by pattern (helix-foundry-*) admits it too.
		_, err := client.AssumeRole(ctx, &sts.AssumeRoleInput{
			RoleArn: aws.String(arn), RoleSessionName: aws.String("foundry-probe-" + workspaceID),
			ExternalId: aws.String(S3ExternalID(id.New())),
		})
		switch {
		case err == nil:
			s3AssumeRole.WithLabelValues("unenforced").Inc()
			return nil, invalid("your role's trust policy must require external ID %s; Helix won't use a role that doesn't", externalID)
		case apiCode(err) != "AccessDenied":
			s3AssumeRole.WithLabelValues("error").Inc()
			return nil, k.stsFailure(err, externalID)
		}
	}
	creds := aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(client, arn, func(o *stscreds.AssumeRoleOptions) {
		o.ExternalID = aws.String(externalID)
		o.RoleSessionName = "foundry-" + workspaceID
		// Chained sessions last an hour at most; the cache renews them.
		o.Duration = time.Hour
	}), func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = k.timeouts.expiry })
	if _, err := creds.Retrieve(ctx); err != nil {
		if apiCode(err) == "AccessDenied" {
			s3AssumeRole.WithLabelValues("denied").Inc()
		} else {
			s3AssumeRole.WithLabelValues("error").Inc()
		}
		return nil, k.stsFailure(err, externalID)
	}
	s3AssumeRole.WithLabelValues("ok").Inc()
	return creds, nil
}

func (k *s3Kind) stsFailure(err error, externalID string) error {
	switch apiCode(err) {
	case "AccessDenied":
		return refused(err, "Helix could not assume the role; check its trust policy names %s and requires external ID %s", k.opts.RoleARN, externalID)
	case "ValidationError":
		return invalid(msgRoleARN)
	}
	return refused(err, msgUnreadable)
}

// newClient signs only with the session's credentials, never the
// process's, and talks to AWS, or a custom endpoint through the guard.
func (sess *s3Session) newClient(region string) *s3.Client {
	k := sess.k
	if region == "" {
		region = "us-east-1"
	}
	o := s3.Options{
		Region: region, Credentials: sess.creds, Retryer: k.retryer(), HTTPClient: k.awsHTTP(),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		UsePathStyle:               strings.Contains(sess.settings.Bucket, "."),
	}
	switch {
	case sess.settings.Keys != nil && sess.settings.Keys.Endpoint != "":
		transport := k.guard.Transport()
		transport.ResponseHeaderTimeout = k.timeouts.header
		transport.TLSClientConfig = k.tlsConfig
		o.BaseEndpoint, o.UsePathStyle = aws.String(sess.settings.Keys.Endpoint), true
		o.HTTPClient = &http.Client{Transport: transport, CheckRedirect: noRedirects}
	case k.endpoints.S3 != "":
		o.BaseEndpoint, o.UsePathStyle = aws.String(k.endpoints.S3), true
	}
	return s3.New(o)
}

// bucketRegion asks us-east-1, which answers for every bucket: the region
// is on the success and in a redirect's or refusal's headers.
func (sess *s3Session) bucketRegion(ctx context.Context) (string, error) {
	out, err := sess.newClient("us-east-1").HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(sess.settings.Bucket)})
	if err == nil && out.BucketRegion != nil && *out.BucketRegion != "" {
		return *out.BucketRegion, nil
	}
	if region := regionHeader(err); region != "" {
		return region, nil
	}
	if code := apiCode(err); code == "NotFound" || code == "NoSuchBucket" {
		return "", invalid("bucket %s does not exist", sess.settings.Bucket)
	}
	if err != nil && !isAccessDenied(err) {
		return "", s3Failure(err, opList, sess.settings, "")
	}
	return "", invalid(msgRegion)
}

func regionHeader(err error) string {
	var re *awshttp.ResponseError
	if errors.As(err, &re) && re.Response != nil {
		return re.Response.Header.Get("X-Amz-Bucket-Region")
	}
	return ""
}

// list returns every object under the prefix. A bucket in another region
// than settings say moves the session there once.
func (sess *s3Session) list(ctx context.Context) ([]filesource.Listed, error) {
	objs, err := sess.listOnce(ctx)
	if region := regionHeader(err); region != "" && region != sess.region && wrongRegion(err) {
		sess.region, sess.client = region, sess.newClient(region)
		objs, err = sess.listOnce(ctx)
	}
	return objs, err
}

func wrongRegion(err error) bool {
	var re *awshttp.ResponseError
	code := apiCode(err)
	return code == "PermanentRedirect" || code == "AuthorizationHeaderMalformed" || errors.As(err, &re) && re.HTTPStatusCode() == http.StatusMovedPermanently
}

func (sess *s3Session) listOnce(ctx context.Context) ([]filesource.Listed, error) {
	var objs []filesource.Listed
	seen := map[string]bool{}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(sess.settings.Bucket), Prefix: aws.String(sess.settings.Prefix)}
	for {
		out, err := sess.client.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, s3Failure(err, opList, sess.settings, "")
		}
		for _, o := range out.Contents {
			objs = append(objs, filesource.Listed{
				Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), ETag: aws.ToString(o.ETag), LastModified: aws.ToTime(o.LastModified),
				Archived: o.StorageClass == s3types.ObjectStorageClassGlacier || o.StorageClass == s3types.ObjectStorageClassDeepArchive,
			})
		}
		if len(objs) > maxS3Keys {
			return nil, invalid(msgTooManyKeys)
		}
		token := aws.ToString(out.NextContinuationToken)
		if !aws.ToBool(out.IsTruncated) || token == "" {
			return objs, nil
		}
		// A store that hands back a token twice would page forever.
		if seen[token] {
			return nil, refused(errors.New("repeated continuation token"), msgUnreadable)
		}
		seen[token] = true
		in.ContinuationToken = aws.String(token)
	}
}

// errGrew is an object or table larger than its listing said.
var errGrew = errors.New("connector: s3 object grew")

// fetch downloads one object, unchanged since it was listed, to path. A
// body that breaks off is read again, up to objectReadTries times. count
// sees each read's bytes and may stop the download.
func (sess *s3Session) fetch(ctx context.Context, o Object, path string, count func(int64) error) (int64, error) {
	var lastErr error
	for try := 0; try < objectReadTries; try++ {
		var got int64
		n, err := sess.fetchOnce(ctx, o, path, func(m int64) error {
			got += m
			return count(m)
		})
		if err == nil {
			return n, nil
		}
		// The next try rewrites the file, so this one's bytes stop counting.
		_ = count(-got) // giving bytes back cannot go over a limit
		var read *bodyError
		if !errors.As(err, &read) || ctx.Err() != nil {
			return 0, err
		}
		lastErr = read.err
	}
	return 0, refused(lastErr, msgUnreadable)
}

// bodyError is a body that broke off, which another read may get whole.
type bodyError struct{ err error }

func (e *bodyError) Error() string { return "read body: " + e.err.Error() }

func (sess *s3Session) fetchOnce(ctx context.Context, o Object, path string, count func(int64) error) (_ int64, err error) {
	k := sess.k
	objCtx, cancel := context.WithTimeout(ctx, k.timeouts.object+time.Duration(o.Size>>20)*k.timeouts.perMiB)
	defer cancel()
	out, err := sess.client.GetObject(objCtx, &s3.GetObjectInput{
		Bucket: aws.String(sess.settings.Bucket), Key: aws.String(o.Key), IfMatch: aws.String(o.ETag),
	})
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, s3Failure(err, opGet, sess.settings, o.Key)
	}
	defer func() { _ = out.Body.Close() }() // best-effort: only read
	f, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	body := newIdleReader(out.Body, k.timeouts.idle, cancel)
	defer body.stop()
	var n int64
	buf := make([]byte, 256<<10)
	for {
		m, rerr := body.Read(buf)
		if m > 0 {
			n += int64(m)
			if n > o.Size {
				return 0, sess.changed(o.Key, errGrew)
			}
			if err := count(int64(m)); err != nil {
				return 0, sess.changed(o.Key, err)
			}
			if _, err := f.Write(buf[:m]); err != nil {
				return 0, err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return n, nil
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, &bodyError{err: rerr}
		}
	}
}

func (sess *s3Session) changed(key string, err error) error {
	s3Files.WithLabelValues("unknown", "changed").Inc()
	return refused(err, "%s changed while Helix read it; Helix will retry", key)
}

// idleReader cancels a download whose body sends nothing for idle, so a
// stalled store fails in minutes rather than at the attempt's end.
type idleReader struct {
	r     io.Reader
	idle  time.Duration
	timer *time.Timer
}

func newIdleReader(r io.Reader, idle time.Duration, cancel context.CancelFunc) *idleReader {
	return &idleReader{r: r, idle: idle, timer: time.AfterFunc(idle, cancel)}
}

func (i *idleReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if n > 0 {
		i.timer.Reset(i.idle)
	}
	return n, err
}

func (i *idleReader) stop() { i.timer.Stop() }

// s3Op is what a request was doing, which decides its error's message.
type s3Op uint8

const (
	opList s3Op = iota
	opGet
)

// s3Failure gives a request's error the customer's message. The upstream
// error stays wrapped for the log; it never reaches the message.
func s3Failure(err error, op s3Op, s S3Settings, key string) error {
	var re *awshttp.ResponseError
	code := apiCode(err)
	switch {
	case errors.Is(err, ErrPrivateAddress):
		return invalid(msgPrivate)
	case code == "NoSuchBucket":
		return invalid("bucket %s does not exist", s.Bucket)
	case code == "InvalidAccessKeyId" || code == "SignatureDoesNotMatch":
		return refused(err, msgKeyRejected)
	// An object archived since the listing answers 403 too; it changed.
	case op == opGet && (code == "PreconditionFailed" || code == "NoSuchKey" || code == "InvalidObjectState" ||
		errors.As(err, &re) && re.HTTPStatusCode() == http.StatusPreconditionFailed):
		s3Files.WithLabelValues("unknown", "changed").Inc()
		return refused(err, "%s changed while Helix read it; Helix will retry", key)
	case isAccessDenied(err) && op == opList:
		return refused(err, "Helix cannot list s3://%s/%s; allow s3:ListBucket on this folder", s.Bucket, s.Prefix)
	case isAccessDenied(err):
		s3Files.WithLabelValues("unknown", "denied").Inc()
		return refused(err, "Helix cannot read %s; allow s3:GetObject on it, and kms:Decrypt if the bucket uses a KMS key", key)
	case code == "SlowDown" || errors.As(err, &re) && re.HTTPStatusCode() >= 500:
		return refused(err, msgThrottled)
	}
	return refused(err, msgUnreadable)
}

func apiCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

// isAccessDenied is a refusal, as an error code or a bodiless 403.
func isAccessDenied(err error) bool {
	var re *awshttp.ResponseError
	switch apiCode(err) {
	case "AccessDenied", "Forbidden", "AllAccessDisabled":
		return true
	}
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusForbidden
}

// rangedReader reads an object through ranged GETs, so a Parquet footer
// costs two small requests.
type rangedReader struct {
	ctx  context.Context
	sess *s3Session
	o    Object
}

func (r rangedReader) ReadAt(p []byte, off int64) (int, error) {
	out, err := r.sess.client.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket: aws.String(r.sess.settings.Bucket), Key: aws.String(r.o.Key), IfMatch: aws.String(r.o.ETag),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)),
	})
	if err != nil {
		return 0, err
	}
	defer func() { _ = out.Body.Close() }() // best-effort: only read
	return io.ReadFull(out.Body, p)
}

// sampleTables reads a few files of each table for columns, a preview and
// a key guess, within the test's bounds: at most maxSampleGets GETs,
// fetchesAtOnce at a time, decodes shared across the process, and a soft
// deadline after which tables come back without columns.
func (k *s3Kind) sampleTables(ctx context.Context, sess *s3Session, tables []Table, byAge [][]Object) {
	ctx, cancel := context.WithTimeout(ctx, k.timeouts.sample)
	defer cancel()
	type result struct {
		sampled filesource.Sampled
		problem string
	}
	results := make([][]result, len(tables))
	var mu sync.Mutex
	fetches := make(chan struct{}, fetchesAtOnce)
	var wg sync.WaitGroup
	gets := 0
	for _, job := range sampleSchedule(byAge) {
		// A Parquet footer takes two ranged GETs: its length, then itself.
		cost := 1
		if job.o.Format == filesource.FormatParquet {
			cost = 2
		}
		if gets+cost > maxSampleGets || ctx.Err() != nil {
			break
		}
		gets += cost
		fetches <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-fetches }()
			sampled, problem, ok := k.sampleFile(ctx, sess, job.o)
			if !ok {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			results[job.table] = append(results[job.table], result{sampled, problem})
		}()
	}
	wg.Wait()
	var previewBytes, columnBytes int
	for i := range tables {
		var sets [][]filesource.Column
		var preview []json.RawMessage
		key := true
		for _, r := range results[i] {
			if r.problem != "" && tables[i].Problem == "" {
				tables[i].Problem = r.problem
			}
			sets = append(sets, r.sampled.Columns)
			key = key && slices.Equal(r.sampled.PrimaryKey, []string{"id"})
			for _, row := range r.sampled.Rows {
				if len(preview) < filesource.DefaultSampleLimits.MaxRows {
					preview = append(preview, json.RawMessage(row))
				}
			}
		}
		if len(sets) == 0 {
			continue
		}
		var cols []Column
		size := 0
		for _, c := range filesource.Merge(sets...) {
			cols = append(cols, Column{Name: c.Name, Type: c.Type.String(), Nullable: c.Nullable})
			size += len(c.Name) + len(c.Type.String()) + 16
		}
		if columnBytes+size > maxColumnBytes {
			continue
		}
		columnBytes += size
		tables[i].Columns = cols
		if key && slices.ContainsFunc(cols, func(c Column) bool { return c.Name == "id" }) {
			tables[i].PrimaryKey = []string{"id"}
		}
		size = 0
		for _, row := range preview {
			size += len(row)
		}
		if previewBytes+size <= maxPreviewBytes {
			previewBytes += size
			tables[i].Preview = preview
		}
	}
}

type sampleJob struct {
	table int
	o     Object
}

// sampleSchedule orders the files to sample: each table's newest first,
// then, round by round, its oldest and newest left, up to filesPerTable.
// byAge holds each table's files oldest first.
func sampleSchedule(byAge [][]Object) []sampleJob {
	remaining := make([][]Object, len(byAge))
	for i, files := range byAge {
		remaining[i] = slices.Clone(files)
	}
	var jobs []sampleJob
	for round := 0; round < filesPerTable; round++ {
		for i := range remaining {
			if len(remaining[i]) == 0 {
				continue
			}
			pick := len(remaining[i]) - 1
			if round%2 == 1 {
				pick = 0
			}
			jobs = append(jobs, sampleJob{table: i, o: remaining[i][pick]})
			remaining[i] = slices.Delete(remaining[i], pick, pick+1)
		}
	}
	return jobs
}

// sampleFile reads one file's start, or a Parquet footer. ok is false when
// the sample says nothing: the deadline passed, or a bound was reached.
func (k *s3Kind) sampleFile(ctx context.Context, sess *s3Session, o Object) (filesource.Sampled, string, bool) {
	select {
	case k.decodes <- struct{}{}:
	case <-ctx.Done():
		return filesource.Sampled{}, "", false
	}
	defer func() { <-k.decodes }()
	var sampled filesource.Sampled
	var err error
	if o.Format == filesource.FormatParquet {
		sampled.Columns, _, err = filesource.ParquetFooter(ctx, rangedReader{ctx: ctx, sess: sess, o: o}, o.Size, filesource.DefaultSampleLimits)
	} else {
		var out *s3.GetObjectOutput
		out, err = sess.client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(sess.settings.Bucket), Key: aws.String(o.Key), IfMatch: aws.String(o.ETag),
			Range: aws.String(fmt.Sprintf("bytes=0-%d", sampleRangeSize-1)),
		})
		if err == nil {
			sampled, err = filesource.Sample(ctx, out.Body, o.Key, o.Format, o.Size > sampleRangeSize, filesource.DefaultSampleLimits)
			_ = out.Body.Close() // best-effort: only read
		}
	}
	switch {
	case err == nil:
		return sampled, "", true
	case ctx.Err() != nil, errors.Is(err, filesource.ErrSampleBudget):
		// A file over the sample's bounds adds no columns until the first sync.
		return filesource.Sampled{}, "", false
	case isAccessDenied(err):
		return filesource.Sampled{}, msgSampleDeny, true
	}
	var e *filesource.Error
	if errors.As(err, &e) {
		return filesource.Sampled{}, e.Error(), true
	}
	if errors.Is(err, filesource.ErrParquet) || errors.Is(err, filesource.ErrTooManyColumns) || errors.Is(err, filesource.ErrTooDeep) {
		return filesource.Sampled{}, o.Key + ": " + err.Error(), true
	}
	return filesource.Sampled{}, "", false
}
