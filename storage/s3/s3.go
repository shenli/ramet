// SPDX-License-Identifier: Apache-2.0
// Package s3 implements content-addressed storage on S3-compatible endpoints.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/shenli/ramet/storage"
)

type Options struct {
	Endpoint     string
	Region       string
	Bucket       string
	Prefix       string
	AccessKey    string
	SecretKey    string
	SessionToken string
}
type Store struct {
	client                   *minio.Client
	bucket, prefix, identity string
}

func New(opts Options) (*Store, error) {
	u, err := url.Parse(opts.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("S3 endpoint must be an http(s) origin without credentials or a path")
	}
	if opts.Bucket == "" {
		return nil, errors.New("S3 bucket is required")
	}
	prefix := strings.Trim(opts.Prefix, "/")
	if prefix == "" || strings.Contains(prefix, "..") || strings.ContainsAny(prefix, "\\\x00") {
		return nil, errors.New("a nonempty, safe S3 namespace prefix is required")
	}
	creds := credentials.NewEnvAWS()
	if opts.AccessKey != "" || opts.SecretKey != "" {
		if opts.AccessKey == "" || opts.SecretKey == "" {
			return nil, errors.New("both S3 access key and secret key are required")
		}
		creds = credentials.NewStaticV4(opts.AccessKey, opts.SecretKey, opts.SessionToken)
	}
	c, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme == "https", Region: opts.Region})
	if err != nil {
		return nil, err
	}
	identity := u.Scheme + "://" + u.Host + "/" + opts.Bucket + "/" + prefix
	return &Store{client: c, bucket: opts.Bucket, prefix: prefix + "/", identity: identity}, nil
}
func (s *Store) Identity() string { return s.identity }
func (s *Store) Ping(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return err
	}
	if !exists {
		return errors.New("configured S3 bucket does not exist; create it before starting Ramet")
	}
	return nil
}
func (s *Store) Open(ctx context.Context, digest string) (io.ReadCloser, error) {
	if !storage.ValidDigest(digest) {
		return nil, storage.ErrNotFound
	}
	object, err := s.client.GetObject(ctx, s.bucket, s.prefix+digest, minio.GetObjectOptions{})
	if err != nil {
		return nil, mapError(err)
	}
	if _, err = object.Stat(); err != nil {
		object.Close()
		return nil, mapError(err)
	}
	return object, nil
}
func (s *Store) Put(ctx context.Context, ref storage.Ref, src io.ReadSeeker) (bool, error) {
	if !storage.ValidDigest(ref.Digest) || ref.Size < 0 {
		return false, storage.ErrCorrupt
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	h := sha256.New()
	n, err := io.Copy(h, &contextReader{ctx, src})
	if err != nil {
		return false, err
	}
	if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.Digest {
		return false, storage.ErrCorrupt
	}
	existing, err := s.Open(ctx, ref.Digest)
	if err == nil {
		h.Reset()
		n, readErr := io.Copy(h, existing)
		closeErr := existing.Close()
		if readErr != nil {
			return false, readErr
		}
		if closeErr != nil {
			return false, closeErr
		}
		if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.Digest {
			return false, fmt.Errorf("%w: S3 object digest mismatch", storage.ErrCorrupt)
		}
		return false, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if _, err = src.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	info, err := s.client.PutObject(ctx, s.bucket, s.prefix+ref.Digest, src, ref.Size, minio.PutObjectOptions{ContentType: "application/octet-stream", SendContentMd5: true, UserMetadata: map[string]string{"sha256": ref.Digest}})
	if err != nil {
		return false, err
	}
	if info.Size != ref.Size {
		return false, fmt.Errorf("%w: incomplete S3 upload", storage.ErrCorrupt)
	}
	return true, nil
}
func mapError(err error) error {
	code := minio.ToErrorResponse(err).Code
	if code == "NoSuchKey" || code == "NoSuchObject" || code == "NotFound" {
		return storage.ErrNotFound
	}
	return err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
