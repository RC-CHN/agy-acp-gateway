"""Private stdio adapter launched by the Go gateway for the pinned sample."""
import contextlib
from datetime import datetime, timezone
from email.utils import parsedate_to_datetime
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import sqlite3
import subprocess
import sys
import threading


UUID = re.compile(r"[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}")
EMPTY = (-1, -1)


def conversation(sid):
    if not isinstance(sid, str) or not UUID.fullmatch(sid):
        raise ValueError("invalid ACP session ID")
    return Path.home() / ".gemini/antigravity-cli/conversations" / (sid + ".db")


@contextlib.contextmanager
def snapshot(path):
    with contextlib.closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True,
                                           timeout=0.05)) as db:
        db.execute("PRAGMA query_only=on")
        db.execute("BEGIN")
        yield db


def baseline(sid):
    try:
        path = conversation(sid)
        if not path.exists():
            return EMPTY
        with snapshot(path) as db:
            return db.execute("SELECT (SELECT coalesce(max(idx),-1) FROM steps), "
                              "(SELECT coalesce(max(idx),-1) FROM gen_metadata)").fetchone()
    except (OSError, ValueError, sqlite3.Error):
        return None


def varint(blob, end):
    value = 0
    for shift in range(0, 70, 7):
        if blob.tell() >= end:
            break
        b = blob.read(1)[0]
        value |= (b & 127) << shift
        if b < 128:
            return value
    raise ValueError("invalid varint")


def fields(blob, span, selected):
    """Index selected protobuf fields by offset. Skip text and nested payloads."""
    start, size = span
    end = start + size
    blob.seek(start)
    values = {}
    while blob.tell() < end:
        tag = varint(blob, end)
        key, wire = divmod(tag, 8)
        if key == 0:
            raise ValueError("invalid field")
        if wire == 0:
            value = varint(blob, end)
        elif wire in (1, 2, 5):
            n = varint(blob, end) if wire == 2 else (8 if wire == 1 else 4)
            pos = blob.tell()
            if n > end - pos:
                raise ValueError("invalid length")
            value = (pos, n)
            blob.seek(pos + n)
        else:
            raise ValueError("unsupported wire type")
        if key in selected:
            if wire != selected[key] or key in values:
                raise ValueError("ambiguous field")
            values[key] = value
    return values


def read_text(blob, span):
    pos, size = span
    if size > 65536:
        raise ValueError("oversized diagnostic")
    blob.seek(pos)
    return blob.read(size).decode("utf-8")


def http_status_line(blob, span, code):
    # Stop at the next label, before the Sherlog value, trace ID and header values.
    pos, size = span
    end = pos + size
    blob.seek(pos)
    line = bytearray()
    while blob.tell() < end and len(line) < 72:
        char = blob.read(1)
        if char == b"\n":
            break
        line.extend(char)
    else:
        return False
    prefix = b"HTTP " + str(code).encode()
    return (re.fullmatch(re.escape(prefix) + rb"(?: [^\r\n]{1,64})?", line) is not None
            and end - blob.tell() >= 9 and blob.read(9) == b"Sherlog: ")


FIELD = "gateway_observed_http_status"
STATE = "gateway_observed_provider_status"
RETRY = "gateway_observed_retry_after_s"
SOURCE = "gateway_observed_source"


def unique_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def retry_header(blob, span):
    # Pinned FullError begins with four CLI-generated lines before the body.
    pos, size = span
    blob.seek(pos)
    head, separator, _ = blob.read(min(size, 16384)).partition(b"\n\n")
    lines = head.split(b"\n")
    if not separator or len(lines) != 4 or not lines[1].startswith(b"Sherlog: ") or not lines[2].startswith(b"TraceID: 0x") or not lines[3].startswith(b"Headers: "):
        return None
    try:
        headers = json.loads(lines[3][9:], object_pairs_hook=unique_keys)
        if not isinstance(headers, dict):
            return None
        matches = [v for k, v in headers.items() if k.lower() == "retry-after"]
        if len(matches) != 1 or not isinstance(matches[0], list) or len(matches[0]) != 1:
            return None
        value = matches[0][0]
        if not isinstance(value, str):
            return None
        if re.fullmatch(r"[0-9]{1,16}", value):
            n = int(value)
            return n if n < 2**53 else None
        when = parsedate_to_datetime(value)
        if when.tzinfo is not None:
            return max(0, math.ceil((when - datetime.now(timezone.utc)).total_seconds()))
    except (TypeError, ValueError, OverflowError, RecursionError):
        pass
    return None


