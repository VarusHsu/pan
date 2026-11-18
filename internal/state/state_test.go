package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"example.com/pan/internal/baidu"
)

func TestStorePersistsProgress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(File{FSID: 7, RemotePath: "/a", LocalPath: "/tmp/a", Size: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(7, 40, "downloading", ""); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := reopened.Summary()
	if sum.Total != 1 || sum.Downloaded != 40 || sum.Pending != 1 {
		t.Fatalf("unexpected summary: %+v", sum)
	}
}

func TestConfigYAMLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conf.yaml")
	want := Config{
		App:      AppConfig{AppID: "1", AppKey: "key", SecretKey: "secret", SignKey: "sign"},
		OAuth:    OAuthConfig{RedirectURI: "http://localhost/callback"},
		Download: DownloadConfig{RemotePaths: []string{"/", "/docs"}, Output: "./downloads", Workers: 3, Retries: 5, VerifyMD5: true},
		Token:    baidu.Token{AccessToken: "access", RefreshToken: "refresh", ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)},
	}
	if err := SaveConfig(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.App != want.App || got.OAuth != want.OAuth || got.Token.AccessToken != want.Token.AccessToken || len(got.Download.RemotePaths) != 2 {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}
