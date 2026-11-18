package baidu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	apiBase   = "https://pan.baidu.com/rest/2.0/xpan"
	oauthBase = "https://openapi.baidu.com/oauth/2.0"
)

type Token struct {
	AccessToken  string    `json:"access_token" yaml:"access_token"`
	RefreshToken string    `json:"refresh_token" yaml:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at" yaml:"expires_at"`
}

type File struct {
	FSID  int64  `json:"fs_id"`
	Path  string `json:"path"`
	Name  string `json:"server_filename"`
	Size  int64  `json:"size"`
	IsDir int    `json:"isdir"`
	MD5   string `json:"md5"`
	DLink string `json:"dlink"`
}

type Client struct {
	clientID, clientSecret string
	httpClient             *http.Client
	mu                     sync.Mutex
	token                  Token
	onToken                func(Token) error
}

func NewClient(clientID, clientSecret string, token Token, onToken func(Token) error) *Client {
	return &Client{
		clientID: clientID, clientSecret: clientSecret, token: token, onToken: onToken,
		httpClient: &http.Client{Timeout: 0},
	}
}

func AuthorizationURL(clientID, redirectURI string) string {
	v := url.Values{"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI}, "scope": {"basic,netdisk"}, "display": {"page"}}
	return oauthBase + "/authorize?" + v.Encode()
}

func ExchangeCode(ctx context.Context, hc *http.Client, clientID, secret, redirectURI, code string) (*Token, error) {
	v := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "client_secret": {secret}, "redirect_uri": {redirectURI}}
	return requestToken(ctx, hc, v)
}

func requestToken(ctx context.Context, hc *http.Client, values url.Values) (*Token, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oauthBase+"/token?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 OAuth token: %w", err)
	}
	defer resp.Body.Close()
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("OAuth 返回无效 JSON: %w", err)
	}
	if raw.Error != "" || raw.AccessToken == "" {
		return nil, fmt.Errorf("OAuth 失败: %s %s", raw.Error, raw.ErrorDescription)
	}
	return &Token{AccessToken: raw.AccessToken, RefreshToken: raw.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)}, nil
}

func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token.AccessToken != "" && (c.token.ExpiresAt.IsZero() || time.Until(c.token.ExpiresAt) > 5*time.Minute) {
		return c.token.AccessToken, nil
	}
	if c.token.RefreshToken == "" {
		return "", errors.New("access token 已过期且没有 refresh token，请重新运行 pan auth")
	}
	v := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c.token.RefreshToken}, "client_id": {c.clientID}, "client_secret": {c.clientSecret}}
	tok, err := requestToken(ctx, c.httpClient, v)
	if err != nil {
		return "", fmt.Errorf("刷新 token: %w", err)
	}
	c.token = *tok
	if c.onToken != nil {
		if err := c.onToken(*tok); err != nil {
			return "", fmt.Errorf("保存刷新后的 token: %w", err)
		}
	}
	return tok.AccessToken, nil
}

type apiResponse struct {
	Errno     int    `json:"errno"`
	RequestID int64  `json:"request_id"`
	List      []File `json:"list"`
	ErrorMsg  string `json:"errmsg"`
	HasMore   int    `json:"has_more"`
}

func (c *Client) List(ctx context.Context, dir string, page int) ([]File, bool, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, false, err
	}
	v := url.Values{
		"method": {"list"}, "access_token": {token}, "dir": {dir}, "page": {strconv.Itoa(page)},
		"num": {"1000"}, "order": {"name"}, "desc": {"0"}, "web": {"0"},
	}
	var out apiResponse
	if err := c.getJSON(ctx, apiBase+"/file?"+v.Encode(), &out); err != nil {
		return nil, false, err
	}
	if out.Errno != 0 {
		return nil, false, apiError("列目录 "+dir, out.Errno, out.ErrorMsg, out.RequestID)
	}
	return out.List, out.HasMore == 1 || len(out.List) == 1000, nil
}

func (c *Client) Meta(ctx context.Context, fsid int64) (File, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return File{}, err
	}
	ids, _ := json.Marshal([]int64{fsid})
	v := url.Values{"method": {"filemetas"}, "access_token": {token}, "fsids": {string(ids)}, "dlink": {"1"}}
	var out apiResponse
	if err := c.getJSON(ctx, apiBase+"/multimedia?"+v.Encode(), &out); err != nil {
		return File{}, err
	}
	if out.Errno != 0 {
		return File{}, apiError("获取下载地址", out.Errno, out.ErrorMsg, out.RequestID)
	}
	if len(out.List) != 1 || out.List[0].DLink == "" {
		return File{}, errors.New("接口未返回下载地址（请确认应用具有网盘下载权限）")
	}
	return out.List[0], nil
}

func (c *Client) DownloadRequest(ctx context.Context, dlink string, offset int64) (*http.Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(dlink)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("access_token", token)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	return c.httpClient.Do(req)
}

func (c *Client) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pan.baidu.com")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

func apiError(action string, errno int, msg string, requestID int64) error {
	if msg == "" {
		msg = map[int]string{-6: "身份验证失败或 token 已过期", -7: "文件或目录无权访问", -9: "文件或目录不存在", 31034: "命中接口限流"}[errno]
	}
	return fmt.Errorf("%s失败: errno=%d %s (request_id=%d)", action, errno, strings.TrimSpace(msg), requestID)
}
