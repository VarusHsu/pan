package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"example.com/pan/internal/baidu"
	"gopkg.in/yaml.v3"
)

type Config struct {
	App      AppConfig      `yaml:"app"`
	OAuth    OAuthConfig    `yaml:"oauth"`
	Download DownloadConfig `yaml:"download"`
	Token    baidu.Token    `yaml:"token"`
}

type AppConfig struct {
	AppID     string `yaml:"app_id"`
	AppKey    string `yaml:"app_key"`
	SecretKey string `yaml:"secret_key"`
	SignKey   string `yaml:"sign_key"`
}

type OAuthConfig struct {
	RedirectURI string `yaml:"redirect_uri"`
}

type DownloadConfig struct {
	RemotePaths []string `yaml:"remote_paths"`
	Output      string   `yaml:"output"`
	Workers     int      `yaml:"workers"`
	Retries     int      `yaml:"retries"`
	VerifyMD5   bool     `yaml:"verify_md5"`
}

type File struct {
	FSID       int64     `json:"fs_id"`
	RemotePath string    `json:"remote_path"`
	LocalPath  string    `json:"local_path"`
	Size       int64     `json:"size"`
	MD5        string    `json:"md5,omitempty"`
	Downloaded int64     `json:"downloaded"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type data struct {
	Version int              `json:"version"`
	Files   map[string]*File `json:"files"`
}

type Store struct {
	mu   sync.Mutex
	path string
	data data
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.App.AppKey == "" || cfg.App.SecretKey == "" {
		return cfg, errors.New("配置文件缺少 app.app_key 或 app.secret_key")
	}
	return cfg, nil
}

func SaveConfig(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, data: data{Version: 1, Files: map[string]*File{}}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, fmt.Errorf("状态文件损坏 %s: %w", path, err)
	}
	if s.data.Files == nil {
		s.data.Files = map[string]*File{}
	}
	return s, nil
}

func (s *Store) Upsert(f File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fmt.Sprintf("%d", f.FSID)
	if old := s.data.Files[key]; old != nil {
		f.Downloaded, f.Status, f.Error, f.UpdatedAt = old.Downloaded, old.Status, old.Error, old.UpdatedAt
		if f.Status == "complete" && f.Size != old.Size {
			f.Status, f.Downloaded = "pending", 0
		}
	} else {
		f.Status = "pending"
		f.UpdatedAt = time.Now()
	}
	s.data.Files[key] = &f
	return s.saveLocked()
}

func (s *Store) Update(fsid int64, downloaded int64, status, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.data.Files[fmt.Sprintf("%d", fsid)]
	if f == nil {
		return fmt.Errorf("未知文件 ID %d", fsid)
	}
	f.Downloaded, f.Status, f.Error, f.UpdatedAt = downloaded, status, errMsg, time.Now()
	return s.saveLocked()
}

func (s *Store) Pending() []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]File, 0, len(s.data.Files))
	for _, f := range s.data.Files {
		if f.Status != "complete" {
			result = append(result, *f)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RemotePath < result[j].RemotePath })
	return result
}

func (s *Store) Failed() []File {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []File
	for _, f := range s.data.Files {
		if f.Status == "failed" {
			result = append(result, *f)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RemotePath < result[j].RemotePath })
	return result
}

type Summary struct {
	Total, Completed, Pending, Failed int
	Bytes, Downloaded                 int64
}

func (s Summary) Percent() float64 {
	if s.Bytes == 0 {
		if s.Total > 0 && s.Completed == s.Total {
			return 100
		}
		return 0
	}
	return float64(s.Downloaded) * 100 / float64(s.Bytes)
}

func (s *Store) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Summary
	for _, f := range s.data.Files {
		out.Total++
		out.Bytes += f.Size
		d := f.Downloaded
		if d > f.Size {
			d = f.Size
		}
		out.Downloaded += d
		switch f.Status {
		case "complete":
			out.Completed++
		case "failed":
			out.Failed++
		default:
			out.Pending++
		}
	}
	return out
}

func (s *Store) saveLocked() error { return writeJSON(s.path, s.data, 0o600) }

func writeJSON(path string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
