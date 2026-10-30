package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/aws-abaschen/strata/backend/internal/core"
)

// S3 stores objects and sealed changes in one bucket:
//
//	repos/<repo>/objects/<h[0:2]>/<h>   file and tree bytes
//	repos/<repo>/changes/<rev>.json     sealed change documents
//
// Every key is written once (If-None-Match: *), so history cannot be
// overwritten even by a client holding a valid upload URL.
type S3 struct {
	Client  *s3.Client
	Presign *s3.PresignClient
	Bucket  string
	// URLTTL is how long presigned URLs live. Default 15 minutes.
	URLTTL time.Duration
}

func objectKey(repo string, h core.Hash) string {
	return fmt.Sprintf("repos/%s/objects/%s/%s", repo, h[:2], h)
}

func (s *S3) ttl(o *s3.PresignOptions) {
	o.Expires = s.URLTTL
	if o.Expires == 0 {
		o.Expires = 15 * time.Minute
	}
}

func httpStatus(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// Exists implements Objects.
func (s *S3) Exists(ctx context.Context, repo string, h core.Hash) (bool, error) {
	_, err := s.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.Bucket, Key: aws.String(objectKey(repo, h))})
	if err == nil {
		return true, nil
	}
	if httpStatus(err) == http.StatusNotFound {
		return false, nil
	}
	return false, err
}

// Get implements Objects.
func (s *S3) Get(ctx context.Context, repo string, h core.Hash, max int64) ([]byte, error) {
	out, err := s.Client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: aws.String(objectKey(repo, h))})
	if err != nil {
		if httpStatus(err) == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("object %s larger than %d bytes", h.Short(), max)
	}
	return b, nil
}

// PresignPut implements Objects. The URL signs the SHA-256 checksum, so S3
// itself rejects any body that does not match the content address.
func (s *S3) PresignPut(ctx context.Context, repo string, h core.Hash, size int64) (Upload, error) {
	raw, err := hex.DecodeString(string(h))
	if err != nil {
		return Upload{}, err
	}
	req, err := s.Presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:         &s.Bucket,
		Key:            aws.String(objectKey(repo, h)),
		ContentLength:  aws.Int64(size),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(raw)),
		IfNoneMatch:    aws.String("*"),
	}, s.ttl)
	if err != nil {
		return Upload{}, err
	}
	return Upload{Hash: h, URL: req.URL, Method: req.Method, Headers: headerMap(req.SignedHeader)}, nil
}

// PresignGet implements Objects.
func (s *S3) PresignGet(ctx context.Context, repo string, h core.Hash) (string, error) {
	req, err := s.Presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.Bucket, Key: aws.String(objectKey(repo, h))}, s.ttl)
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

// PutChangeDoc implements Objects.
func (s *S3) PutChangeDoc(ctx context.Context, c core.Change, doc []byte) error {
	_, err := s.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.Bucket,
		Key:         aws.String(fmt.Sprintf("repos/%s/changes/%s.json", c.Repo, c.Revision)),
		Body:        bytes.NewReader(doc),
		ContentType: aws.String("application/json"),
		IfNoneMatch: aws.String("*"),
	})
	if httpStatus(err) == http.StatusPreconditionFailed {
		return ErrExists
	}
	return err
}
