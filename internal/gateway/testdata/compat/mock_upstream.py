#!/usr/bin/env python3
"""Local OpenAI-compatible upstream for reproducible two-turn SDK checks."""
import argparse
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Upstream(BaseHTTPRequestHandler):
    counter = 0
    counter_lock = threading.Lock()

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_response(404)
            self.end_headers()
            return
        request = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.counter_lock:
            type(self).counter += 1
            response_id = f"chat_sdk_{self.counter}"
        messages = request["messages"]
        final = any(message["role"] == "tool" for message in messages)
        malformed = any(message.get("content") == "malformed" for message in messages)
        if final:
            results = {message["tool_call_id"]: message["content"] for message in messages if message["role"] == "tool"}
            assert results == {"call_weather": "Sunny", "call_time": "Noon"}, results
        usage = {"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
        calls = [
            {"id": "call_weather", "type": "function", "function": {"name": "get_weather", "arguments": '{"city":"Boston"}'}},
            {"id": "call_time", "type": "function", "function": {"name": "get_time", "arguments": '{"city":"Boston"}'}},
        ]
        if malformed:
            calls[0]["function"]["arguments"] = "{"
        content = "Boston is sunny; local time is noon." if final else "I'll check."
        finish = "stop" if final else "tool_calls"
        message = {"role": "assistant", "content": content}
        if not final:
            message["tool_calls"] = calls
        base = {"id": response_id, "created": 1, "model": request["model"]}
        if not request.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({**base, "object": "chat.completion", "choices": [{"index": 0, "message": message, "finish_reason": finish}], "usage": usage}).encode())
            return
        assert request["stream_options"]["include_usage"] is True
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()

        def chunk(delta=None, reason=None, tokens=None):
            event = {**base, "object": "chat.completion.chunk", "choices": [] if delta is None else [{"index": 0, "delta": delta, "finish_reason": reason}]}
            if tokens is not None:
                event["usage"] = tokens
            self.wfile.write(("data: " + json.dumps(event, separators=(",", ":")) + "\n\n").encode())
            self.wfile.flush()

        if final:
            chunk({"role": "assistant", "content": "Boston is sunny; "})
            chunk({"content": "local time is noon."})
        else:
            # Both calls start with metadata only. Call 1 gets arguments before
            # call 0; Messages block starts must still use contiguous indices.
            chunk({"role": "assistant", "content": content, "tool_calls": [{**call, "index": i, "function": {"name": call["function"]["name"], "arguments": ""}} for i, call in enumerate(calls)]})
            if malformed:
                chunk({"tool_calls": [{"index": 0, "function": {"arguments": "{"}}, {"index": 1, "function": {"arguments": "{}"}}]})
            else:
                for index in (1, 0):
                    chunk({"tool_calls": [{"index": index, "function": {"arguments": '{"city":'}}]})
                for index in (1, 0):
                    chunk({"tool_calls": [{"index": index, "function": {"arguments": '"Boston"}'}}]})
        chunk({}, finish)
        chunk(tokens=usage)  # Usage intentionally follows the finish chunk.
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def log_message(self, *_):
        pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=18151)
    args = parser.parse_args()
    with ThreadingHTTPServer(("127.0.0.1", args.port), Upstream) as server:
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
