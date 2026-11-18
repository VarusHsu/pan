package downloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"example.com/pan/internal/baidu"
	"example.com/pan/internal/state"
)

type fakeAPI struct {
	content []byte
	offset  int64
}

func (f *fakeAPI) List(_ context.Context, dir string, page int) ([]baidu.File, bool, error) {
	if dir != "/" || page != 1 {
		return nil, false, nil
	}
	return []baidu.File{{FSID: 1, Path: "/dir", Name: "dir", IsDir: 1}, {FSID: 2, Path: "/zero", Name: "zero", Size: 0}}, false, nil
}

func (f *fakeAPI) Meta(_ context.Context, fsid int64) (baidu.File, error) {
	return baidu.File{FSID: fsid, DLink: "https://example.invalid/file"}, nil
}

func (f *fakeAPI) DownloadRequest(_ context.Context, _ string, offset int64) (*http.Response, error) {
	f.offset = offset
	status := http.StatusOK
	if offset > 0 {
		status = http.StatusPartialContent
	}
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d", status), Body: io.NopCloser(bytes.NewReader(f.content[offset:]))}, nil
}

func TestResumeFromPartFile(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, ".state.json"))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{content: []byte("hello resumable world")}
	local := filepath.Join(dir, "file.txt")
	f := state.File{FSID: 42, RemotePath: "/file.txt", LocalPath: local, Size: int64(len(api.content))}
	if err := store.Upsert(f); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local+".part", api.content[:6], 0o644); err != nil {
		t.Fatal(err)
	}
	d := New(api, store, Options{Output: dir, Workers: 1, Retries: 0})
	if err := d.downloadOne(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if api.offset != 6 {
		t.Fatalf("Range offset = %d, want 6", api.offset)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, api.content) {
		t.Fatalf("content = %q, want %q", got, api.content)
	}
	if store.Summary().Completed != 1 {
		t.Fatal("file was not marked complete")
	}
}

func TestSafeJoinRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	bad := []string{"../secret", "a/../../secret", "/absolute"}
	for _, rel := range bad {
		if _, err := safeJoin(root, rel); err == nil {
			t.Errorf("safeJoin(%q) accepted traversal", rel)
		}
	}
	got, err := safeJoin(root, "folder/file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "folder", "file.txt") {
		t.Fatalf("got %q", got)
	}
}

func TestCorruptCompletedFileIsRedownloaded(t *testing.T) {
	dir := t.TempDir()
	store, err := state.Open(filepath.Join(dir, ".state.json"))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{content: []byte("correct")}
	local := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(local, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := state.File{FSID: 43, RemotePath: "/file.txt", LocalPath: local, Size: 7, MD5: "e5d7cffe25654f7e3a1e334118c71549"}
	if err := store.Upsert(f); err != nil {
		t.Fatal(err)
	}
	d := New(api, store, Options{Output: dir, Workers: 1, VerifyMD5: true})
	if err := d.downloadOne(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, api.content) {
		t.Fatalf("content = %q, want %q", got, api.content)
	}
}
