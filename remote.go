package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// remoteStore talks to a real S3-compatible backend.
type remoteStore struct {
	client *s3.Client
}

func newRemoteStore(ctx context.Context, endpoint, region, accessKey, secretKey string) (*remoteStore, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		),
	)
	if err != nil {
		return nil, err
	}
	opts := []func(*s3.Options){}
	if endpoint != "" {
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		})
	}
	return &remoteStore{client: s3.NewFromConfig(cfg, opts...)}, nil
}

func mapS3Error(err error) error {
	var nsk *types.NoSuchKey
	var nsb *types.NoSuchBucket
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nsb) || errors.As(err, &nf) {
		return ErrNotFound
	}
	return err
}

func (s *remoteStore) Get(ctx context.Context, bucket, key string) (io.ReadCloser, *ObjectMeta, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, nil, mapS3Error(err)
	}
	meta := &ObjectMeta{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
	}
	return out.Body, meta, nil
}

func (s *remoteStore) Head(ctx context.Context, bucket, key string) (*ObjectMeta, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, mapS3Error(err)
	}
	return &ObjectMeta{
		Key:          key,
		Size:         aws.ToInt64(out.ContentLength),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: aws.ToTime(out.LastModified),
		ContentType:  aws.ToString(out.ContentType),
	}, nil
}

func (s *remoteStore) Put(ctx context.Context, bucket, key string, body io.Reader, contentType string) (*ObjectMeta, error) {
	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: time.Now().UTC(),
	}, nil
}

func (s *remoteStore) List(ctx context.Context, bucket string) ([]ObjectMeta, error) {
	var out []ObjectMeta
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, mapS3Error(err)
		}
		for _, obj := range resp.Contents {
			out = append(out, ObjectMeta{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				ETag:         strings.Trim(aws.ToString(obj.ETag), `"`),
				LastModified: aws.ToTime(obj.LastModified),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}

func (s *remoteStore) ListBuckets(ctx context.Context) ([]string, error) {
	out, err := s.client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return nil, err
	}
	var buckets []string
	for _, b := range out.Buckets {
		buckets = append(buckets, aws.ToString(b.Name))
	}
	return buckets, nil
}

func (s *remoteStore) BucketExists(ctx context.Context, bucket string) (bool, error) {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err == nil {
		return true, nil
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return false, nil
	}
	return false, err
}

func (s *remoteStore) CreateBucket(ctx context.Context, bucket string) error {
	_, err := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	return err
}

func (s *remoteStore) InitiateMultipart(ctx context.Context, bucket, key, contentType string) (string, error) {
	out, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.UploadId), nil
}

func (s *remoteStore) UploadPart(ctx context.Context, bucket, key, uploadID string, partNumber int32, body io.Reader) (string, error) {
	out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(partNumber),
		Body:       body,
	})
	if err != nil {
		return "", err
	}
	return strings.Trim(aws.ToString(out.ETag), `"`), nil
}

func (s *remoteStore) CompleteMultipart(ctx context.Context, bucket, key, uploadID string, parts []CompletedPart) (*ObjectMeta, error) {
	var completed []types.CompletedPart
	for _, p := range parts {
		completed = append(completed, types.CompletedPart{
			PartNumber: aws.Int32(p.PartNumber),
			ETag:       aws.String(`"` + p.ETag + `"`),
		})
	}
	out, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return nil, err
	}
	return &ObjectMeta{
		Key:          key,
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		LastModified: time.Now().UTC(),
	}, nil
}

func (s *remoteStore) AbortMultipart(ctx context.Context, bucket, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	return err
}

func (s *remoteStore) ListParts(ctx context.Context, bucket, key, uploadID string) ([]PartInfo, error) {
	var out []PartInfo
	var marker *string
	for {
		resp, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, err
		}
		for _, p := range resp.Parts {
			out = append(out, PartInfo{
				PartNumber:   aws.ToInt32(p.PartNumber),
				ETag:         strings.Trim(aws.ToString(p.ETag), `"`),
				Size:         aws.ToInt64(p.Size),
				LastModified: aws.ToTime(p.LastModified),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		marker = resp.NextPartNumberMarker
	}
	return out, nil
}

func (s *remoteStore) ListMultipartUploads(ctx context.Context, bucket string) ([]MultipartInfo, error) {
	var out []MultipartInfo
	var keyMarker, uploadIDMarker *string
	for {
		resp, err := s.client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: uploadIDMarker,
		})
		if err != nil {
			return nil, err
		}
		for _, u := range resp.Uploads {
			out = append(out, MultipartInfo{
				Key:       aws.ToString(u.Key),
				UploadID:  aws.ToString(u.UploadId),
				Initiated: aws.ToTime(u.Initiated),
			})
		}
		if resp.IsTruncated == nil || !*resp.IsTruncated {
			break
		}
		keyMarker = resp.NextKeyMarker
		uploadIDMarker = resp.NextUploadIdMarker
	}
	return out, nil
}
