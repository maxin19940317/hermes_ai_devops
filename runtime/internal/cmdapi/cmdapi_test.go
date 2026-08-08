package cmdapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hermes-devops/runtime/internal/feishucmd"
)

// fakeExec 记录收到的指令,返回固定回复(测试用)。
type fakeExec struct {
	got   feishucmd.Command
	reply string
	err   error
}

func (f *fakeExec) ExecuteCommand(_ context.Context, cmd feishucmd.Command) (string, error) {
	f.got = cmd
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

func doPost(h http.Handler, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/cmd", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCmdAPIHappyPath(t *testing.T) {
	fe := &fakeExec{reply: "设备列表..."}
	h := &Handler{Token: "secret-token", Exec: fe}
	rec := doPost(h, "secret-token", `{"command":"devices","args":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["reply"] != "设备列表..." {
		t.Errorf("reply = %q, want 设备列表...", resp["reply"])
	}
	if fe.got.Name != "devices" || len(fe.got.Args) != 0 {
		t.Errorf("got cmd = %+v, want devices/[]", fe.got)
	}
}

func TestCmdAPITestArgs(t *testing.T) {
	fe := &fakeExec{reply: "ok"}
	h := &Handler{Token: "t", Exec: fe}
	rec := doPost(h, "t", `{"command":"test","args":["aarch64_Android_SNPE_1.68"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if fe.got.Name != "test" || len(fe.got.Args) != 1 || fe.got.Args[0] != "aarch64_Android_SNPE_1.68" {
		t.Errorf("got cmd = %+v, want test/[aarch64_Android_SNPE_1.68]", fe.got)
	}
}

func TestCmdAPIRejectsBadToken(t *testing.T) {
	h := &Handler{Token: "secret", Exec: &fakeExec{}}
	rec := doPost(h, "wrong", `{"command":"devices"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCmdAPIRejectsMissingTokenConfig(t *testing.T) {
	h := &Handler{Token: "", Exec: &fakeExec{}}
	rec := doPost(h, "", `{"command":"devices"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestCmdAPIRejectsUnknownCommand(t *testing.T) {
	h := &Handler{Token: "t", Exec: &fakeExec{}}
	rec := doPost(h, "t", `{"command":"drop database","args":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCmdAPIRejectsHelp(t *testing.T) {
	// 受控接口不接受 help/none(LLM 不该发未知指令)。
	h := &Handler{Token: "t", Exec: &fakeExec{}}
	rec := doPost(h, "t", `{"command":"help"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCmdAPIRejectsNonPost(t *testing.T) {
	h := &Handler{Token: "t", Exec: &fakeExec{}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cmd", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestCmdAPIRejectsBadJSON(t *testing.T) {
	h := &Handler{Token: "t", Exec: &fakeExec{}}
	rec := doPost(h, "t", `{invalid`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCmdAPIRejectsEmptyCommand(t *testing.T) {
	h := &Handler{Token: "t", Exec: &fakeExec{}}
	rec := doPost(h, "t", `{"command":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
