package activity

import (
	"context"
	"fmt"
	"time"

	"github.com/minio/minio-go/v7"
)

// MinioLifecycleResult is the outcome of one MinIO lifecycle sweep.
type MinioLifecycleResult struct {
	TasksScanned   int `json:"tasks_scanned"`
	ObjectsDeleted int `json:"objects_deleted"`
	Errors         int `json:"errors"`
}

// MinioLifecycle scans and removes expired task attachments from MinIO.
// Called daily by EvidenceLifecycleWorkflow (Temporal Schedule).
// PASSED → 7 days; failed → 90 days.
func (a *Acts) EvidenceLifecycle(ctx context.Context) (MinioLifecycleResult, error) {
	res := MinioLifecycleResult{}
	if !a.Cfg.presignEnabled() {
		return res, nil
	}
	cli, err := evidenceClient(a.Cfg)
	if err != nil {
		return res, fmt.Errorf("minio lifecycle: client init: %w", err)
	}

	const (
		maxAgePassed = 7 * 24 * time.Hour
		maxAgeFailed = 90 * 24 * time.Hour
	)

	expired, err := a.Store.ListExpiredTaskIDs(ctx, maxAgePassed, maxAgeFailed)
	if err != nil {
		return res, fmt.Errorf("minio lifecycle: list expired: %w", err)
	}
	res.TasksScanned = len(expired)

	bucket := a.Cfg.MinIOBucket
	for _, t := range expired {
		prefixes := []string{
			"runs/" + t.TaskID + "/",
			"evidence/" + t.TaskID + "/",
		}
		for _, prefix := range prefixes {
			objectsCh := cli.ListObjects(ctx, bucket, minio.ListObjectsOptions{
				Prefix:    prefix,
				Recursive: true,
			})
			for obj := range objectsCh {
				if obj.Err != nil {
					a.warnf("minio lifecycle: list %s: %v", obj.Key, obj.Err)
					res.Errors++
					continue
				}
				if err := cli.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
					a.warnf("minio lifecycle: remove %s: %v", obj.Key, err)
					res.Errors++
				} else {
					res.ObjectsDeleted++
				}
			}
		}
	}

	return res, nil
}
