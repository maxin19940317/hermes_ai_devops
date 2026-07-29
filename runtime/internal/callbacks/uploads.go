package callbacks

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"

	"hermes-devops/runtime/internal/store"
)

// defaultUploadMaxFiles 是单次请求的文件数上限缺省值(差距 #8)。
const defaultUploadMaxFiles = 64

type uploadRequestReq struct {
	TaskID          string   `json:"task_id"`
	ClientID        string   `json:"client_id"`
	DeviceID        string   `json:"device_id"`
	Attempt         int      `json:"attempt"`
	LeaseID         string   `json:"lease_id"`
	LeaseGeneration int      `json:"lease_generation"`
	Files           []string `json:"files"`
}

type uploadItem struct {
	Path      string `json:"path"`
	ObjectKey string `json:"object_key"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at"`
}

type rejectedItem struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// uploadRequests 按需签发附件上传 URL(差距 #8)。
//
// 与本包其他端点的性质不同:它签发的是**写入凭据**而非接收数据,因此必须先校验
// 租约所有权(差距 #15 的凭据)。callbacks 整体今天无鉴权(mTLS 属 Phase 3),
// 若不校验,同网段任何人都能拿猜到的 task_id 换取往证据桶写入的能力。
func (h *Handler) uploadRequests(w http.ResponseWriter, r *http.Request) {
	var req uploadRequestReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.TaskID == "" || req.ClientID == "" || req.DeviceID == "" ||
		req.LeaseID == "" || req.Attempt < 1 || req.LeaseGeneration < 1 ||
		len(req.Files) == 0 {
		writeErr(w, http.StatusBadRequest, "bad_upload_request", "invalid upload request payload")
		return
	}
	max := h.UploadMaxFiles
	if max <= 0 {
		max = defaultUploadMaxFiles
	}
	// 超限整体拒绝而非截断:截断会让 Agent 以为传全了。
	if len(req.Files) > max {
		writeErr(w, http.StatusBadRequest, "too_many_files", "files exceeds limit")
		return
	}
	ok, err := h.store.VerifyLease(r.Context(), store.LeaseCredential{
		DeviceID: req.DeviceID, ClientID: req.ClientID, TaskID: req.TaskID,
		Attempt: req.Attempt, LeaseID: req.LeaseID, Generation: req.LeaseGeneration,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !ok {
		// 不泄露任何 URL,也不区分"租约易主"与"任务不存在"——两者对调用方是同一结论。
		h.log.Info().Str("task", req.TaskID).Str("client", req.ClientID).
			Msg("upload request rejected: lease not owned")
		writeErr(w, http.StatusUnauthorized, "lease_not_owned", "lease credential mismatch")
		return
	}
	if h.Presign == nil {
		writeErr(w, http.StatusServiceUnavailable, "presign_disabled", "minio not configured")
		return
	}

	prefix := "runs/" + req.TaskID + "/"
	uploads := []uploadItem{}
	rejected := []rejectedItem{}
	for _, p := range req.Files {
		key, reason := confineKey(prefix, p)
		if reason != "" {
			rejected = append(rejected, rejectedItem{Path: p, Reason: reason})
			continue
		}
		u, exp, err := h.Presign.PutURL(r.Context(), key)
		if err != nil {
			// URL 含签名,永不落日志;只记 object key。
			h.log.Error().Err(err).Str("object_key", key).Msg("presign put failed")
			rejected = append(rejected, rejectedItem{Path: p, Reason: "presign failed"})
			continue
		}
		uploads = append(uploads, uploadItem{
			Path: p, ObjectKey: key, URL: u, ExpiresAt: exp.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"uploads": uploads, "rejected": rejected})
}

// confineKey 把 out_dir 相对路径拼成 object key,并确认结果不越出 prefix。
// 返回的 reason 非空即表示拒绝。
func confineKey(prefix, rel string) (key, reason string) {
	if rel == "" {
		return "", "empty path"
	}
	if strings.HasPrefix(rel, "/") || strings.Contains(rel, "\\") {
		return "", "absolute or non-slash path"
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", "path escapes task prefix"
		}
	}
	// path.Clean 之后再验前缀:防御归一化本身被绕过(如 a/../../b)。
	key = path.Clean(prefix + rel)
	if !strings.HasPrefix(key, prefix) {
		return "", "path escapes task prefix"
	}
	return key, ""
}
