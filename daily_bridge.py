#!/usr/bin/env python3
import argparse
import base64
import json
import os
import signal
import sys
import threading
import time

from daily import (
    CallClient,
    CustomAudioSource,
    CustomAudioTrack,
    Daily,
    EventHandler,
)
from daily import LogLevel as DailyLogLevel


AUDIO_OUT_SAMPLE_RATE = 24000
AUDIO_IN_SAMPLE_RATE = 16000
CHANNELS = 1


stdout_lock = threading.Lock()


def perf_diagnostics_enabled():
    value = os.getenv("PERF_DIAGNOSTICS_ENABLED")
    if value is not None:
        return value.strip() == "1"
    return os.getenv("PYROSCOPE_ENABLED", "").strip() == "1"


PERF_DIAGNOSTICS_ENABLED = perf_diagnostics_enabled()


class TimingAggregator:
    def __init__(self):
        self.lock = threading.Lock()
        self.stats = {}

    def record(self, name, elapsed_seconds):
        elapsed_ms = max(0.0, elapsed_seconds * 1000.0)
        with self.lock:
            stat = self.stats.setdefault(
                name,
                {"count": 0, "total_ms": 0.0, "max_ms": 0.0},
            )
            stat["count"] += 1
            stat["total_ms"] += elapsed_ms
            if elapsed_ms > stat["max_ms"]:
                stat["max_ms"] = elapsed_ms

    def snapshot_and_reset(self):
        with self.lock:
            stats = self.stats
            self.stats = {}
        entries = []
        for name, stat in sorted(stats.items()):
            count = stat["count"]
            if count <= 0:
                continue
            entries.append(
                {
                    "name": name,
                    "count": count,
                    "avg_ms": round(stat["total_ms"] / count, 3),
                    "max_ms": round(stat["max_ms"], 3),
                    "total_ms": round(stat["total_ms"], 3),
                }
            )
        return entries


timings = TimingAggregator() if PERF_DIAGNOSTICS_ENABLED else None


def emit(event, **data):
    payload = {"event": event, **data}
    with stdout_lock:
        sys.stdout.write(json.dumps(payload, separators=(",", ":")) + "\n")
        sys.stdout.flush()


def log(message):
    print(message, file=sys.stderr, flush=True)


def configure_pyroscope():
    if not PERF_DIAGNOSTICS_ENABLED:
        log("performance diagnostics disabled")
        return

    server_address = os.getenv("PYROSCOPE_SERVER_ADDRESS", "").strip()
    basic_auth_username = os.getenv("PYROSCOPE_BASIC_AUTH_USER", "").strip()
    basic_auth_password = os.getenv("PYROSCOPE_BASIC_AUTH_PASSWORD", "").strip()
    if not server_address or not basic_auth_username or not basic_auth_password:
        log("pyroscope disabled: missing server address or basic auth credentials")
        return
    try:
        sample_rate = int(os.getenv("PYROSCOPE_PYTHON_SAMPLE_RATE", "100"))
    except ValueError:
        sample_rate = 100

    try:
        import pyroscope
    except Exception as exc:
        log(f"pyroscope import failed: {exc}")
        return

    application_name = os.getenv(
        "PYROSCOPE_PYTHON_APPLICATION_NAME",
        "talk-go.daily_bridge.python",
    ).strip()
    tags = {
        "environment": os.getenv("PYROSCOPE_ENVIRONMENT")
        or os.getenv("ENVIRONMENT")
        or os.getenv("SENTRY_ENVIRONMENT")
        or "",
        "deployment": os.getenv("GKE_DEPLOYMENT_NAME") or os.getenv("FLY_APP_NAME") or "",
        "pod": os.getenv("HOSTNAME") or "",
        "node": os.getenv("NODE_NAME") or "",
        "process": "daily-bridge",
        "language": "python",
    }
    tags = {key: value for key, value in tags.items() if value}
    kwargs = {
        "application_name": application_name,
        "server_address": server_address,
        "basic_auth_username": basic_auth_username,
        "basic_auth_password": basic_auth_password,
        "sample_rate": sample_rate,
        "oncpu": True,
        "gil_only": os.getenv("PYROSCOPE_PYTHON_GIL_ONLY", "0").strip() == "1",
        "enable_logging": False,
        "tags": tags,
    }
    tenant_id = os.getenv("PYROSCOPE_TENANT_ID", "").strip()
    if tenant_id:
        kwargs["tenant_id"] = tenant_id
    try:
        pyroscope.configure(**kwargs)
    except Exception as exc:
        log(f"pyroscope configure failed: {exc}")
        return
    log(f"pyroscope enabled: application={application_name}")


