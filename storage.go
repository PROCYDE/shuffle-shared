package shuffle

/*
	Optional object storage for Shuffle.

	Shuffle historically persists binary data (file uploads, oversized execution
	payloads, workflow parameters and app image metadata) on the local filesystem
	under SHUFFLE_FILE_LOCATION. This file adds an optional S3-compatible backend
	so deployments can run without persistent volumes for this data.

	Configuration (environment variables):
	  SHUFFLE_STORAGE_BACKEND   "local" (default), "s3"
	  SHUFFLE_S3_ENDPOINT       S3-compatible endpoint, e.g. "s3.amazonaws.com" or
	                            "minio.default.svc.cluster.local:9000"
	  SHUFFLE_S3_BUCKET         Bucket name (required for s3)
	  SHUFFLE_S3_PREFIX         Optional key prefix inside the bucket
	  SHUFFLE_S3_REGION         Region (default "us-east-1")
	  SHUFFLE_S3_ACCESS_KEY     Access key (falls back to AWS_ACCESS_KEY_ID)
	  SHUFFLE_S3_SECRET_KEY     Secret key (falls back to AWS_SECRET_ACCESS_KEY)
	  SHUFFLE_S3_USE_PATH_STYLE Force path-style addressing (default true, required
	                            for MinIO and most self-hosted S3 implementations)
	  SHUFFLE_S3_INSECURE       Use http:// against the endpoint

	Keys are always relative to SHUFFLE_FILE_LOCATION (the same relative layout
	used on disk), so switching between backends does not change how paths are
	referenced in the database. The local backend is rooted at
	SHUFFLE_FILE_LOCATION via os.Root, which refuses path traversal outside of
	the storage directory.
*/

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Storage is a minimal blob interface for file data (uploads, oversized
// execution payloads, workflow parameters, app image metadata). Keys are
// relative paths using
// forward slashes (e.g. "large_executions/orgid/execid_actionid") and are
// interpreted relative to SHUFFLE_FILE_LOCATION. The read side follows the
// standard library io/fs conventions (fs.File, os.ErrNotExist-compatible
// errors); the write side is a minimal streaming API.
type Storage interface {
	// Open returns a reader for the stored object. Not-found errors are
	// always compatible with os.IsNotExist.
	Open(ctx context.Context, key string) (fs.File, error)
	// Write stores the object, replacing any previous content. A size of -1
	// means unknown.
	Write(ctx context.Context, key string, data io.Reader, size int64) error
	// Append adds data to the end of the object, creating it if missing.
	Append(ctx context.Context, key string, data io.Reader) error
	Delete(ctx context.Context, key string) error
	Exists(ctx context.Context, key string) (bool, error)
}

var (
	storageOnce     sync.Once
	storageInstance Storage
	storageErr      error
)

// GetFileStorage returns the configured storage backend. Defaults to the
// local filesystem; never returns a nil backend without an error.
func GetFileStorage() (Storage, error) {
	storageOnce.Do(func() {
		backend := strings.ToLower(strings.TrimSpace(os.Getenv("SHUFFLE_STORAGE_BACKEND")))
		if backend == "" {
			backend = "local"
		}

		switch backend {
		case "local", "filesystem", "":
			storageInstance = &localStorage{}
		case "s3", "s3compatible", "minio":
			s3, err := newS3Storage()
			if err != nil {
				storageErr = err
				return
			}
			storageInstance = s3
		default:
			storageErr = unknownBackendError(backend)
		}
	})

	return storageInstance, storageErr
}

func unknownBackendError(backend string) error {
	return fmt.Errorf("unknown SHUFFLE_STORAGE_BACKEND '%s' (supported: local, s3)", backend)
}

// storageLocation returns the root directory for local file storage.
func storageLocation() string {
	if len(basepath) > 0 {
		return basepath
	}
	return "files"
}

// storageKey converts an absolute or basepath-prefixed path into a
// backend-relative object key. Absolute paths under the file location are
// normalized; relative paths pass through unchanged.
func storageKey(path string) string {
	base := strings.TrimRight(basepath, "/")
	clean := filepath.ToSlash(path)

	if len(base) > 0 && strings.HasPrefix(clean, base+"/") {
		clean = strings.TrimPrefix(clean, base+"/")
	}

	return strings.TrimLeft(clean, "/")
}

// localStorage stores objects in the local filesystem under
// SHUFFLE_FILE_LOCATION, using os.Root so that keys cannot escape the storage
// directory. The root handle is opened per operation: basepath is a process
// constant in practice, and the open cost is negligible next to the I/O it
// wraps.
type localStorage struct{}

func newLocalRoot(key string) (*os.Root, error) {
	rootPath := storageLocation()
	if err := os.MkdirAll(rootPath, 0755); err != nil {
		return nil, fmt.Errorf("create storage directory %s: %w", rootPath, err)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open storage root %s: %w", rootPath, err)
	}

	return root, nil
}

func (l *localStorage) Open(ctx context.Context, key string) (fs.File, error) {
	root, err := newLocalRoot(key)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	f, err := root.Open(key)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (l *localStorage) Write(ctx context.Context, key string, data io.Reader, size int64) error {
	root, err := newLocalRoot(key)
	if err != nil {
		return err
	}
	defer root.Close()

	if dir := filepath.Dir(key); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory for %s: %w", key, err)
		}
	}

	f, err := root.OpenFile(key, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("open %s for writing: %w", key, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, data); err != nil {
		return fmt.Errorf("write %s: %w", key, err)
	}
	return nil
}

func (l *localStorage) Append(ctx context.Context, key string, data io.Reader) error {
	root, err := newLocalRoot(key)
	if err != nil {
		return err
	}
	defer root.Close()

	if dir := filepath.Dir(key); dir != "." {
		if err := root.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory for %s: %w", key, err)
		}
	}

	f, err := root.OpenFile(key, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open %s for appending: %w", key, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, data); err != nil {
		return fmt.Errorf("append to %s: %w", key, err)
	}
	return nil
}

