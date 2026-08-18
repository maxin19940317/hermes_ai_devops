#!/usr/bin/env python3
"""task-dashboard — 任务执行实时面板(2026-08-18)。

读取 Runtime Postgres(task_events/tasks/devices),实时展示每个任务的
流水线阶段(QUEUED→ACCEPTED→DOWNLOADING→PREPARING→DEPLOYING→RUNNING→
COLLECTING→COMPLETED)与设备信息。纯只读,不进执行关键路径。

端点:
  GET /              HTML 页面(前端每 2s 轮询 /api/live)
  GET /api/live      JSON:{tasks:[{task_id, workflow, variant, status,
                        device_name, soc, stages:[{status, at, dur}]}],
                        generated_at}
  GET /healthz

配置(env):
  DASHBOARD_PORT      缺省 18687
  DASHBOARD_DB_HOST   缺省 127.0.0.1(compose postgres 端口映射)
  DASHBOARD_DB_PORT   缺省 5432
  DASHBOARD_DB_USER   缺省 hermes_runtime
  DASHBOARD_DB_PASSWORD  必填
  DASHBOARD_DB_NAME   缺省 hermes_runtime
"""
import json
import os
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import psycopg2

DB = {
    "host": os.environ.get("DASHBOARD_DB_HOST", "127.0.0.1"),
    "port": os.environ.get("DASHBOARD_DB_PORT", "5432"),
    "user": os.environ.get("DASHBOARD_DB_USER", "hermes_runtime"),
    "password": os.environ.get("DASHBOARD_DB_PASSWORD", ""),
    "dbname": os.environ.get("DASHBOARD_DB_NAME", "hermes_runtime"),
}
PORT = int(os.environ.get("DASHBOARD_PORT", "18687"))

# 阶段顺序(§9 状态机)
STAGES = ["QUEUED", "ACCEPTED", "DOWNLOADING", "PREPARING", "DEPLOYING",
          "RUNNING", "COLLECTING", "COMPLETED", "FAILED", "TIMEOUT", "CANCELED"]

# 阶段中文名
STAGE_CN = {
    "QUEUED": "排队", "ACCEPTED": "已接受", "DOWNLOADING": "下载中",
    "PREPARING": "准备中", "DEPLOYING": "部署中", "RUNNING": "运行中",
    "COLLECTING": "收集", "COMPLETED": "完成", "FAILED": "失败",
    "TIMEOUT": "超时", "CANCELED": "取消",
}

PIPELINE = ["QUEUED", "ACCEPTED", "DOWNLOADING", "PREPARING", "DEPLOYING",
            "RUNNING", "COLLECTING", "COMPLETED"]


def conn():
    return psycopg2.connect(**DB)


def live_tasks():
    """返回运行中(未终态)任务 + 各阶段时间线。"""
    with conn() as c:
        cur = c.cursor()
        # 未终态任务(含刚结束但 events 仍在的)
        cur.execute("""
            SELECT t.task_id, t.workflow_id, t.test_id, t.status, t.attempt,
                   t.device_id, t.client_id, t.created_at, t.ended_at,
                   d.display_name, d.soc, d.os,
                   (SELECT to_status FROM task_events e
                    WHERE e.task_id = t.task_id ORDER BY e.seq DESC LIMIT 1) AS last_stage
            FROM tasks t
            LEFT JOIN devices d ON d.device_id = t.device_id
            WHERE t.status NOT IN ('COMPLETED','FAILED','TIMEOUT','CANCELED')
               OR t.ended_at >= now() - interval '10 minutes'
            ORDER BY t.created_at DESC
            LIMIT 20
        """)
        rows = cur.fetchall()
        tasks = []
        for r in rows:
            (task_id, wf, test_id, status, attempt, dev_id, client_id,
             created, ended, disp_name, soc, os_, last_stage) = r
            # 该任务的阶段时间线
            cur.execute("""
                SELECT from_status, to_status, created_at
                FROM task_events WHERE task_id = %s ORDER BY seq
            """, (task_id,))
            events = cur.fetchall()
            stages = []
            for fs, ts, at in events:
                stages.append({"status": ts, "at": at.isoformat() if at else None})
            tasks.append({
                "task_id": task_id,
                "workflow_id": wf,
                "variant": test_id,
                "status": status,
                "attempt": attempt,
                "last_stage": last_stage or status,
                "device_name": disp_name or dev_id,
                "soc": soc or "",
                "os": os_ or "",
                "client_id": client_id,
                "created_at": created.isoformat() if created else None,
                "ended_at": ended.isoformat() if ended else None,
                "stages": stages,
            })
        return tasks


