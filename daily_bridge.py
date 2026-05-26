#!/usr/bin/env python3
import argparse
import base64
import json
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


def emit(event, **data):
    payload = {"event": event, **data}
    with stdout_lock:
        sys.stdout.write(json.dumps(payload, separators=(",", ":")) + "\n")
        sys.stdout.flush()


def log(message):
    print(message, file=sys.stderr, flush=True)


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
            emit(
                "audio",
                participant_id=participant_id_value,
                sample_rate=audio.sample_rate,
                channels=audio.num_channels,
                data=base64.b64encode(bytes(audio.audio_frames)).decode("ascii"),
            )
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
            command = json.loads(line)
        except json.JSONDecodeError:
            continue
        command_type = command.get("type")
        if command_type == "audio":
            raw = base64.b64decode(command.get("data") or "")
            audio_source.write_frames(raw)
        elif command_type == "message":
            client.send_app_message(command.get("data"), None)
        elif command_type == "leave":
            break


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

    if join_room(args, handler, client, audio_track):
        stdin_thread = threading.Thread(target=stdin_loop, args=(client, audio_source, stop_event))
        stdin_thread.daemon = True
        stdin_thread.start()
        while stdin_thread.is_alive() and not stop_event.is_set():
            stdin_thread.join(timeout=0.2)

    stop_event.set()
    leave(client)
    time.sleep(0.05)


if __name__ == "__main__":
    main()
