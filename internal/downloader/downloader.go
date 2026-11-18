package downloader

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"example.com/pan/internal/baidu"
	"example.com/pan/internal/state"
)

type API interface {
	List(context.Context, string, int) ([]baidu.File, bool, error)
	Meta(context.Context, int64) (baidu.File, error)
	DownloadRequest(context.Context, string, int64) (*http.Response, error)
}

type Options struct {
	Output    string
	Workers   int
	Retries   int
	VerifyMD5 bool
}

type Downloader struct {
	api   API
	store *state.Store
	opts  Options
}

func New(api API, store *state.Store, opts Options) *Downloader {
	return &Downloader{api: api, store: store, opts: opts}
}

func (d *Downloader) Run(ctx context.Context, roots []string) error {
	fmt.Printf("扫描网盘路径: %s\n", strings.Join(roots, ", "))
	for _, root := range roots {
		if err := d.scan(ctx, root, root); err != nil {
			return err
		}
	}
	files := d.store.Pending()
	s := d.store.Summary()
	fmt.Printf("扫描完成：共 %d 个文件，待下载 %d，合计 %.1f%% 已完成\n", s.Total, len(files), s.Percent())
	if len(files) == 0 {
		return nil
	}

	jobs := make(chan state.File)
	var wg sync.WaitGroup
	var failMu sync.Mutex
	failed := 0
	for i := 0; i < d.opts.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range jobs {
				if err := d.downloadWithRetry(ctx, f); err != nil {
					failMu.Lock()
					failed++
					failMu.Unlock()
					fmt.Fprintf(os.Stderr, "失败  %s: %v\n", f.RemotePath, err)
				} else {
					fmt.Printf("完成  %s\n", f.RemotePath)
				}
			}
		}()
	}
sendLoop:
	for _, f := range files {
		select {
		case jobs <- f:
		case <-ctx.Done():
			break sendLoop
		}
	}
	close(jobs)
	wg.Wait()
	if ctx.Err() != nil {
		return fmt.Errorf("已停止，重新运行相同命令即可续传: %w", ctx.Err())
	}
	if failed > 0 {
		return fmt.Errorf("%d 个文件下载失败；重新运行命令可重试", failed)
	}
	return nil
}

func (d *Downloader) scan(ctx context.Context, path, root string) error {
	page := 1
	for {
		items, more, err := d.api.List(ctx, path, page)
		if err != nil {
			// A root can also be a single file. Resolve it by listing its parent.
			if page == 1 && path == root && path != "/" {
				return d.scanSingle(ctx, path, root, err)
			}
			return err
		}
		for _, item := range items {
			if item.IsDir == 1 {
				if err := d.scan(ctx, item.Path, root); err != nil {
					return err
				}
				continue
			}
			if err := d.add(item, root); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
		page++
	}
}

func (d *Downloader) scanSingle(ctx context.Context, target, root string, original error) error {
	parent := filepath.ToSlash(filepath.Dir(target))
	items, _, err := d.api.List(ctx, parent, 1)
	if err != nil {
		return original
	}
	for _, item := range items {
		if item.Path == target && item.IsDir == 0 {
			return d.add(item, root)
		}
	}
	return original
}

func (d *Downloader) add(f baidu.File, root string) error {
	rel := strings.TrimPrefix(f.Path, "/")
	local, err := safeJoin(d.opts.Output, rel)
	if err != nil {
		return err
	}
	return d.store.Upsert(state.File{FSID: f.FSID, RemotePath: f.Path, LocalPath: local, Size: f.Size, MD5: f.MD5})
}

func safeJoin(root, rel string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("不安全的网盘路径 %q", rel)
	}
	path := filepath.Join(root, clean)
	relCheck, err := filepath.Rel(root, path)
	if err != nil || relCheck == ".." || strings.HasPrefix(relCheck, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界 %q", rel)
	}
	return path, nil
}

func (d *Downloader) downloadWithRetry(ctx context.Context, f state.File) error {
	var last error
	for attempt := 0; attempt <= d.opts.Retries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<min(attempt-1, 6)) * time.Second
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		last = d.downloadOne(ctx, f)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_ = d.store.Update(f.FSID, partSize(f.LocalPath+".part"), "pending", last.Error())
	}
	_ = d.store.Update(f.FSID, partSize(f.LocalPath+".part"), "failed", last.Error())
	return last
}

func (d *Downloader) downloadOne(ctx context.Context, f state.File) error {
	if info, err := os.Stat(f.LocalPath); err == nil && info.Size() == f.Size {
		if d.opts.VerifyMD5 && f.MD5 != "" {
			if err := verifyMD5(f.LocalPath, f.MD5); err == nil {
				return d.store.Update(f.FSID, f.Size, "complete", "")
			} else if removeErr := os.Remove(f.LocalPath); removeErr != nil {
				return fmt.Errorf("本地文件校验失败且无法删除: %v（删除错误: %w）", err, removeErr)
			}
		} else {
			return d.store.Update(f.FSID, f.Size, "complete", "")
		}
	}
	if err := os.MkdirAll(filepath.Dir(f.LocalPath), 0o755); err != nil {
		return err
	}
	part := f.LocalPath + ".part"
	offset := partSize(part)
	if offset > f.Size {
		if err := os.Truncate(part, 0); err != nil {
			return err
		}
		offset = 0
	}
	meta, err := d.api.Meta(ctx, f.FSID)
	if err != nil {
		return err
	}
	resp, err := d.api.DownloadRequest(ctx, meta.DLink, offset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset == f.Size {
		return d.finish(f, part)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("下载 HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	flags := os.O_CREATE | os.O_WRONLY
	if offset > 0 && resp.StatusCode == http.StatusPartialContent {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
		offset = 0
	}
	out, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	reporter := &progressWriter{w: out, fsid: f.FSID, base: offset, store: d.store, last: time.Now()}
	_, copyErr := io.Copy(reporter, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if size := partSize(part); size != f.Size {
		return fmt.Errorf("文件大小不符: 得到 %d，期望 %d", size, f.Size)
	}
	return d.finish(f, part)
}

func (d *Downloader) finish(f state.File, part string) error {
	if d.opts.VerifyMD5 && f.MD5 != "" {
		if err := verifyMD5(part, f.MD5); err != nil {
			return err
		}
	}
	if err := os.Rename(part, f.LocalPath); err != nil {
		return err
	}
	return d.store.Update(f.FSID, f.Size, "complete", "")
}

type progressWriter struct {
	w                   io.Writer
	fsid, base, written int64
	store               *state.Store
	last                time.Time
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.written += int64(n)
	if time.Since(p.last) >= time.Second {
		_ = p.store.Update(p.fsid, p.base+p.written, "downloading", "")
		p.last = time.Now()
	}
	return n, err
}

func partSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func verifyMD5(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("MD5 不符: 得到 %s，期望 %s", got, want)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