func (l *localStorage) Delete(ctx context.Context, key string) error {
	root, err := newLocalRoot(key)
	if err != nil {
		return err
	}
	defer root.Close()

	return root.Remove(key)
}

func (l *localStorage) Exists(ctx context.Context, key string) (bool, error) {
	root, err := newLocalRoot(key)
	if err != nil {
		return false, err
	}
	defer root.Close()

	_, err = root.Stat(key)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// s3Storage stores objects in an S3-compatible bucket using minio-go.
type s3Storage struct {
	client *minio.Client
	bucket string
	prefix string
}

func newS3Storage() (*s3Storage, error) {
	endpoint := strings.TrimSpace(os.Getenv("SHUFFLE_S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("SHUFFLE_S3_BUCKET"))

	if len(bucket) == 0 {
		return nil, errors.New("SHUFFLE_S3_BUCKET is required when SHUFFLE_STORAGE_BACKEND=s3")
	}

	accessKey := os.Getenv("SHUFFLE_S3_ACCESS_KEY")
	if len(accessKey) == 0 {
		accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}

	secretKey := os.Getenv("SHUFFLE_S3_SECRET_KEY")
	if len(secretKey) == 0 {
		secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}

	if len(accessKey) == 0 || len(secretKey) == 0 {
		// Anonymous/ambient credentials (e.g. IRSA on EKS). minio-go will
		// fall back to the IAM role when both keys are empty.
		accessKey, secretKey = "", ""
	}

	region := strings.TrimSpace(os.Getenv("SHUFFLE_S3_REGION"))
	if len(region) == 0 {
		region = "us-east-1"
	}

	usePathStyle := true
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SHUFFLE_S3_USE_PATH_STYLE"))) {
	case "false", "0", "no":
		usePathStyle = false
	}

	secure := true
	if strings.HasPrefix(endpoint, "http://") {
		secure = false
		endpoint = strings.TrimPrefix(endpoint, "http://")
	} else if strings.HasPrefix(endpoint, "https://") {
		endpoint = strings.TrimPrefix(endpoint, "https://")
	} else if strings.EqualFold(strings.TrimSpace(os.Getenv("SHUFFLE_S3_INSECURE")), "true") {
		secure = false
	}

	if len(endpoint) == 0 {
		endpoint = "s3.amazonaws.com"
	}

	return newS3StorageWithConfig(endpoint, bucket, os.Getenv("SHUFFLE_S3_PREFIX"), region, accessKey, secretKey, usePathStyle, secure)
}

func newS3StorageWithConfig(endpoint, bucket, prefix, region, accessKey, secretKey string, usePathStyle, secure bool) (*s3Storage, error) {
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: bucketLookupType(usePathStyle),
	})
	if err != nil {
		return nil, fmt.Errorf("initialize S3 client for endpoint '%s': %w", endpoint, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err = client.BucketExists(ctx, bucket); err != nil {
		return nil, fmt.Errorf("verify access to S3 bucket '%s': %w", bucket, err)
	}

	prefix = strings.Trim(prefix, "/")
	if len(prefix) > 0 {
		prefix += "/"
	}

	log.Printf("[INFO] Using S3-compatible object storage: endpoint=%s bucket=%s prefix='%s' pathStyle=%t", endpoint, bucket, prefix, usePathStyle)

	return &s3Storage{
		client: client,
		bucket: bucket,
		prefix: prefix,
	}, nil
}

func bucketLookupType(usePathStyle bool) minio.BucketLookupType {
	if usePathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupAuto
}

func (s *s3Storage) objectKey(key string) string {
	return s.prefix + strings.TrimLeft(key, "/")
}

func (s *s3Storage) Open(ctx context.Context, key string) (fs.File, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.objectKey(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, objectError(key, err)
	}

	// minio-go performs the actual request lazily; probe it so callers get a
	// not-found error from Open rather than from the first Read.
	if _, err = obj.Stat(); err != nil {
		obj.Close()
		return nil, objectError(key, err)
	}

	return &s3File{obj: obj, key: key}, nil
}

// s3File adapts minio's *Object to the standard library fs.File interface:
// minio.Object.Stat returns minio.ObjectInfo rather than fs.FileInfo.
type s3File struct {
	obj *minio.Object
	key string
}

func (f *s3File) Read(p []byte) (int, error)                   { return f.obj.Read(p) }
func (f *s3File) Close() error                                 { return f.obj.Close() }
func (f *s3File) Seek(offset int64, whence int) (int64, error) { return f.obj.Seek(offset, whence) }

func (f *s3File) Stat() (fs.FileInfo, error) {
	info, err := f.obj.Stat()
	if err != nil {
		return nil, objectError(f.key, err)
	}
	return s3FileInfo{info}, nil
}

// s3FileInfo implements fs.FileInfo on top of minio.ObjectInfo.
type s3FileInfo struct {
	info minio.ObjectInfo
}

func (i s3FileInfo) Name() string { return path.Base(i.info.Key) }
func (i s3FileInfo) Size() int64  { return i.info.Size }

func (i s3FileInfo) Mode() fs.FileMode {
	if strings.HasSuffix(i.info.Key, "/") {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

func (i s3FileInfo) ModTime() time.Time { return i.info.LastModified }
func (i s3FileInfo) IsDir() bool        { return strings.HasSuffix(i.info.Key, "/") }
func (i s3FileInfo) Sys() any           { return nil }

func (s *s3Storage) Write(ctx context.Context, key string, data io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.objectKey(key), data, size, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("put object '%s': %w", key, err)
	}
	return nil
}

// Append emulates appends via read-modify-write, which is the best an object
// store can offer. The existing object is buffered in memory; file uploads
// using this path are bounded by maxFileSize (10MB on-prem).
func (s *s3Storage) Append(ctx context.Context, key string, data io.Reader) error {
	existing, err := s.Open(ctx, key)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("read object '%s' for append: %w", key, err)
		}
		existing = nil
	}
	defer func() {
		if existing != nil {
			existing.Close()
		}
	}()

	merged := new(bytes.Buffer)
	if existing != nil {
		if _, err = io.Copy(merged, existing); err != nil {
			return fmt.Errorf("read object '%s' for append: %w", key, err)
		}
	}

	if _, err = io.Copy(merged, data); err != nil {
		return fmt.Errorf("buffer append data for '%s': %w", key, err)
	}

	return s.Write(ctx, key, merged, int64(merged.Len()))
}

func (s *s3Storage) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.objectKey(key), minio.RemoveObjectOptions{})
}

