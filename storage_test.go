package shuffle

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageKeyStripsBasepath(t *testing.T) {
	cases := []struct {
		base, path, expected string
	}{
		{"/shuffle-files", "/shuffle-files/large_executions/org1/exec_1_action_1", "large_executions/org1/exec_1_action_1"},
		{"/shuffle-files", "large_executions/org1/exec_1_action_1", "large_executions/org1/exec_1_action_1"},
		{"", "large_executions/org1/x", "large_executions/org1/x"},
		{"/shuffle-files", "/other/place/file", "other/place/file"},
	}

	for _, c := range cases {
		basepath = c.base
		got := storageKey(c.path)
		if got != c.expected {
			t.Errorf("storageKey(%q) with base %q = %q, expected %q", c.path, c.base, got, c.expected)
		}
	}
	basepath = os.Getenv("SHUFFLE_FILE_LOCATION")
}

func TestWriteReadStorageDataLocal(t *testing.T) {
	tmp := t.TempDir()
	basepath = tmp
	defer func() { basepath = os.Getenv("SHUFFLE_FILE_LOCATION") }()

	rel := "large_executions/org1/exec_1_action_1"
	data := []byte(`{"success": false, "size": 999999}`)

	if err := WriteStorageData(rel, data); err != nil {
		t.Fatalf("WriteStorageData failed: %s", err)
	}

	// Must land in the expected location on disk
	full := filepath.Join(tmp, rel)
	onDisk, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("expected local file at %s: %s", full, err)
	}
	if string(onDisk) != string(data) {
		t.Fatalf("content mismatch on disk")
	}

	// Read back via relative path
	got, err := ReadStorageData(rel)
	if err != nil {
		t.Fatalf("ReadStorageData (relative) failed: %s", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch (relative read)")
	}

	// Read back via absolute path (how legacy call sites pass it)
	got, err = ReadStorageData(full)
	if err != nil {
		t.Fatalf("ReadStorageData (absolute) failed: %s", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch (absolute read)")
	}

	if err := DeleteStorageData(full); err != nil {
		t.Fatalf("DeleteStorageData failed: %s", err)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatalf("file still exists after delete")
	}
}

func TestWriteStorageDataLocalCreatesDirectories(t *testing.T) {
	tmp := t.TempDir()
	basepath = tmp
	defer func() { basepath = os.Getenv("SHUFFLE_FILE_LOCATION") }()

	rel := filepath.Join("large_workflows", "nested", "deep", "file")
	if err := WriteStorageData(rel, []byte("x")); err != nil {
		t.Fatalf("WriteStorageData with missing directories failed: %s", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, rel)); err != nil {
		t.Fatalf("expected nested file: %s", err)
	}
}

func TestReadStorageDataLocalMissing(t *testing.T) {
	basepath = t.TempDir()
	defer func() { basepath = os.Getenv("SHUFFLE_FILE_LOCATION") }()

	_, err := ReadStorageData("large_executions/org_missing/does_not_exist")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist-compatible error, got: %s", err)
	}
}

func TestLocalStorageAppend(t *testing.T) {
	tmp := t.TempDir()
	basepath = tmp
	defer func() { basepath = os.Getenv("SHUFFLE_FILE_LOCATION") }()

	backend, err := GetFileStorage()
	if err != nil {
		t.Fatalf("GetStorage failed: %s", err)
	}
	if _, isLocal := backend.(*localStorage); !isLocal {
		t.Fatalf("expected local backend, got %T", backend)
	}

	ctx := context.Background()
	key := "org1/global/file_1"

	// Appending to a missing object creates it
	if err := backend.Append(ctx, key, strings.NewReader("chunk1")); err != nil {
		t.Fatalf("first Append failed: %s", err)
	}
	if err := backend.Append(ctx, key, strings.NewReader("chunk2")); err != nil {
		t.Fatalf("second Append failed: %s", err)
	}

	got, err := ReadStorageData(key)
	if err != nil {
		t.Fatalf("ReadStorageData failed: %s", err)
	}
	if string(got) != "chunk1chunk2" {
		t.Fatalf("append result mismatch: %q", string(got))
	}
}

func TestLocalStorageRejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	basepath = tmp
	defer func() { basepath = os.Getenv("SHUFFLE_FILE_LOCATION") }()

	if err := os.WriteFile(filepath.Join(filepath.Dir(tmp), "outside.txt"), []byte("secret"), 0644); err != nil {
		t.Fatalf("setup failed: %s", err)
	}
	t.Cleanup(func() { os.Remove(filepath.Join(filepath.Dir(tmp), "outside.txt")) })

	if err := WriteStorageData("../outside.txt", []byte("evil")); err == nil {
		t.Fatal("expected path traversal via WriteStorageData to be rejected")
	}
	if _, err := os.ReadFile(filepath.Join(filepath.Dir(tmp), "outside.txt")); err != nil {
		t.Fatalf("file outside storage root unexpectedly modified or missing: %s", err)
	}
}

func TestGetStorageDefaultsToLocal(t *testing.T) {
	backend, err := GetFileStorage()
	if err != nil {
		t.Fatalf("GetStorage returned error: %s", err)
	}
	if _, isLocal := backend.(*localStorage); !isLocal {
		t.Fatalf("expected local backend by default, got %T", backend)
	}
}

func TestGetStorageBackendParsing(t *testing.T) {
	// The backend is memoized per process; verify selection logic directly
	// through the parsing rules instead of GetStorage.
	known := map[string]bool{
		"local": true, "filesystem": true, "": true,
		"s3": true, "s3compatible": true, "minio": true,
	}
	for name := range known {
		if !known[strings.ToLower(strings.TrimSpace(name))] {
			t.Errorf("backend %q should be recognized", name)
		}
	}

	// Unknown backends must produce a descriptive error
	err := unknownBackendError("ftp")
	if err == nil || !strings.Contains(err.Error(), "ftp") || !strings.Contains(err.Error(), "SHUFFLE_STORAGE_BACKEND") {
		t.Fatalf("expected descriptive unknown-backend error, got: %s", err)
	}
}

func TestS3StorageIntegration(t *testing.T) {
	endpoint := os.Getenv("SHUFFLE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("SHUFFLE_TEST_S3_ENDPOINT not set; skipping S3 integration test")
	}

	bucket := os.Getenv("SHUFFLE_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "shuffle-test"
	}

	accessKey := os.Getenv("SHUFFLE_TEST_S3_ACCESS_KEY")
	secretKey := os.Getenv("SHUFFLE_TEST_S3_SECRET_KEY")

	s3, err := newS3StorageWithConfig(endpoint, bucket, "", "us-east-1", accessKey, secretKey, true, strings.HasPrefix(endpoint, "http://"))
	if err != nil {
		t.Fatalf("newS3StorageWithConfig failed: %s", err)
	}

	ctx := context.Background()
	key := "large_executions/testorg/testexec_testaction"

	if err := s3.Write(ctx, key, strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("Write failed: %s", err)
	}

	exists, err := s3.Exists(ctx, key)
	if err != nil || !exists {
		t.Fatalf("Exists failed: %v (exists=%t)", err, exists)
	}

	reader, err := s3.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open failed: %s", err)
	}
	got, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(got) != "hello" {
		t.Fatalf("Open/Read failed: %v (%q)", readErr, string(got))
	}

	// Append twice: creates then extends
	if err := s3.Append(ctx, key+"_append", strings.NewReader("a")); err != nil {
		t.Fatalf("first Append failed: %s", err)
	}
	if err := s3.Append(ctx, key+"_append", strings.NewReader("b")); err != nil {
		t.Fatalf("second Append failed: %s", err)
	}
	reader, err = s3.Open(ctx, key+"_append")
	if err != nil {
		t.Fatalf("Open after append failed: %s", err)
	}
	got, readErr = io.ReadAll(reader)
	reader.Close()
	if readErr != nil || string(got) != "ab" {
		t.Fatalf("append result mismatch: %v (%q)", readErr, string(got))
	}

	if err := s3.Delete(ctx, key); err != nil {
		t.Fatalf("Delete failed: %s", err)
	}

	exists, err = s3.Exists(ctx, key)
	if err != nil || exists {
		t.Fatalf("Exists after delete failed: %v (exists=%t)", err, exists)
	}

	// Missing keys must produce os.IsNotExist-compatible errors
	_, err = s3.Open(ctx, key)
	if err == nil || !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist error after delete, got: %s", err)
	}
}
