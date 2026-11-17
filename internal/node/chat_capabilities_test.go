package node

import "testing"

func TestChatCapabilitiesDescribeCurrentLimits(t *testing.T) {
	n := &Node{}
	features, ok := n.capabilities()["chat_features"].(map[string]any)
	if !ok {
		t.Fatal("chat features missing")
	}
	for _, name := range []string{"attachments", "multimodal_input", "generation_tool_loop", "reasoning_history"} {
		if features[name] != false {
			t.Fatalf("%s must be unavailable: %v", name, features[name])
		}
	}
	if features["provider_catalog"] != "provider.catalog" || features["model_limits"] != "unknown" || features["list_snapshot_pagination"] != true {
		t.Fatalf("catalog capabilities: %+v", features)
	}
	if features["selected_instructions"] != false || features["runtime_instruction_handoff"] != true {
		t.Fatal("instruction source must be explicit")
	}
	if features["text_generation"] != true || features["streaming"] != true || features["workspace_scope"] != "paired_client" || features["conversation_max_bytes"] != 3<<20 {
		t.Fatalf("unexpected capabilities: %+v", features)
	}
}

func TestNegotiatedAdaptersAndConstraints(t *testing.T) {
	features := (&Node{}).capabilities()["chat_features"].(map[string]any)
	n := features["negotiation"].(map[string]any)
	if n["version"] != 1 || n["catalog_requires_authentication"] != true {
		t.Fatal(n)
	}
	modalities := n["modalities"].(map[string]any)
	if modalities["attachment_upload"] != false {
		t.Fatal(modalities)
	}
	generation := n["generation"].(map[string]any)
	if generation["tool_loop"] != false || generation["max_history_messages"] != 256 {
		t.Fatal(generation)
	}
	adapters := n["adapters"].(map[string]any)
	if len(adapters) != 4 {
		t.Fatal(adapters)
	}
	for _, id := range []string{"openai", "openai_responses", "anthropic", "google"} {
		if _, ok := adapters[id]; !ok {
			t.Fatal(id)
		}
	}
	if n["stored_runtime"].(map[string]any)["credential_values_returned"] != false {
		t.Fatal(n)
	}
}
