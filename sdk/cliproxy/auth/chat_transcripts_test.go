package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetChatStore() {
	chatStore.Lock()
	chatStore.seq = 0
	chatStore.transcripts = make(map[string]*chatTranscript)
	chatStore.Unlock()
}

func chatSessionID(t *testing.T, session string) string {
	t.Helper()
	id, _ := chatSessionKey(claudeCodeOpts(session, `{}`))
	if id == "" {
		t.Fatal("no session key")
	}
	return id
}

func TestChatTranscriptKeepsMessagesAcrossRequests(t *testing.T) {
	resetChatStore()
	now := time.Unix(1_800_000_000, 0)
	first := `{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Fix the login bug"}]}]}`
	rec := recordChatRequest(claudeCodeOpts("chat-1", first), now)
	if rec == nil {
		t.Fatal("request not recorded")
	}
	rec.observeChunk([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
	rec.observeChunk([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Looking \"}}\n\n"))
	rec.observeChunk([]byte("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"at it.\"}}\n\n"))
	rec.observeChunk([]byte("data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"name\":\"Read\"}}\n\n"))
	rec.observeChunk([]byte("data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file\\\":\\\"a.go\\\"}\"}}\n\n"))

	id := chatSessionID(t, "chat-1")
	live, _ := (&Manager{}).ChatTranscript(id)
	if len(live.Messages) != 2 || !live.Messages[1].Live || live.Messages[1].Blocks[0].Text != "Looking at it." {
		t.Fatalf("live transcript = %+v", live.Messages)
	}
	user := live.Messages[0]
	if len(user.Blocks) != 2 || user.Blocks[0].Kind != "context" || user.Blocks[1].Kind != "text" || user.Blocks[1].Text != "Fix the login bug" {
		t.Fatalf("user blocks = %+v", user.Blocks)
	}
	if tool := live.Messages[1].Blocks[1]; tool.Kind != "tool_use" || tool.Name != "Read" || tool.Text != `{"file":"a.go"}` {
		t.Fatalf("tool block = %+v", tool)
	}
	rec.publish(true)
	replySeq := live.Messages[1].Seq

	second := `{"messages":[
		{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Fix the login bug"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"x"},{"type":"text","text":"Looking at it."},{"type":"tool_use","id":"t1","name":"Read","input":{"file":"a.go"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"package a","is_error":false}]}]}`
	recordChatRequest(claudeCodeOpts("chat-1", second), now.Add(time.Minute))
	got, _ := (&Manager{}).ChatTranscript(id)
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v", got.Messages)
	}
	if got.Messages[0].Seq != user.Seq || !got.Messages[0].At.Equal(now) {
		t.Fatalf("user message restamped: %+v", got.Messages[0])
	}
	if got.Messages[1].Seq != replySeq || got.Messages[1].Live {
		t.Fatalf("reply did not keep its stream stamp: %+v", got.Messages[1])
	}
	if got.Messages[2].Blocks[0].Kind != "tool_result" || got.Messages[2].Blocks[0].Text != "package a" {
		t.Fatalf("tool result = %+v", got.Messages[2])
	}

	feed := (&Manager{}).ChatFeed(10, false)
	if len(feed.Items) != 2 || feed.Items[0].Message.Seq != replySeq {
		t.Fatalf("feed = %+v", feed.Items)
	}
	if blocks := feed.Items[1].Message.Blocks; len(blocks) != 1 || blocks[0].Kind != "text" {
		t.Fatalf("feed hides context: %+v", blocks)
	}
}

func TestChatTranscriptIgnoresOneOffSideRequests(t *testing.T) {
	resetChatStore()
	now := time.Unix(1_800_000_000, 0)
	long := `{"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"},{"role":"assistant","content":"d"},{"role":"user","content":"e"},{"role":"assistant","content":"f"}]}`
	side := `{"messages":[{"role":"user","content":"title please"}]}`
	recordChatRequest(claudeCodeOpts("chat-2", long), now)
	if rec := recordChatRequest(claudeCodeOpts("chat-2", side), now); rec != nil {
		t.Fatal("side request replaced the transcript")
	}
	id := chatSessionID(t, "chat-2")
	if got, _ := (&Manager{}).ChatTranscript(id); len(got.Messages) != 6 {
		t.Fatalf("messages = %d", len(got.Messages))
	}
	// Repeated short requests are a compaction: the transcript follows them.
	recordChatRequest(claudeCodeOpts("chat-2", side), now)
	recordChatRequest(claudeCodeOpts("chat-2", side), now)
	if got, _ := (&Manager{}).ChatTranscript(id); len(got.Messages) != 1 {
		t.Fatalf("messages after compaction = %d", len(got.Messages))
	}
}

func TestChatTranscriptReadsResponsesStream(t *testing.T) {
	resetChatStore()
	body := `{"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"rules"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>x</environment_context>"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"Write tests"}]},
		{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"ls\"}","call_id":"c1"},
		{"type":"function_call_output","call_id":"c1","output":"a.go"}]}`
	rec := recordChatRequest(claudeCodeOpts("chat-3", body), time.Now())
	stream := make(chan cliproxyexecutor.StreamChunk, 4)
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`)}
	stream <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","output_index":0,"delta":"Done"}`)}
	close(stream)
	result := teeChatStream(context.Background(), rec, &cliproxyexecutor.StreamResult{Chunks: stream})
	forwarded := 0
	for range result.Chunks {
		forwarded++
	}
	if forwarded != 2 {
		t.Fatalf("forwarded %d chunks", forwarded)
	}

	got, _ := (&Manager{}).ChatTranscript(chatSessionID(t, "chat-3"))
	roles := []string{}
	for _, msg := range got.Messages {
		roles = append(roles, msg.Role)
	}
	want := []string{"system", "user", "user", "assistant", "user", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v", roles)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v", roles)
		}
	}
	last := got.Messages[len(got.Messages)-1]
	if last.Live || last.Blocks[0].Text != "Done" {
		t.Fatalf("reply = %+v", last)
	}
	if got.Messages[1].Blocks[0].Kind != "context" || got.Messages[3].Blocks[0].Name != "shell" {
		t.Fatalf("messages = %+v", got.Messages)
	}
}

func TestChatTranscriptVersionAndTruncation(t *testing.T) {
	resetChatStore()
	long := make([]byte, 0, maxChatTextRunes+100)
	for i := 0; i < maxChatTextRunes+100; i++ {
		long = append(long, 'x')
	}
	recordChatRequest(claudeCodeOpts("chat-4", `{"messages":[{"role":"user","content":"`+string(long)+`"}]}`), time.Now())
	id := chatSessionID(t, "chat-4")
	got, _ := (&Manager{}).ChatTranscript(id)
	if b := got.Messages[0].Blocks[0]; !b.Truncated || len(b.Text) != maxChatTextRunes {
		t.Fatalf("block not truncated: %d", len(b.Text))
	}
	if v := (&Manager{}).ChatTranscriptVersion(id); v != got.Version || v < 1 {
		t.Fatalf("version = %d, transcript %d", v, got.Version)
	}
	if v := (&Manager{}).ChatTranscriptVersion("missing"); v != -1 {
		t.Fatalf("missing version = %d", v)
	}
}
