// Package artifact 负责产物下载、整包 sha256 校验与安全解压。
// 红线:解压严格防路径穿越;下载必须校验整包 sha256 后才可消费。
package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Auth 描述 Registry 下载凭据(契约 client-agent-api §TaskDispatchRequest.artifact.auth)。
type Auth struct {
	Type  string // bearer | job_token
	Token string
}

func (a *Auth) apply(req *http.Request) error {
	if a == nil {
		return nil
	}
	switch a.Type {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+a.Token)
	case "job_token":
		req.Header.Set("JOB-TOKEN", a.Token)
	default:
		return fmt.Errorf("unknown auth type %q", a.Type)
	}
	return nil
}

// Download 下载 url 到 dest(原子写:先临时文件后 rename,失败不留残档)。
func Download(ctx context.Context, client *http.Client, url string, auth *Auth, dest string) error {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if err := auth.apply(req); err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write body: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// SHA256File 计算文件 sha256(hex)。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifySHA256 校验文件与期望值(不区分大小写)。
func VerifySHA256(path, want string) error {
	got, err := SHA256File(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("sha256 mismatch for %s: got %s, want %s", path, got, want)
	}
	return nil
}

// ExtractTarGz 解压 src 到 destDir,返回解出的常规文件相对路径。
// 拒绝绝对路径、包含 .. 的成员与符号链接。
func ExtractTarGz(src, destDir string) ([]string, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	var files []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		name := filepath.ToSlash(hdr.Name)
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			return nil, fmt.Errorf("unsafe path in package: %q", hdr.Name)
		}
		target := filepath.Join(destDir, filepath.FromSlash(name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return nil, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return nil, err
			}
			mode := os.FileMode(hdr.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return nil, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return nil, fmt.Errorf("extract %s: %w", name, err)
			}
			if err := out.Close(); err != nil {
				return nil, err
			}
			files = append(files, name)
		default:
			return nil, fmt.Errorf("unsupported tar entry type %c for %q (symlinks rejected)", hdr.Typeflag, hdr.Name)
		}
	}
}
