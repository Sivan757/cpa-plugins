package main

import "strings"

// sanitizeToolPairing makes the tool-call conversation visible to the upstream's
// pairing validator.
//
// The strict model families treat a message whose content is null, or whose
// keys are missing, as "not present", and then reject the request with
// "Messages with role 'tool' must be a response to a preceding message with
// 'tool_calls'". Four repairs make a client-shaped history acceptable without
// changing its meaning:
//
//   - a null content becomes an empty string,
//   - a tool result with no preceding tool_calls gets a synthetic assistant
//     stub declaring that call,
//   - an assistant message that declares calls but has no result gets a
//     placeholder tool result,
//   - a duplicated result for one call id is dropped.
func sanitizeToolPairing(messages []any) []any {
	out := make([]any, 0, len(messages))
	// answered tracks tool call ids that already have a result behind them.
	answered := map[string]bool{}
	// declared tracks call ids seen in an assistant tool_calls array.
	declared := map[string]bool{}

	appendAssistantStub := func(callID string) {
		out = append(out, map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":       callID,
				"type":     "function",
				"function": map[string]any{"name": "", "arguments": "{}"},
			}},
		})
	}

	for _, entry := range messages {
		message, okMessage := entry.(map[string]any)
		if !okMessage {
			out = append(out, entry)
			continue
		}
		role, _ := message["role"].(string)

		// The upstream rejects the developer role during deserialisation.
		if strings.EqualFold(role, "developer") {
			message["role"] = "system"
			role = "system"
		}
		// A null content hides the message from the pairing validator.
		if content, present := message["content"]; present && content == nil {
			message["content"] = ""
		}

		switch role {
		case "assistant":
			if calls, okCalls := message["tool_calls"].([]any); okCalls {
				for _, rawCall := range calls {
					call, okCall := rawCall.(map[string]any)
					if !okCall {
						continue
					}
					if id, _ := call["id"].(string); strings.TrimSpace(id) != "" {
						declared[id] = true
						delete(answered, id)
					}
				}
			}
			out = append(out, message)

		case "tool":
			callID, _ := message["tool_call_id"].(string)
			if callID != "" {
				if answered[callID] {
					// A second result for the same call is dropped rather than
					// forwarded, because the validator counts it as an orphan.
					continue
				}
				if !declared[callID] {
					appendAssistantStub(callID)
					declared[callID] = true
				}
				answered[callID] = true
			} else if !hasPrecedingToolCalls(out) {
				// No call id and nothing to answer: the message cannot be
				// paired at all, so it is forwarded as a plain user note.
				message["role"] = "user"
			}
			out = append(out, message)

		default:
			out = append(out, message)
		}
	}

	// An assistant message that declared calls but never got a result would
	// leave the conversation unanswerable; a placeholder closes the pair.
	out = closeUnansweredCalls(out, declared, answered)
	return out
}

// hasPrecedingToolCalls reports whether anything already emitted declares a
// tool call.
func hasPrecedingToolCalls(emitted []any) bool {
	for _, entry := range emitted {
		message, okMessage := entry.(map[string]any)
		if !okMessage {
			continue
		}
		if role, _ := message["role"].(string); role == "assistant" {
			if calls, okCalls := message["tool_calls"].([]any); okCalls && len(calls) > 0 {
				return true
			}
		}
	}
	return false
}

// closeUnansweredCalls appends placeholder results for declared-but-unanswered
// call ids.
func closeUnansweredCalls(messages []any, declared, answered map[string]bool) []any {
	missing := make([]string, 0)
	for id := range declared {
		if !answered[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return messages
	}
	for _, id := range missing {
		messages = append(messages, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"content":      "(no result was produced)",
		})
	}
	return messages
}