def participant_id(participant):
    if not isinstance(participant, dict):
        return ""
    return participant.get("id") or participant.get("session_id") or participant.get("user_id") or ""


class BridgeHandler(EventHandler):
    def __init__(self):
        super().__init__()
        self.client = None
        self.local_id = ""
        self.captured = set()
        self.lock = threading.Lock()

    def set_client(self, client):
        self.client = client

    def set_local_id(self, local_id):
        self.local_id = local_id or ""

    def capture_participant(self, participant):
        pid = participant_id(participant)
        if not pid or pid == self.local_id:
            return
        with self.lock:
            if pid in self.captured:
                return
            self.captured.add(pid)
        try:
            self.client.update_subscriptions(
                {pid: {"media": {"microphone": "subscribed"}}},
                completion=lambda err: log(f"subscription update error for {pid}: {err}") if err else None,
            )
            self.client.set_audio_renderer(
                pid,
                self.on_audio_data,
                audio_source="microphone",
                sample_rate=AUDIO_IN_SAMPLE_RATE,
                callback_interval_ms=20,
            )
        except Exception as exc:
            emit("error", message=f"failed to capture participant audio: {exc}")

    def on_audio_data(self, participant_id_value, audio, audio_source):
        try:
            if PERF_DIAGNOSTICS_ENABLED:
                start = time.perf_counter()
                audio_bytes = bytes(audio.audio_frames)
                timings.record("py_daily_audio_bytes_copy", time.perf_counter() - start)
                start = time.perf_counter()
                data = base64.b64encode(audio_bytes).decode("ascii")
                timings.record("py_daily_audio_base64_encode", time.perf_counter() - start)
                start = time.perf_counter()
            else:
                audio_bytes = bytes(audio.audio_frames)
                data = base64.b64encode(audio_bytes).decode("ascii")
            emit(
                "audio",
                participant_id=participant_id_value,
                sample_rate=audio.sample_rate,
                channels=audio.num_channels,
                data=data,
            )
            if PERF_DIAGNOSTICS_ENABLED:
                timings.record("py_daily_audio_emit_stdout", time.perf_counter() - start)
        except Exception as exc:
            log(f"audio callback error: {exc}")

    def on_participant_joined(self, participant):
        pid = participant_id(participant)
        if pid and pid != self.local_id:
            emit("participant_joined", participant_id=pid)
            self.capture_participant(participant)

    def on_participant_left(self, participant, reason):
        pid = participant_id(participant)
        if pid and pid != self.local_id:
            emit("participant_left", participant_id=pid, reason=reason or "")

    def on_app_message(self, message, sender):
        emit("app_message", sender=sender or "", message=message)

    def on_error(self, message):
        emit("error", message=str(message))


def join_room(args, handler, client, audio_track):
    joined = threading.Event()
    result = {"data": None, "error": None}

    def on_joined(data, error):
        result["data"] = data
        result["error"] = error
        joined.set()

    client.update_subscription_profiles(
        {"base": {"camera": "unsubscribed", "screenVideo": "unsubscribed"}}
    )
    client.set_user_name(args.bot_name)
    client.join(
        args.room_url,
        meeting_token=args.token or None,
        completion=on_joined,
        client_settings={
            "inputs": {
                "camera": {"isEnabled": False},
                "microphone": {
                    "isEnabled": True,
                    "settings": {"customTrack": {"id": audio_track.id}},
                },
            },
            "publishing": {
                "camera": {
                    "sendSettings": {
                        "maxQuality": "low",
                        "encodings": {"low": {"maxBitrate": 1, "maxFramerate": 1}},
                    }
                },
                "microphone": {
                    "sendSettings": {
                        "channelConfig": "mono",
                        "bitrate": 64000,
                    }
                },
            },
        },
    )

    if not joined.wait(args.join_timeout_seconds):
        emit("error", message="Daily join timed out")
        return False
    if result["error"]:
        emit("error", message=f"Daily join failed: {result['error']}")
        return False

    data = result["data"] or {}
    local = data.get("participants", {}).get("local", {})
    local_id = participant_id(local)
    handler.set_local_id(local_id)
    emit(
        "joined",
        participant_id=local_id,
        meeting_id=data.get("meetingSession", {}).get("id") or "",
    )

    for participant in (client.participants() or {}).values():
        handler.capture_participant(participant)
        pid = participant_id(participant)
        if pid and pid != local_id:
            emit("participant_joined", participant_id=pid)
    return True


