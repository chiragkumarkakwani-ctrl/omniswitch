#!/usr/bin/env python3
"""Exercise official SDKs against the gateway and mock_upstream.py.

Requires openai and anthropic in a virtual environment. The gateway key comes
from OMNISWITCH_COMPAT_API_KEY; never put an actual credential in this file.
"""
import argparse
import json
import os

import anthropic
import openai

SCHEMA = {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"], "additionalProperties": False}
TOOLS = ("get_weather", "get_time")
QUESTION = "What is the weather and local time in Boston?"
ANSWER = "Boston is sunny; local time is noon."


def check_openai(client, model, streaming):
    tools = [{"type": "function", "name": name, "parameters": SCHEMA, "strict": True} for name in TOOLS]

    def create(history, **options):
        parameters = dict(model=model, input=history, max_output_tokens=64, **options)
        if not streaming:
            return client.responses.create(**parameters)
        with client.responses.stream(**parameters) as stream:
            events = list(stream)
            sequences = [event.sequence_number for event in events if hasattr(event, "sequence_number")]
            assert sequences == list(range(len(sequences))), sequences
            assert events[0].type == "response.created"
            assert events[-1].type == "response.completed"
            if options.get("tools"):
                assert any(event.type == "response.function_call_arguments.delta" for event in events)
                assert sum(event.type == "response.function_call_arguments.done" for event in events) == 2
            return stream.get_final_response()

    history = [{"role": "user", "content": QUESTION}]
    first = create(history, tools=tools, tool_choice="required", parallel_tool_calls=True, instructions="Use the available tools.")
    calls = [item for item in first.output if item.type == "function_call"]
    assert len(calls) == 2 and {call.name for call in calls} == set(TOOLS)
    assert first.id.startswith("resp_") and first.status == "completed"
    assert first.usage.input_tokens == 11 and first.usage.output_tokens == 7 and first.usage.total_tokens == 18
    history.extend(item.model_dump(mode="json") for item in first.output)
    for call in calls:
        assert json.loads(call.arguments) == {"city": "Boston"}
        history.append({"type": "function_call_output", "call_id": call.call_id, "output": "Sunny" if call.name == "get_weather" else "Noon"})
    final = create(history)
    assert final.output_text == ANSWER, final.output_text
    assert final.usage.total_tokens == 18
    return {"api": "responses", "mode": "streaming" if streaming else "blocking", "turns": 2, "tool_calls": len(calls), "result": "PASS"}


def check_anthropic(client, model, streaming):
    tools = [{"name": name, "input_schema": SCHEMA} for name in TOOLS]

    def create(history, **options):
        parameters = dict(model=model, max_tokens=64, messages=history, **options)
        if not streaming:
            return client.messages.create(**parameters)
        with client.messages.stream(**parameters) as stream:
            events = list(stream)
            assert events[0].type == "message_start" and events[-1].type == "message_stop"
            if options.get("tools"):
                assert any(event.type == "content_block_delta" and event.delta.type == "input_json_delta" for event in events)
            return stream.get_final_message()

    history = [{"role": "user", "content": QUESTION}]
    first = create(history, tools=tools, tool_choice={"type": "any", "disable_parallel_tool_use": False}, system=[{"type": "text", "text": "Use tools."}], stop_sequences=["END"], extra_body={"top_k": 10})
    calls = [block for block in first.content if block.type == "tool_use"]
    assert len(calls) == 2 and {call.name for call in calls} == set(TOOLS)
    assert first.id.startswith("msg_") and first.stop_reason == "tool_use"
    assert first.usage.input_tokens == 11 and first.usage.output_tokens == 7
    history.append({"role": "assistant", "content": [block.model_dump(mode="json") for block in first.content]})
    results = []
    for call in calls:
        assert call.input == {"city": "Boston"}, call.input
        results.append({"type": "tool_result", "tool_use_id": call.id, "content": "Sunny" if call.name == "get_weather" else "Noon"})
    history.append({"role": "user", "content": results})
    final = create(history)
    assert final.stop_reason == "end_turn"
    assert "".join(block.text for block in final.content if block.type == "text") == ANSWER
    assert final.usage.input_tokens == 11 and final.usage.output_tokens == 7
    return {"api": "messages", "mode": "streaming" if streaming else "blocking", "turns": 2, "tool_calls": len(calls), "result": "PASS"}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--gateway-url", default="http://127.0.0.1:18150")
    parser.add_argument("--model", default="test-model")
    args = parser.parse_args()
    key = os.environ["OMNISWITCH_COMPAT_API_KEY"]
    with openai.OpenAI(api_key=key, base_url=args.gateway_url.rstrip("/") + "/v1", max_retries=0) as oc, anthropic.Anthropic(api_key=key, base_url=args.gateway_url, max_retries=0) as ac:
        results = []
        for streaming in (False, True):
            results.append(check_openai(oc, args.model, streaming))
            results.append(check_anthropic(ac, args.model, streaming))
        try:
            oc.responses.create(model=args.model, input="Hi", previous_response_id="resp_old")
            raise AssertionError("previous_response_id must not succeed")
        except openai.BadRequestError as error:
            assert error.status_code == 400 and error.code == "unsupported_parameter"
        for streaming in (False, True):
            try:
                ac.messages.create(model=args.model, max_tokens=64, messages=[{"role": "user", "content": "malformed"}], stream=streaming)
                raise AssertionError("malformed arguments must return 502 with default stream buffering")
            except anthropic.InternalServerError as error:
                assert error.status_code == 502 and error.body["error"]["type"] == "api_error", error.body
    print(json.dumps({"openai": openai.__version__, "anthropic": anthropic.__version__, "results": results, "negative_checks": "PASS: previous_response_id 400; malformed arguments 502 in both modes"}, indent=2))


if __name__ == "__main__":
    main()