func (s *s3Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.objectKey(key), minio.StatObjectOptions{})
	if err != nil {
		resp := minio.ToErrorResponse(err)
		if resp.Code == "NoSuchKey" {
			return false, nil
		}
		return false, objectError(key, err)
	}
	return true, nil
}

// objectError wraps S3 errors so callers can keep using os.IsNotExist on them.
func objectError(key string, err error) error {
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" {
		return &os.PathError{Op: "open", Path: key, Err: os.ErrNotExist}
	}
	return fmt.Errorf("s3 object '%s': %w", key, err)
}

// StorageAreaName returns the StorageArea metadata value for newly created
// files, based on the configured storage backend.
func StorageAreaName() string {
	if backend, err := GetFileStorage(); err == nil {
		if _, isLocal := backend.(*localStorage); !isLocal {
			return "s3"
		}
	}
	return "local"
}

// The helpers below are the []byte-shaped convenience API used by the
// storage call sites. They delegate to the streaming backend interface.

// WriteStorageData persists data under the given path. The path is
// interpreted relative to SHUFFLE_FILE_LOCATION (basepath-prefixed or
// absolute paths are normalized), replacing any previous content.
func WriteStorageData(path string, data []byte) error {
	backend, err := GetFileStorage()
	if err != nil {
		return err
	}

	return backend.Write(context.Background(), storageKey(path), bytes.NewReader(data), int64(len(data)))
}

// ReadStorageData reads data previously written by WriteStorageData.
func ReadStorageData(path string) ([]byte, error) {
	backend, err := GetFileStorage()
	if err != nil {
		return nil, err
	}

	reader, err := backend.Open(context.Background(), storageKey(path))
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	return io.ReadAll(reader)
}

// DeleteStorageData removes data written by WriteStorageData.
func DeleteStorageData(path string) error {
	backend, err := GetFileStorage()
	if err != nil {
		return err
	}

	return backend.Delete(context.Background(), storageKey(path))
}