def model_metadata(blob, error, code):
    result = {FIELD: code}
    retryable = code in (429, 503)
    seconds = retry_header(blob, error[3]) if retryable else None
    if seconds is not None:
        result[RETRY] = seconds
    try:
        body = json.loads(read_text(blob, error[5]), object_pairs_hook=unique_keys)
        entry = body.get("error") if isinstance(body, dict) else None
        if not isinstance(entry, dict) or type(entry.get("code")) is not int or entry["code"] != code:
            return result
        state = entry.get("status")
        if isinstance(state, str) and re.fullmatch(r"[A-Z_]{1,32}", state):
            result[STATE] = state  # Go applies the public enum allowlist.
        details = entry.get("details")
        if retryable and seconds is None and isinstance(details, list):
            hints = [v.get("retryDelay") for v in details if isinstance(v, dict)
                     and v.get("@type") == "type.googleapis.com/google.rpc.RetryInfo"]
            if len(hints) == 1 and isinstance(hints[0], str):
                duration = re.fullmatch(r"([0-9]{1,12})(?:\.([0-9]{1,9}))?s", hints[0])
                if duration and int(duration[1]) <= 315576000000:
                    result[RETRY] = int(duration[1]) + int(bool((duration[2] or "").strip("0")))
    except (ValueError, OverflowError, RecursionError):
        pass
    return result


def stored_status(sid, before, wire_text):
    if before is None:
        return 0
    try:
        with snapshot(conversation(sid)) as db:
            last, gen = before
            row = db.execute("SELECT idx, step_type, status, has_subtrajectory, step_format FROM steps "
                             "WHERE idx > ? ORDER BY idx DESC LIMIT 1", (last,)).fetchone()
            if not row or row[1:] != (17, 3, 0, 0):
                return 0
            idx = row[0]
            if db.execute("SELECT count(*) FROM steps WHERE idx > ? AND step_type=14", (last,)).fetchone() != (1,):
                return 0
            trajectories = db.execute("SELECT trajectory_id FROM trajectory_meta WHERE cascade_id=?", (sid,)).fetchall()
            if len(trajectories) != 1:
                return 0
            trajectory = trajectories[0][0]
            with db.blobopen("steps", "step_payload", idx, readonly=True) as blob:
                step = fields(blob, (0, len(blob)), {1: 0, 4: 0, 5: 2, 24: 2})
                meta = fields(blob, step[5], {3: 0, 20: 2})
                ref = fields(blob, meta[20], {1: 2, 2: 0, 4: 2})
                visible = fields(blob, step[24], {3: 2, 5: 0})
                if (step[1], step[4], meta[3], ref[2], visible[5]) != (17, 3, 5, idx, 1):
                    return 0
                if read_text(blob, ref[1]) != trajectory or read_text(blob, ref[4]) != sid:
                    return 0
                error = fields(blob, visible[3], {1: 2, 3: 2, 5: 2, 6: 2, 7: 0, 9: 2})
                code = error[7]
                if not (400 <= code <= 599 and error[5][1] and
                        read_text(blob, error[6]) == f"{trajectory}-{idx}"):
                    return 0
                # HTTPError.ToCortexErrorDetails fills FullError with its own HTTP
                # status line. Read only that line and its fixed next-field label.
                if not http_status_line(blob, error[3], code):
                    return 0
                user = read_text(blob, error[1]).strip()
                short = read_text(blob, error[9])
                message = user
                if user in ("", "Agent execution terminated due to error.") and short.strip():
                    message = short.strip()
                elif user == "Our servers are experiencing high traffic right now, please try again in a minute." and short.strip() and short.strip() not in user:
                    message = user + " (" + short.strip() + ")"
                if wire_text != "\n⚠ " + message + "\n":
                    return 0
                metadata = model_metadata(blob, error, code)
            generation = db.execute("SELECT max(idx) FROM gen_metadata WHERE idx>?", (gen,)).fetchone()[0]
            if generation is None:
                return 0
            with db.blobopen("gen_metadata", "data", generation, readonly=True) as blob:
                record = fields(blob, (0, len(blob)), {5: 2})
                if not short or read_text(blob, record[5]) != short:
                    return 0
            return metadata
    except (OSError, sqlite3.Error, ValueError, KeyError, IndexError, OverflowError):
        return 0