def fmt_dur(sec):
    if sec is None:
        return "-"
    if sec < 1:
        return f"{sec*1000:.0f}ms"
    if sec < 60:
        return f"{sec:.1f}s"
    return f"{sec/60:.1f}m"


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        print(f"[http] {self.address_string()} {fmt % args}", flush=True)

    def _json(self, status, body):
        data = json.dumps(body, ensure_ascii=False).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _html(self):
        html = PAGE  # 在文件底部定义
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(html.encode())))
        self.end_headers()
        self.wfile.write(html.encode())

    def do_GET(self):
        if self.path == "/" or self.path == "/index.html":
            self._html()
        elif self.path == "/api/live":
            try:
                self._json(200, {"tasks": live_tasks(),
                                 "generated_at": datetime.now(timezone.utc).isoformat()})
            except Exception as e:  # noqa: BLE001
                self._json(500, {"error": str(e)})
        elif self.path == "/healthz":
            self._json(200, {"ok": True})
        else:
            self._json(404, {"error": "not found"})


# ---- HTML 页面(自刷新)----
PAGE = """<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>DevOps 任务实时面板</title>
<style>
  body { font-family: -apple-system, 'Segoe UI', sans-serif; margin: 16px; background:#f5f6fa; }
  h1 { font-size: 18px; }
  .task { background:#fff; border:1px solid #e1e4ea; border-radius:8px; padding:12px 16px; margin-bottom:12px; }
  .task-head { display:flex; justify-content:space-between; align-items:center; }
  .variant { font-weight:600; font-size:14px; }
  .meta { color:#666; font-size:12px; margin-top:2px; }
  .badge { padding:2px 8px; border-radius:10px; font-size:12px; color:#fff; }
  .badge.COMPLETED,.badge.PASSED { background:#2ab371; }
  .badge.FAILED,.badge.TIMEOUT { background:#d54545; }
  .badge.RUNNING,.badge.ACCEPTED { background:#3574f0; }
  .badge.DOWNLOADING,.badge.DEPLOYING,.badge.PREPARING,.badge.COLLECTING { background:#f5a623; }
  .badge.QUEUED { background:#9aa4b2; }
  .pipeline { display:flex; gap:6px; margin-top:10px; flex-wrap:wrap; }
  .stage { font-size:11px; padding:4px 10px; border-radius:4px; background:#eef0f4; color:#8a93a3; }
  .stage.active { background:#3574f0; color:#fff; font-weight:600; }
  .stage.done { background:#2ab371; color:#fff; }
  .stage.fail { background:#d54545; color:#fff; }
  .empty { color:#8a93a3; padding:30px; text-align:center; }
  .gen { color:#999; font-size:11px; }
</style></head><body>
<h1>📊 DevOps 任务实时面板 <span class="gen" id="gen"></span></h1>
<div id="list"><div class="empty">加载中…</div></div>
<script>
const PIPELINE = ["QUEUED","ACCEPTED","DOWNLOADING","PREPARING","DEPLOYING","RUNNING","COLLECTING","COMPLETED"];
const CN = {"QUEUED":"排队","ACCEPTED":"已接受","DOWNLOADING":"下载中","PREPARING":"准备中","DEPLOYING":"部署中","RUNNING":"运行中","COLLECTING":"收集","COMPLETED":"完成","FAILED":"失败","TIMEOUT":"超时","CANCELED":"取消"};
async function load() {
  try {
    const r = await fetch('/api/live');
    const d = await r.json();
    document.getElementById('gen').textContent = '更新于 ' + new Date(d.generated_at).toLocaleTimeString();
    render(d.tasks || []);
  } catch(e) { document.getElementById('list').innerHTML = '<div class="empty">加载失败: '+e+'</div>'; }
}
function render(tasks) {
  const el = document.getElementById('list');
  if (!tasks.length) { el.innerHTML = '<div class="empty">当前无运行中任务</div>'; return; }
  el.innerHTML = tasks.map(t => {
    const lastIdx = PIPELINE.indexOf(t.last_stage);
    const curIdx = PIPELINE.indexOf(t.status);
    const activeIdx = curIdx >= 0 ? curIdx : (lastIdx >= 0 ? lastIdx : -1);
    const stages = PIPELINE.map((s, i) => {
      let cls = 'stage';
      if (i < activeIdx) cls += ' done';
      else if (i === activeIdx) cls += ' active';
      if (t.status === 'FAILED' || t.status === 'TIMEOUT') cls = 'stage fail';
      return `<span class="${cls}">${CN[s]}</span>`;
    }).join('');
    const badge = `<span class="badge ${t.status}">${CN[t.status]||t.status}</span>`;
    return `<div class="task">
      <div class="task-head"><span class="variant">${esc(t.variant)} ${badge}</span>
        <span class="meta">attempt ${t.attempt}</span></div>
      <div class="meta">📱 ${esc(t.device_name||'-')} ${t.soc?esc(t.soc):''} ${t.os?esc(t.os):''} · client ${esc(t.client_id)}</div>
      <div class="pipeline">${stages}</div>
      <div class="meta" style="margin-top:6px">${esc(t.task_id)}</div>
    </div>`;
  }).join('');
}
function esc(s){ return String(s||'').replace(/[&<>"]/g, c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }
load();
setInterval(load, 2000);
</script></body></html>
"""


def main():
    print(f"task-dashboard listening on 0.0.0.0:{PORT}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
