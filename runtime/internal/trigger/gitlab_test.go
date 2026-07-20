package trigger

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGitLab 模拟 13.8 的 Packages API:列包 + generic 下载。
func fakeGitLab(t *testing.T, versions []string, files map[string][]byte) (*httptest.Server, *[]string) {
	t.Helper()
	var seenAuth []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v4/projects/7/packages", func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("PRIVATE-TOKEN"))
		if r.URL.Query().Get("package_name") == "" {
			t.Error("必须按 package_name 过滤")
		}
		type pkg struct {
			Version string `json:"version"`
		}
		out := make([]pkg, 0, len(versions))
		for _, v := range versions {
			out = append(out, pkg{Version: v})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/v4/projects/7/packages/generic/", func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("PRIVATE-TOKEN"))
		if body, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(body)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seenAuth
}

func TestFetchBundleFindsAcrossVersions(t *testing.T) {
	bundle := []byte(`{"fake":"bundle"}`)
	// 最新版本 2.0.0 没有该 sha 的 bundle,1.2.3 有 → 逐版本探测
	srv, seenAuth := fakeGitLab(t, []string{"2.0.0", "1.2.3", "2.0.0"}, map[string][]byte{
		"/api/v4/projects/7/packages/generic/algo-super-sdk/1.2.3/bundle-gabcd1234-p42001.json": bundle,
	})
	gl := &GitLabClient{BaseURL: srv.URL, Token: "tok", PackageName: "algo-super-sdk", HTTP: srv.Client()}

	raw, found, err := gl.FetchBundle(context.Background(), 7, "abcd1234", 42001)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if string(raw) != string(bundle) {
		t.Errorf("raw = %s", raw)
	}
	for _, a := range *seenAuth {
		if a != "tok" {
			t.Errorf("请求缺 PRIVATE-TOKEN: %q", a)
		}
	}
}

func TestFetchBundleNotFound(t *testing.T) {
	srv, _ := fakeGitLab(t, []string{"1.2.3"}, nil)
	gl := &GitLabClient{BaseURL: srv.URL, Token: "tok", PackageName: "algo-super-sdk", HTTP: srv.Client()}
	_, found, err := gl.FetchBundle(context.Background(), 7, "abcd1234", 42001)
	if err != nil {
		t.Fatalf("全 404 不是错误: %v", err)
	}
	if found {
		t.Error("found = true, want false")
	}
}

func TestFetchBundleNoVersions(t *testing.T) {
	srv, _ := fakeGitLab(t, nil, nil)
	gl := &GitLabClient{BaseURL: srv.URL, Token: "tok", PackageName: "algo-super-sdk", HTTP: srv.Client()}
	_, found, err := gl.FetchBundle(context.Background(), 7, "abcd1234", 42001)
	if err != nil || found {
		t.Fatalf("found=%v err=%v, want false,nil", found, err)
	}
}

func TestFetchBundleServerErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	gl := &GitLabClient{BaseURL: srv.URL, Token: "tok", PackageName: "algo-super-sdk", HTTP: srv.Client()}
	if _, _, err := gl.FetchBundle(context.Background(), 7, "abcd1234", 42001); err == nil {
		t.Error("500 应报错")
	}
}
