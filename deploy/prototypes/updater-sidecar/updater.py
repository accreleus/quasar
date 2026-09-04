#!/usr/bin/env python3
"""PROTOTYPE (#113) — throwaway updater sidecar. Not production code.

HTTP over a unix socket in a volume shared with the agent (and the control plane on its
host). A request names component image digests and the compose services to recreate.

  POST /apply    {"id": "...", "images": {"control": "<ref@sha256:..>", "agent": "..."},
                  "services": ["quasar-control-plane"], "wait": true}
  GET  /result/<id>          the result file the executor wrote (also on disk, see RESULT_DIR)
  GET  /results              every result file, newest first
  GET  /whoami               what the sidecar discovered about its own compose project

The decision (`plan`) is a pure function; the executor is detached from the request
(setsid + closed stdio) so it survives the requester's own recreate — that is question 2.
"""
import json, os, re, subprocess, sys, time, socketserver, http.server, threading, glob

SOCK_DIR = os.environ.get("UPDATER_RUN_DIR", "/run/quasar-updater")
SOCK = os.path.join(SOCK_DIR, "updater.sock")
RESULT_DIR = os.path.join(SOCK_DIR, "results")
ENV_FILE = os.environ.get("UPDATER_ENV_FILE")  # default: <project working_dir>/.env
ALLOW = [a.strip() for a in os.environ.get("UPDATER_ALLOW", "ghcr.io/accreleus/quasar/").split(",") if a.strip()]
SERVICE_ENV = {"control": "QUASAR_CONTROL_IMAGE", "agent": "QUASAR_AGENT_IMAGE"}
KNOWN_SERVICES = {"quasar-control-plane": "control", "quasar-node-agent": "agent"}
DIGEST_RE = re.compile(r"^[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}$")
DOCKER = os.environ.get("UPDATER_DOCKER_BIN", "docker")


def log(*a):
    print(time.strftime("%H:%M:%S"), "updater:", *a, flush=True)


# ---------- self discovery: which compose project am I part of? ----------
def discover_project():
    """Read our own container's compose labels. Falls back to env."""
    info = {"source": "env", "project": os.environ.get("COMPOSE_PROJECT_NAME"),
            "config_files": os.environ.get("COMPOSE_FILE"), "working_dir": os.environ.get("UPDATER_WORKING_DIR")}
    cid = os.environ.get("HOSTNAME") or open("/etc/hostname").read().strip()
    try:
        out = subprocess.run([DOCKER, "inspect", cid, "--format", "{{json .Config.Labels}}"],
                             capture_output=True, text=True, timeout=20)
        if out.returncode == 0:
            labels = json.loads(out.stdout)
            if labels.get("com.docker.compose.project"):
                info = {"source": "labels", "container": cid,
                        "project": labels["com.docker.compose.project"],
                        "config_files": labels.get("com.docker.compose.project.config_files"),
                        "working_dir": labels.get("com.docker.compose.project.working_dir"),
                        "service": labels.get("com.docker.compose.service")}
        else:
            info["inspect_error"] = out.stderr.strip()
    except Exception as e:  # noqa
        info["inspect_error"] = repr(e)
    return info


PROJECT = discover_project()


def compose_base():
    args = [DOCKER, "compose"]
    if PROJECT.get("project"):
        args += ["-p", PROJECT["project"]]
    if PROJECT.get("working_dir"):
        args += ["--project-directory", PROJECT["working_dir"]]
    for f in (PROJECT.get("config_files") or "").split(","):
        if f:
            args += ["-f", f]
    return args


def env_path():
    return ENV_FILE or os.path.join(PROJECT.get("working_dir") or ".", ".env")