def main():
    backend, agent, binary, *args = sys.argv[1:]
    if backend == "frida":
        import frida
    elif not hasattr(sqlite3.Connection, "blobopen"):
        sys.exit("the trajectory reader needs Python 3.11+ with sqlite3")
    # Pin the executable on every router generation, including supervisor restarts.
    with open(binary, "rb") as sample:
        target = os.fstat(sample.fileno())
        digest = hashlib.sha256()
        for block in iter(lambda: sample.read(1 << 20), b""):
            digest.update(block)
    if digest.hexdigest() != "37873b8062391b6202a3d3199ec012abc9e53aab720d26cd65699441497d8c94":
        sys.exit("unsupported agy binary for status observation")
    if args == ["--check"]:
        return
    router = subprocess.Popen([binary, *args], stdin=subprocess.PIPE, stdout=subprocess.PIPE)
    # stdin/out belong to the gateway. Source code and the binary path are supplied
    # by the Go launcher; there is no attach-by-PID API accessible to the client.
    workers = {}
    pending = {}
    turns = {}
    lock = threading.Lock()

    def attach_workers():
        for pid in list(workers):
            if workers[pid][0].is_detached:
                del workers[pid]
        children = set()
        for entry in Path(f"/proc/{router.pid}/task").glob("*/children"):
            try:
                children.update(map(int, entry.read_text().split()))
            except FileNotFoundError:
                continue
        for pid in children - workers.keys():
            session = None
            try:
                proc = Path(f"/proc/{pid}")
                if not os.path.samestat(target, (proc / "exe").stat()):
                    continue
                if (proc / "cmdline").read_bytes().split(b"\0")[1:3] != [b"acp", b"--worker"]:
                    continue
                session = frida.attach(pid)
                script = session.create_script(agent)
                script.load()
                script.exports_sync.ready()
                workers[pid] = session, script
            except Exception:
                if session is not None and not session.is_detached:
                    session.detach()
                print("agy status observer could not attach to a session worker", file=sys.stderr)

    def forward_requests():
        try:
            for line in sys.stdin.buffer:
                f = json.loads(line)
                method = f.get("method")
                if method == "session/cancel":
                    with lock:
                        turns.pop(f.get("params", {}).get("sessionId"), None)
                if "id" in f and method in ("session/new", "session/prompt", "session/close"):
                    sid = f.get("params", {}).get("sessionId")
                    before = baseline(sid) if method == "session/prompt" and hasattr(sqlite3.Connection, "blobopen") else None
                    with lock:
                        pending[f["id"]] = (method, sid, before)
                        if method == "session/prompt":
                            turns[sid] = (None, 0)
                router.stdin.write(line)
                router.stdin.flush()
        except (BrokenPipeError, OSError, ValueError):
            pass
        finally:
            router.stdin.close()

    def emit(line):
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()

    def confirmed(sid, chunk, count, before):
        text = json.loads(chunk)["params"]["update"]["content"]["text"]
        def from_store():
            metadata = stored_status(sid, before, text)
            if metadata:
                metadata[SOURCE] = "trajectory_store"
            return metadata

        if backend == "store" or not workers:
            return from_store()
        size = len(text.encode("utf-8"))
        matches = []
        cancel = frida.Cancellable()
        timer = threading.Timer(1.0, cancel.cancel)
        timer.start()
        try:
            with cancel:
                for session, script in workers.values():
                    if session.is_detached:
                        continue
                    try:
                        code = script.exports_sync.match(sid, count, size)
                    except frida.OperationCancelledError:
                        return from_store()
                    except Exception:
                        continue
                    if type(code) is int and 400 <= code <= 599:
                        matches.append(code)
        finally:
            timer.cancel()
        if len(matches) != 1:
            return from_store()
        metadata = stored_status(sid, before, text)
        if metadata and metadata[FIELD] == matches[0]:
            metadata[SOURCE] = "frida"
            return metadata
        return {FIELD: matches[0], SOURCE: "frida"}

    threading.Thread(target=forward_requests, daemon=True).start()
    try:
        for line in router.stdout:
            f = json.loads(line)
            if "method" in f:
                params = f.get("params", {})
                sid = params.get("sessionId")
                update = params.get("update", {})
                kind = update.get("sessionUpdate")
                # Hold only a possible agy diagnostic until the next visible update or reply.
                # Metadata can be forwarded immediately. Flush before tool activity.
                with lock:
                    active = sid in turns
                    held, count = turns.get(sid, (None, 0))
                    text = (f["method"] == "session/update" and kind == "agent_message_chunk"
                            and isinstance(update.get("content", {}).get("text"), str))
                    candidate = text and update["content"]["text"].startswith("\n⚠ ")
                    visible = kind == "agent_message_chunk" or kind in ("agent_thought_chunk", "tool_call", "tool_call_update") or "id" in f
                    if active and visible:
                        turns[sid] = (line if candidate else None, count + int(kind == "agent_message_chunk"))
                if active and visible and held:
                    emit(held)
                if active and candidate:
                    continue
            else:
                with lock:
                    method, sid, before = pending.pop(f.get("id"), (None, None, None))
                    held, count = turns.pop(sid, (None, 0)) if method == "session/prompt" else (None, 0)
                    if method == "session/close":
                        turns.pop(sid, None)
                        for rpc_id, entry in list(pending.items()):
                            if entry[:2] == ("session/prompt", sid):
                                del pending[rpc_id]
                result = f.get("result")
                if backend == "frida" and method == "session/new" and isinstance(result, dict) and result.get("sessionId"):
                    attach_workers()
                if method == "session/prompt" and isinstance(result, dict):
                    # Strip the private field before introducing a local observation.
                    changed = any(key in result for key in (FIELD, STATE, RETRY, SOURCE))
                    for key in (FIELD, STATE, RETRY, SOURCE):
                        result.pop(key, None)
                    if held and result.get("stopReason") == "end_turn":
                        metadata = confirmed(sid, held, count, before)
                        if metadata:
                            result.update(metadata)
                            changed = True
                            held = None
                    if changed:
                        line = json.dumps(f, separators=(",", ":")).encode() + b"\n"
                if held:
                    emit(held)
            emit(line)
    finally:
        if router.poll() is None:
            router.send_signal(signal.SIGTERM)
        router.wait()


if __name__ == "__main__":
    main()