def stdin_loop(client, audio_source, stop_event):
    for line in sys.stdin:
        if stop_event.is_set():
            break
        try:
            if PERF_DIAGNOSTICS_ENABLED:
                start = time.perf_counter()
                command = json.loads(line)
                timings.record("py_stdin_json_loads", time.perf_counter() - start)
            else:
                command = json.loads(line)
        except json.JSONDecodeError:
            continue
        command_type = command.get("type")
        if command_type == "audio":
            if PERF_DIAGNOSTICS_ENABLED:
                start = time.perf_counter()
                raw = base64.b64decode(command.get("data") or "")
                timings.record("py_stdin_audio_base64_decode", time.perf_counter() - start)
                start = time.perf_counter()
                audio_source.write_frames(raw)
                timings.record("py_stdin_audio_write_frames", time.perf_counter() - start)
            else:
                raw = base64.b64decode(command.get("data") or "")
                audio_source.write_frames(raw)
        elif command_type == "message":
            client.send_app_message(command.get("data"), None)
        elif command_type == "leave":
            break


def timing_loop(stop_event, interval_seconds=10):
    if not PERF_DIAGNOSTICS_ENABLED or timings is None:
        return
    while not stop_event.wait(interval_seconds):
        entries = timings.snapshot_and_reset()
        if entries:
            log("audio_timing " + json.dumps({"event": "audio_timing", "timings": entries}, separators=(",", ":")))


def leave(client):
    left = threading.Event()

    def on_left(error):
        if error:
            log(f"Daily leave error: {error}")
        left.set()

    try:
        client.leave(completion=on_left)
        left.wait(5)
    except Exception as exc:
        log(f"Daily leave exception: {exc}")
    try:
        client.release()
    except Exception as exc:
        log(f"Daily release exception: {exc}")
    emit("left")


def main():
    configure_pyroscope()

    parser = argparse.ArgumentParser()
    parser.add_argument("--room-url", required=True)
    parser.add_argument("--token", default="")
    parser.add_argument("--bot-name", default="Chatbot")
    parser.add_argument("--join-timeout-seconds", type=float, default=15)
    args = parser.parse_args()

    Daily.init(log_level=DailyLogLevel.Off)
    audio_source = CustomAudioSource(AUDIO_OUT_SAMPLE_RATE, CHANNELS, True)
    audio_track = CustomAudioTrack(audio_source)
    handler = BridgeHandler()
    client = CallClient(event_handler=handler)
    handler.set_client(client)
    stop_event = threading.Event()

    def handle_signal(signum, frame):
        stop_event.set()

    signal.signal(signal.SIGTERM, handle_signal)
    signal.signal(signal.SIGINT, handle_signal)

    if PERF_DIAGNOSTICS_ENABLED:
        timing_thread = threading.Thread(target=timing_loop, args=(stop_event,))
        timing_thread.daemon = True
        timing_thread.start()

    if join_room(args, handler, client, audio_track):
        stdin_thread = threading.Thread(target=stdin_loop, args=(client, audio_source, stop_event))
        stdin_thread.daemon = True
        stdin_thread.start()
        while stdin_thread.is_alive() and not stop_event.is_set():
            stdin_thread.join(timeout=0.2)

    stop_event.set()
    if PERF_DIAGNOSTICS_ENABLED and timings is not None:
        final_entries = timings.snapshot_and_reset()
        if final_entries:
            log("audio_timing " + json.dumps({"event": "audio_timing", "timings": final_entries}, separators=(",", ":")))
    leave(client)
    time.sleep(0.05)


if __name__ == "__main__":
    main()
