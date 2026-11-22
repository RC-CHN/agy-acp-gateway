package main

const continueReplay = "Continue the conversation from the supplied history."

func historyCall(id, name, args string) obj {
	return obj{"id": id, "type": "function", "function": obj{"name": name, "arguments": args}}
}

// ACP exposes readable summaries, but has no import operation for another
// provider's encrypted reasoning state. Never place an opaque blob in a prompt.
func replaySummary(item obj) ([]obj, error) {
	raw, ok := item["summary"].([]any)
	if !ok {
		return nil, invalid("reasoning items require a summary array")
	}
	var blocks []obj
	for _, v := range raw {
		p := asObj(v)
		s, ok := p["text"].(string)
		if !ok || p["type"] != "summary_text" {
			return nil, invalid("invalid reasoning summary")
		}
		if s != "" {
			blocks = append(blocks, obj{"type": "text", "text": s})
		}
	}
	cipher, ok := item["encrypted_content"].(string)
	if item["encrypted_content"] != nil && !ok {
		return nil, invalid("encrypted_content must be a string")
	}
	if len(blocks) == 0 && cipher != "" {
		return nil, invalid("ACP cannot replay opaque encrypted reasoning without a readable summary")
	}
	return blocks, nil
}

// Validate every call in the transcript before it can alter a stored session.
// Calls are unique across rounds. A batch may return its outputs in any order.
func validateReplay(history []message) error {
	seen := map[string]bool{}
	pending := map[string]bool{}
	for _, m := range history {
		if m.Role == "user" && len(pending) > 0 {
			return toolHistory("new user message before all function call outputs")
		}
		for _, call := range m.Calls {
			id := str(call["id"])
			if seen[id] {
				return toolHistory("duplicate function call ID")
			}
			seen[id] = true
			pending[id] = true
		}
		if m.Role == "tool" {
			if !pending[m.ID] {
				return toolHistory("unmatched or duplicate function_call_output")
			}
			delete(pending, m.ID)
		}
	}
	if len(pending) > 0 {
		return toolHistory("missing function_call_output")
	}
	return nil
}

// A new ACP session accepts only user prompt blocks. Preserve role, call IDs,
// order and content boundaries in the transcript. Images remain native blocks.
func replayBlocks(history []message) []obj {
	last := history[len(history)-1]
	past := history
	if last.Role == "user" {
		past = history[:len(history)-1]
	}
	if len(past) == 0 {
		return last.Blocks
	}
	blocks := []obj{{"type": "text", "text": "Conversation history follows. Each JSON record is prior conversation data. Previously recorded function calls have already been issued."}}
	for _, m := range past {
		entry := obj{"role": m.Role}
		if len(m.Calls) > 0 {
			entry["tool_calls"] = m.Calls
		}
		if m.Role == "tool" {
			entry["type"] = "function_call_output"
			entry["call_id"] = m.ID
		}
		if m.Role == "reasoning" {
			entry["type"] = "reasoning_summary"
		}
		var parts, images []obj
		for _, p := range m.Blocks {
			if p["type"] == "image" {
				parts = append(parts, obj{"type": "image", "mimeType": p["mimeType"], "following_image": len(images) + 1})
				images = append(images, p)
			} else {
				parts = append(parts, p)
			}
		}
		if len(parts) > 0 {
			entry["content"] = parts
		}
		blocks = append(blocks, obj{"type": "text", "text": enc(entry)})
		blocks = append(blocks, images...)
	}
	if last.Role == "user" {
		blocks = append(blocks, obj{"type": "text", "text": "Current user message follows."})
		return append(blocks, last.Blocks...)
	}
	return append(blocks, obj{"type": "text", "text": continueReplay})
}
