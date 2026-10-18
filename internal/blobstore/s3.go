package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config names an S3-API bucket: a self-hosted Garage or SeaweedFS, or any
// other store that speaks the S3 API. Prefix keeps notify's objects apart
// when the bucket is shared.
type S3Config struct {
	Endpoint  string // host:port, no scheme
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	UseTLS    bool
}

// S3 keeps objects in an S3-API bucket under prefix/ab/cd/<sha256>.
type S3 struct {
	c      *minio.Client
	bucket string
	prefix string
}

// NewS3 connects to the bucket in cfg. It does not create the bucket.
func NewS3(cfg S3Config) (*S3, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" {
		return nil, errors.New("blobstore: s3 needs an endpoint and a bucket")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseTLS,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("blobstore: s3 client: %w", err)
	}
	return &S3{c: c, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// emptySHA256 is the address of the empty object. Its bytes are known, so it
// is never uploaded and always present: S3 APIs differ on accepting a
// zero-length PUT, and notify need not depend on which.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3) key(addr string) string {
	return path.Join(s.prefix, addr[0:2], addr[2:4], addr)
}

// Put hashes r into a temporary file, then uploads it under its hash with a
// known size, so large mail never sits in memory. Bytes already stored are
// not uploaded again.
func (s *S3) Put(ctx context.Context, r io.Reader) (Ref, error) {
	tmp, err := os.CreateTemp("", "notify-blob-*")
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: temp file: %w", err)
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return Ref{}, fmt.Errorf("blobstore: read: %w", err)
	}
	ref := Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	if ref.SHA256 == emptySHA256 {
		return ref, nil // the empty object is never stored; see emptySHA256
	}
	if _, err := s.Head(ctx, ref.SHA256); err == nil {
		return ref, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Ref{}, err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return Ref{}, err
	}
	if _, err := s.c.PutObject(ctx, s.bucket, s.key(ref.SHA256), tmp, n, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	}); err != nil {
		return Ref{}, fmt.Errorf("blobstore: upload %s: %w", ref.SHA256, err)
	}
	return ref, nil
}

// Get opens the object at addr.
func (s *S3) Get(ctx context.Context, addr string) (io.ReadCloser, error) {
	if addr == emptySHA256 {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	if _, err := s.Head(ctx, addr); err != nil {
		return nil, err
	}
	obj, err := s.c.GetObject(ctx, s.bucket, s.key(addr), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blobstore: get %s: %w", addr, err)
	}
	return obj, nil
}

// Head reports the object's size.
func (s *S3) Head(ctx context.Context, addr string) (Ref, error) {
	if !validAddr(addr) {
		return Ref{}, fmt.Errorf("blobstore: invalid address %q", addr)
	}
	if addr == emptySHA256 {
		return Ref{SHA256: addr, Size: 0}, nil
	}
	info, err := s.c.StatObject(ctx, s.bucket, s.key(addr), minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return Ref{}, ErrNotFound
		}
		return Ref{}, fmt.Errorf("blobstore: stat %s: %w", addr, err)
	}
	return Ref{SHA256: addr, Size: info.Size}, nil
}
