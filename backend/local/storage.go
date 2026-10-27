package local

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// S3 is an install's S3-compatible store (SeaweedFS in the compose file),
// addressed path-style. Browsers PUT documents to URLs presigned for
// Endpoint, so it must be the same address from the install and from the
// browser.
type S3 struct {
	Endpoint        string `koanf:"endpoint"`
	AccessKeyID     string `koanf:"access_key_id"`
	SecretAccessKey string `koanf:"secret_access_key"`
}

// AWSConfig addresses store: every S3 client built from it (snapshots,
// documents and their presigned URLs) talks path-style to store.Endpoint
// with its static keys, signing for region. Connectors to a customer's S3
// build clients of their own and never inherit it.
func AWSConfig(region string, store S3) aws.Config {
	return aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(store.AccessKeyID, store.SecretAccessKey, ""),
		// S3-compatible stores need no checksum S3 does not require.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		ServiceOptions: []func(string, any){func(_ string, options any) {
			if o, ok := options.(*s3.Options); ok {
				o.BaseEndpoint, o.UsePathStyle = aws.String(store.Endpoint), true
			}
		}},
	}
}

// Bucketer creates buckets; *s3.Client is one.
type Bucketer interface {
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
}

// CreateBucket creates the install's bucket unless it is there already:
// every start runs it.
func CreateBucket(ctx context.Context, client Bucketer, bucket string) error {
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if err != nil && !errors.As(err, &owned) && !errors.As(err, &exists) {
		return fmt.Errorf("create bucket %s: %w", bucket, err)
	}
	return nil
}
