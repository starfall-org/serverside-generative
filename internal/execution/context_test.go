package execution

import (
	"strings"
	"testing"
)

func TestClientContextValidationAndSelectiveApplication(t *testing.T) {
	ctx := ClientContext{
		Instructions: "Prefer short Vietnamese answers.",
		Skills: []AgentSkill{
			{ID: "summarize", Name: "Summarizer", Content: "# Summarizer\nSummarize documents."},
			{ID: "search", Name: "Search", Content: "# Search\nSearch external websites."},
		},
	}
	if err := ctx.Validate(); err != nil {
		t.Fatal(err)
	}
	state := State{
		Provider:         Provider{Platform: "openai-chat", APIKey: "example-token", Model: "example-model"},
		Conversation:     []Message{{Role: "user", Content: "Hello"}},
		SelectedSkillIDs: []string{"summarize"},
	}
	merged, err := state.WithClientContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Conversation) != 3 {
		t.Fatalf("expected instructions, one selected skill, original prompt; got %+v", merged.Conversation)
	}
	if merged.Conversation[0].Role != "system" || merged.Conversation[0].Content != ctx.Instructions {
		t.Fatal("instructions not prepended")
	}
	if !strings.Contains(merged.Conversation[1].Content, "Summarize documents.") {
		t.Fatal("selected skill missing")
	}
	if strings.Contains(merged.Conversation[1].Content, "Search external websites.") {
		t.Fatal("unselected skill leaked")
	}
	if len(state.Conversation) != 1 {
		t.Fatal("input state modified")
	}
	if err := merged.ValidateModel(); err != nil {
		t.Fatal(err)
	}
	state.SelectedSkillIDs = nil
	noSelection, err := state.WithClientContext(ctx)
	if err != nil || len(noSelection.Conversation) != 2 {
		t.Fatal("unselected skills must not be injected")
	}
	state.SelectedSkillIDs = []string{"not-installed"}
	if _, err := state.WithClientContext(ctx); err == nil {
		t.Fatal("missing skill was silently accepted")
	}
	state.SelectedSkillIDs = []string{"summarize", "summarize"}
	if _, err := state.WithClientContext(ctx); err == nil {
		t.Fatal("duplicate selected skill was accepted")
	}
}

func TestClientContextValidationLimits(t *testing.T) {
	for _, ctx := range []ClientContext{
		{Instructions: strings.Repeat("x", MaxInstructionsSize+1)},
		{Instructions: "hello\x00world"},
		{Skills: []AgentSkill{{ID: "../../secret", Name: "bad", Content: "x"}}},
		{Skills: []AgentSkill{{ID: "valid", Name: "Valid", Content: strings.Repeat("x", MaxSkillContentSize+1)}}},
		{Skills: []AgentSkill{{ID: "repeat", Name: "A", Content: "x"}, {ID: "repeat", Name: "B", Content: "y"}}},
	} {
		if err := ctx.Validate(); err == nil {
			t.Fatalf("accepted invalid context: %+v", ctx)
		}
	}
}