# ---------- the pure decision ----------
def plan(req, env_text, allow=ALLOW):
    """Return (ok, reasons, new_env_text, previous, commands). No I/O."""
    reasons, previous, targets = [], {}, {}
    images = req.get("images") or {}
    services = req.get("services") or []
    for comp, ref in images.items():
        if comp not in SERVICE_ENV:
            reasons.append(f"unknown component {comp}")
            continue
        if not DIGEST_RE.match(ref):
            reasons.append(f"{comp}: not a digest reference: {ref}")
        elif not any(ref.startswith(a) for a in allow):
            reasons.append(f"{comp}: {ref} not under allowlist {allow}")
        else:
            targets[SERVICE_ENV[comp]] = ref
    for s in services:
        if s not in KNOWN_SERVICES:
            reasons.append(f"unknown service {s}")
        elif SERVICE_ENV[KNOWN_SERVICES[s]] not in targets:
            reasons.append(f"service {s} named but no image for {KNOWN_SERVICES[s]} given")
    if not services:
        reasons.append("no services named")
    if reasons:
        return False, reasons, env_text, previous, []
    lines, seen = [], set()
    for line in env_text.splitlines():
        m = re.match(r"^\s*(QUASAR_CONTROL_IMAGE|QUASAR_AGENT_IMAGE)=(.*)$", line)
        if m and m.group(1) in targets:
            previous[m.group(1)] = m.group(2)
            lines.append(f"{m.group(1)}={targets[m.group(1)]}")
            seen.add(m.group(1))
        else:
            lines.append(line)
    for k, v in targets.items():
        if k not in seen:
            previous[k] = None
            lines.append(f"{k}={v}")
    new_env = "\n".join(lines) + "\n"
    up = ["up", "-d", "--force-recreate", "--no-deps"] + (["--wait", "--wait-timeout", str(req.get("wait_timeout", 120))] if req.get("wait", True) else [])
    commands = [["pull"] + services, up + services]
    return True, [], new_env, previous, commands


