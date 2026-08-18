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
            prev_at = None
            for fs, ts, at in events:
                dur = None
                if prev_at is not None and at is not None:
                    dur = max(0, (at - prev_at).total_seconds())
                stages.append({"status": ts, "at": at.isoformat() if at else None,
                               "dur": dur})
                prev_at = at
            # 终态任务的指标/退出码
            metrics, exit_code = {}, None
            cur.execute("SELECT result_json FROM results WHERE task_id = %s", (task_id,))
            res_row = cur.fetchone()
            if res_row:
                try:
                    # psycopg2 对 JSONB 列直接返回 dict(不再二次 loads)
                    res = res_row[0]
                    if isinstance(res, (str, bytes)):
                        res = json.loads(res)
                    metrics = res.get("metrics") or {}
                    exit_code = res.get("exit_code")
                except Exception:
                    pass
            # 总耗时
            total_dur = None
            if created and ended:
                total_dur = max(0, (ended - created).total_seconds())
            elif created:
                total_dur = max(0, (datetime.now(timezone.utc) - created).total_seconds())
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
                "total_dur": total_dur,
                "stages": stages,
                "metrics": metrics,
                "exit_code": exit_code,
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


# ---- HTML 页面(自刷新,2026-08-18 增强)----
PAGE = """<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DevOps 任务实时面板</title>
<style>
  :root {
    --bg:#0f172a; --card:#1e293b; --card2:#273449; --line:#334155;
    --text:#e2e8f0; --muted:#94a3b8; --blue:#38bdf8; --green:#34d399;
    --red:#f87171; --orange:#fbbf24; --purple:#a78bfa;
  }
  * { box-sizing:border-box; }
  body { font-family:-apple-system,'Segoe UI',system-ui,sans-serif; margin:0;
         background:linear-gradient(135deg,#0f172a,#1e293b); color:var(--text);
         min-height:100vh; }
  header { padding:18px 24px; display:flex; align-items:center; gap:14px;
           border-bottom:1px solid var(--line); background:rgba(15,23,42,.7);
           position:sticky; top:0; backdrop-filter:blur(6px); z-index:10; }
  h1 { font-size:19px; margin:0; font-weight:700; letter-spacing:.3px; }
  .dot { width:10px; height:10px; border-radius:50%; background:var(--green);
         box-shadow:0 0 8px var(--green); animation:pulse 1.6s infinite; }
  @keyframes pulse { 0%,100%{opacity:1} 50%{opacity:.4} }
  .gen { color:var(--muted); font-size:12px; margin-left:auto; }
  .stats { display:flex; gap:10px; padding:14px 24px 0; }
  .stat { background:var(--card); border:1px solid var(--line); border-radius:10px;
          padding:8px 16px; min-width:110px; }
  .stat .n { font-size:22px; font-weight:700; }
  .stat .l { font-size:11px; color:var(--muted); }
  .stat.running .n{color:var(--blue)} .stat.done .n{color:var(--green)}
  .stat.fail .n{color:var(--red)}
  main { padding:16px 24px; }
  .task { background:var(--card); border:1px solid var(--line); border-radius:12px;
          margin-bottom:14px; overflow:hidden; }
  .task.active-task { border-color:var(--blue); box-shadow:0 0 0 1px var(--blue), 0 8px 24px rgba(56,189,248,.12); }
  .task-head { display:flex; align-items:center; gap:10px; padding:12px 16px;
               border-bottom:1px solid var(--line); background:var(--card2); }
  .variant { font-weight:600; font-size:14px; }
  .badge { padding:3px 10px; border-radius:12px; font-size:11px; font-weight:600; }
  .badge.COMPLETED,.badge.PASSED { background:rgba(52,211,153,.15); color:var(--green); }
  .badge.FAILED,.badge.TIMEOUT { background:rgba(248,113,113,.15); color:var(--red); }
  .badge.RUNNING,.badge.ACCEPTED { background:rgba(56,189,248,.15); color:var(--blue); }
  .badge.DOWNLOADING,.badge.PREPARING,.badge.DEPLOYING,.badge.COLLECTING { background:rgba(251,191,36,.15); color:var(--orange); }
  .badge.QUEUED { background:rgba(148,163,184,.15); color:var(--muted); }
  .task-meta { padding:8px 16px 0; font-size:12px; color:var(--muted);
               display:flex; gap:16px; flex-wrap:wrap; }
  .task-meta b { color:var(--text); font-weight:500; }
  .pipeline { display:flex; padding:12px 16px; gap:0; overflow-x:auto; }
  .stage { flex:1; min-width:70px; text-align:center; padding:8px 6px; font-size:11px;
           position:relative; background:var(--card2); color:var(--muted);
           border-top:2px solid var(--line); border-bottom:2px solid var(--line); }
  .stage:first-child { border-radius:6px 0 0 6px; border-left:2px solid var(--line); }
  .stage:last-child { border-radius:0 6px 6px 0; border-right:2px solid var(--line); }
  .stage.done { background:rgba(52,211,153,.08); border-color:var(--green); color:var(--green); }
  .stage.active { background:rgba(56,189,248,.12); border-color:var(--blue); color:var(--blue); font-weight:700; animation:pulse 1.6s infinite; }
  .stage.fail { background:rgba(248,113,113,.12); border-color:var(--red); color:var(--red); }
  .stage .dur { display:block; font-size:10px; opacity:.7; margin-top:2px; }
  .task-foot { padding:0 16px 12px; font-size:11px; color:var(--muted);
               display:flex; justify-content:space-between; gap:8px; flex-wrap:wrap; }
  .metrics { display:flex; gap:8px; padding:0 16px 12px; flex-wrap:wrap; }
  .metric { background:var(--card2); border:1px solid var(--line); border-radius:8px;
            padding:4px 10px; font-size:11px; }
  .metric b { color:var(--purple); }
  .empty { color:var(--muted); text-align:center; padding:60px 0; font-size:14px; }
  .task-id { font-family:ui-monospace,monospace; font-size:10px; word-break:break-all; }
</style></head><body>
<header><span class="dot"></span><h1>DevOps 任务实时面板</h1><span class="gen" id="gen"></span></header>
<div class="stats" id="stats"></div>
<main><div id="list"><div class="empty">加载中…</div></div></main>
<script>
const PIPELINE=["QUEUED","ACCEPTED","DOWNLOADING","PREPARING","DEPLOYING","RUNNING","COLLECTING","COMPLETED"];
const CN={"QUEUED":"排队","ACCEPTED":"已接受","DOWNLOADING":"下载中","PREPARING":"准备中","DEPLOYING":"部署中","RUNNING":"运行中","COLLECTING":"收集","COMPLETED":"完成","FAILED":"失败","TIMEOUT":"超时","CANCELED":"取消"};
function fmtDur(s){
  if(s==null||isNaN(s))return'-';
  if(s<1)return Math.round(s*1000)+'ms';
  if(s<60)return s.toFixed(1)+'s';
  return (s/60).toFixed(1)+'m';
}
function metricsHtml(t){
  if(!t.metrics||!Object.keys(t.metrics).length)return'';
  return '<div class="metrics">'+Object.entries(t.metrics).map(([k,v])=>
    `<span class="metric">${esc(k.replace('.inference_ms_avg',''))} <b>${typeof v==='number'?v.toFixed(1):v}ms</b></span>`).join('')+'</div>';
}
function render(tasks){
  const running=tasks.filter(t=>!['COMPLETED','FAILED','TIMEOUT','CANCELED'].includes(t.status));
  const done=tasks.filter(t=>['COMPLETED'].includes(t.status));
  const failed=tasks.filter(t=>['FAILED','TIMEOUT'].includes(t.status));
  document.getElementById('stats').innerHTML=
    `<div class="stat running"><div class="n">${running.length}</div><div class="l">运行中</div></div>`+
    `<div class="stat done"><div class="n">${done.length}</div><div class="l">已完成</div></div>`+
    `<div class="stat fail"><div class="n">${failed.length}</div><div class="l">失败</div></div>`;
  const el=document.getElementById('list');
  if(!tasks.length){el.innerHTML='<div class="empty">当前无运行中任务</div>';return;}
  const sorted=[...tasks].sort((a,b)=>new Date(b.created_at)-new Date(a.created_at));
  el.innerHTML=sorted.map(t=>{
    const isActive=!['COMPLETED','FAILED','TIMEOUT','CANCELED'].includes(t.status);
    const activeIdx=PIPELINE.indexOf(isActive?t.status:t.last_stage);
    const stages=PIPELINE.map((s,i)=>{
      let cls='stage';
      const stage=t.stages.find(x=>x.status===s);
      const dur=stage?stage.dur:null;
      if(isActive){ if(i<activeIdx)cls+=' done'; else if(i===activeIdx)cls+=' active'; }
      else { if(i<=activeIdx)cls+=' done'; }
      if(t.status==='FAILED'||t.status==='TIMEOUT')cls='stage fail';
      return `<span class="${cls}">${CN[s]}<span class="dur">${fmtDur(dur)}</span></span>`;
    }).join('');
    return `<div class="task ${isActive?'active-task':''}">
      <div class="task-head"><span class="badge ${t.status}">${CN[t.status]||t.status}</span>
        <span class="variant">${esc(t.variant)}</span>
        <span class="gen" style="margin-left:auto">attempt ${t.attempt} · ${fmtDur(t.total_dur)}</span></div>
      <div class="task-meta">
        <span>📱 <b>${esc(t.device_name||'-')}</b>${t.soc?' · '+esc(t.soc):''}${t.os?' · '+esc(t.os):''}</span>
        <span>🖥️ client <b>${esc(t.client_id)}</b></span>
      </div>
      <div class="pipeline">${stages}</div>
      ${metricsHtml(t)}
      <div class="task-foot"><span class="task-id">${esc(t.task_id)}</span></div>
    </div>`;
  }).join('');
}
function esc(s){return String(s??'').replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));}
async function load(){
  try{const r=await fetch('/api/live');const d=await r.json();
    document.getElementById('gen').textContent='更新于 '+new Date(d.generated_at).toLocaleTimeString();
    render(d.tasks||[]);}catch(e){document.getElementById('list').innerHTML='<div class="empty">加载失败: '+e+'</div>';}
}
load();setInterval(load,2000);
</script></body></html>
"""


def main():
    print(f"task-dashboard listening on 0.0.0.0:{PORT}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
