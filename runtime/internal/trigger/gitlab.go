package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxVersionProbes 限制逐版本探测的上限(正常情况 bundle 在最新 1~2 个版本里)。
const maxVersionProbes = 5

// GitLabClient 通过 Packages API 定位并下载 bundle-g{sha}-p{pipelineGlobalID}.json。
// webhook payload 不携带 Registry 版本号(X.Y.Z 来自 CMakeLists),
// 因此按 package_name 倒序列出版本,逐个探测 bundle 文件(404 → 下一个)。
type GitLabClient struct {
	BaseURL     string // 如 https://gitlab.example
	Token       string // read_api 权限的访问令牌
	TokenHeader string // 缺省 PRIVATE-TOKEN;Deploy Token 用 Deploy-Token
	PackageName string // RELEASE_PACKAGE_NAME,如 algo-super-sdk
	HTTP        *http.Client
}

func (g *GitLabClient) http() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return http.DefaultClient
}

func (g *GitLabClient) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	header := g.TokenHeader
	if header == "" {
		header = "PRIVATE-TOKEN"
	}
	req.Header.Set(header, g.Token)
	return g.http().Do(req)
}

// FetchBundle 实现 BundleFetcher。found=false 表示该 pipeline 无 bundle(不是错误)。
func (g *GitLabClient) FetchBundle(ctx context.Context, projectID int64, shortSHA string, pipelineGlobalID int64) ([]byte, bool, error) {
	versions, err := g.listVersions(ctx, projectID)
	if err != nil {
		return nil, false, fmt.Errorf("list packages: %w", err)
	}
	fileName := fmt.Sprintf("bundle-g%s-p%d.json", shortSHA, pipelineGlobalID)
	for _, v := range versions {
		u := fmt.Sprintf("%s/api/v4/projects/%d/packages/generic/%s/%s/%s",
			g.BaseURL, projectID, url.PathEscape(g.PackageName), url.PathEscape(v), fileName)
		resp, err := g.get(ctx, u)
		if err != nil {
			return nil, false, fmt.Errorf("download %s: %w", fileName, err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if err != nil {
				return nil, false, fmt.Errorf("read bundle: %w", err)
			}
			return raw, true, nil
		case http.StatusNotFound:
			resp.Body.Close()
			continue
		default:
			resp.Body.Close()
			return nil, false, fmt.Errorf("download %s: unexpected status %d", fileName, resp.StatusCode)
		}
	}
	return nil, false, nil
}

// listVersions 返回该包名下按创建时间倒序去重的版本列表(最多 maxVersionProbes 个)。
func (g *GitLabClient) listVersions(ctx context.Context, projectID int64) ([]string, error) {
	u := fmt.Sprintf("%s/api/v4/projects/%d/packages?package_name=%s&order_by=created_at&sort=desc&per_page=%d",
		g.BaseURL, projectID, url.QueryEscape(g.PackageName), 20)
	resp, err := g.get(ctx, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	var pkgs []struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pkgs); err != nil {
		return nil, fmt.Errorf("decode packages: %w", err)
	}
	seen := map[string]bool{}
	versions := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		if p.Version == "" || seen[p.Version] {
			continue
		}
		seen[p.Version] = true
		versions = append(versions, p.Version)
		if len(versions) == maxVersionProbes {
			break
		}
	}
	return versions, nil
}