# ---------- the executor (runs detached) ----------
def execute(req_id, req, new_env, previous, commands):
    result = {"id": req_id, "state": "pending", "requested": req.get("images"), "previous": previous,
              "services": req.get("services"), "started": time.time(), "steps": [], "pid": os.getpid()}
    path = os.path.join(RESULT_DIR, f"{req_id}.json")

    def write(state=None):
        if state:
            result["state"] = state
        result["updated"] = time.time()
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(result, f, indent=1)
        os.replace(tmp, path)

    write("rewriting-env")
    ep = env_path()
    with open(ep) as f:
        cur = f.read()
    with open(ep + ".prev", "w") as f:
        f.write(cur)
    with open(ep, "w") as f:
        f.write(new_env)
    for i, cmd in enumerate(commands):
        full = compose_base() + cmd
        write("pulling" if cmd[0] == "pull" else "recreating")
        step = {"cmd": " ".join(full), "t0": time.time()}
        result["steps"].append(step)
        write()
        p = subprocess.run(full, capture_output=True, text=True)
        step.update(rc=p.returncode, t1=time.time(), stdout=p.stdout[-4000:], stderr=p.stderr[-4000:])
        if p.returncode != 0:
            step["failed"] = True
            write("failed")
            log(f"{req_id}: step {i} FAILED rc={p.returncode}")
            return
        write()
    # Finding (E4): compose's exit code is not a reliable "the service is up" signal across
    # plugin versions, so verify the post-state ourselves: every named service must have a
    # container that actually STARTED and is running (healthy, if it has a healthcheck).
    write("verifying")
    bad = []
    for svc in req.get("services") or []:
        p = subprocess.run(compose_base() + ["ps", "-a", "--format", "json", svc], capture_output=True, text=True)
        rows = [json.loads(l) for l in p.stdout.splitlines() if l.strip()]
        for r in rows:
            ok = r.get("State") == "running" and r.get("Health") in ("", "healthy", None)
            if not ok:
                bad.append({k: r.get(k) for k in ("Name", "State", "Status", "Health", "ExitCode")})
        if not rows:
            bad.append({"Name": svc, "State": "missing"})
    result["verify"] = bad
    if bad:
        write("failed")
        log(f"{req_id}: verify FAILED {bad}")
        return
    write("succeeded")
    log(f"{req_id}: succeeded")


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def _send(self, code, obj):
        body = json.dumps(obj, indent=1).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        log(self.command, self.path, fmt % args)

    def do_GET(self):
        if self.path == "/whoami":
            return self._send(200, {"project": PROJECT, "compose": compose_base(), "env_file": env_path(), "allow": ALLOW,
                                    "uid": os.getuid(), "sock": SOCK})
        if self.path == "/results":
            files = sorted(glob.glob(os.path.join(RESULT_DIR, "*.json")), key=os.path.getmtime, reverse=True)
            return self._send(200, [json.load(open(f)) for f in files])
        if self.path.startswith("/result/"):
            p = os.path.join(RESULT_DIR, os.path.basename(self.path[len("/result/"):]) + ".json")
            if not os.path.exists(p):
                return self._send(404, {"error": "no such result"})
            return self._send(200, json.load(open(p)))
        self._send(404, {"error": "unknown path"})

    def do_POST(self):
        if self.path != "/apply":
            return self._send(404, {"error": "unknown path"})
        n = int(self.headers.get("Content-Length") or 0)
        try:
            req = json.loads(self.rfile.read(n) or b"{}")
        except Exception as e:  # noqa
            return self._send(400, {"error": f"bad json: {e}"})
        req_id = req.get("id") or time.strftime("%Y%m%dT%H%M%S")
        # Peer credentials: who is asking? (question 1)
        try:
            import struct
            creds = self.connection.getsockopt(__import__("socket").SOL_SOCKET, 17, struct.calcsize("3i"))  # SO_PEERCRED
            pid, uid, gid = struct.unpack("3i", creds)
        except Exception:  # noqa
            pid = uid = gid = None
        with open(env_path()) as f:
            env_text = f.read()
        ok, reasons, new_env, previous, commands = plan(req, env_text)
        log(f"{req_id}: from pid={pid} uid={uid} gid={gid} -> ok={ok} {reasons}")
        if not ok:
            return self._send(422, {"id": req_id, "state": "rejected", "reasons": reasons, "peer": {"pid": pid, "uid": uid, "gid": gid}})
        if req.get("dry_run"):
            return self._send(200, {"id": req_id, "state": "dry-run", "previous": previous, "commands": [" ".join(compose_base() + c) for c in commands],
                                    "env_diff": [l for l in new_env.splitlines() if l.startswith("QUASAR_")]})
        # Detach: the executor must outlive this connection AND the requester (question 2).
        pid_child = os.fork()
        if pid_child == 0:
            import signal
            signal.signal(signal.SIGCHLD, signal.SIG_DFL)  # the parent ignores SIGCHLD to auto-reap us; with
            os.setsid()                                   # SIG_IGN inherited, subprocess.run reports rc 0 for EVERY child
            for fd in (0, 1):
                os.close(fd)
            os.open("/dev/null", os.O_RDONLY)
            logf = os.open(os.path.join(RESULT_DIR, f"{req_id}.log"), os.O_WRONLY | os.O_CREAT | os.O_APPEND)
            os.dup2(logf, 1); os.dup2(logf, 2)
            try:
                execute(req_id, req, new_env, previous, commands)
            finally:
                os._exit(0)
        self._send(202, {"id": req_id, "state": "accepted", "executor_pid": pid_child, "previous": previous,
                         "commands": [" ".join(compose_base() + c) for c in commands], "peer": {"pid": pid, "uid": uid, "gid": gid}})


class UnixHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def get_request(self):
        conn, _ = super().get_request()
        return conn, ("unix", 0)


def main():
    os.makedirs(RESULT_DIR, exist_ok=True)
    if os.path.exists(SOCK):
        os.unlink(SOCK)
    srv = UnixHTTPServer(SOCK, Handler)
    os.chmod(SOCK, int(os.environ.get("UPDATER_SOCK_MODE", "0666"), 8))
    if os.environ.get("UPDATER_SOCK_GID"):
        os.chown(SOCK, -1, int(os.environ["UPDATER_SOCK_GID"]))
    log(f"listening on {SOCK} uid={os.getuid()} project={PROJECT}")
    log(f"compose base: {' '.join(compose_base())}; env file: {env_path()}; allow: {ALLOW}")
    import signal
    signal.signal(signal.SIGCHLD, signal.SIG_IGN)  # auto-reap detached executors
    srv.serve_forever()


if __name__ == "__main__":
    main()
