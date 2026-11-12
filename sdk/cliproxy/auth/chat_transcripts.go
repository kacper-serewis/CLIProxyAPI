package auth

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

// Chat transcripts: the conversation of each tracked session, rebuilt from the history every
// request carries plus the reply streamed back, so the management panel can show what a chat
// is doing. Kept in memory only (they hold whatever was typed or read in a chat) and bounded;
// a proxy restart starts empty, like the routing activity.

const (
	maxChatTranscripts    = 40
	maxChatMessages       = 400
	maxChatFingerprints   = 4000
	maxChatTextRunes      = 12000
	maxChatThinkingRunes  = 4000
	maxChatToolInputRunes = 1500
	maxChatToolOutRunes   = 2500
	maxFeedBlockRunes     = 1200
	// A request with far fewer messages than the transcript is usually a side request on the
	// same session (title, summary); it replaces the transcript only once a few in a row do,
	// which is what a compaction looks like.
	chatShrinkTolerance = 3
)

// ChatBlock is one piece of a chat message.
type ChatBlock struct {
	// Kind is text, thinking, tool_use, tool_result, image or context (system reminders,
	// environment and instruction blocks the client adds).
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Name      string `json:"name,omitempty"`
	Error     bool   `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ChatMessage is one turn of a chat.
type ChatMessage struct {
	Seq    int64       `json:"seq"`
	Role   string      `json:"role"`
	At     time.Time   `json:"at"`
	Blocks []ChatBlock `json:"blocks"`
	// Live is set while the reply is still streaming.
	Live bool `json:"live,omitempty"`
}

type chatStamp struct {
	seq int64
	at  time.Time
}

type chatTranscript struct {
	messages []ChatMessage
	omitted  int
	// pending is the reply streamed after the last request, until a request carries it.
	pending *ChatMessage
	// stamps remembers when each message was first seen, so a message keeps its place in
	// the feed when a later request repeats it.
	stamps     map[string][]chatStamp
	stampOrder []string
	shrinks    int
	version    int64
	updated    time.Time
}

var chatStore = struct {
	sync.Mutex
	seq         int64
	transcripts map[string]*chatTranscript
}{transcripts: make(map[string]*chatTranscript)}

// chatSessionKey is the session identity the routing activity tracks, or "" when the
// request has none.
func chatSessionKey(opts cliproxyexecutor.Options) (string, cliproxysession.SessionInfo) {
	info, ok := cliproxysession.ExtractSessionInfo(opts.Headers, opts.OriginalRequest, opts.Metadata)
	id := ""
	if ok {
		id = info.SessionID
	}
	if id == "" {
		if lcp, _ := opts.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string); lcp != "" {
			id = lcp
		}
	}
	if id == "" {
		return "", info
	}
	return cliproxysession.BoundSessionIdentity(id), info
}

// chatReplyRecorder collects the reply to one request into the session's pending message.
type chatReplyRecorder struct {
	session string
	msg     *ChatMessage
	// blockIndex maps a stream's content index to its block.
	blockIndex map[int64]int
}

// recordChatRequest rebuilds the session's transcript from a request. It returns a
// recorder for the reply, or nil when the request is not followed.
func recordChatRequest(opts cliproxyexecutor.Options, now time.Time) *chatReplyRecorder {
	id, info := chatSessionKey(opts)
	if id == "" || info.IsCompaction || len(opts.OriginalRequest) == 0 {
		return nil
	}
	messages, total := parseChatRequest(opts.OriginalRequest)
	if total == 0 {
		return nil
	}

	chatStore.Lock()
	defer chatStore.Unlock()
	t := chatStore.transcripts[id]
	if t == nil {
		t = &chatTranscript{stamps: make(map[string][]chatStamp)}
		chatStore.transcripts[id] = t
		evictChatTranscriptsLocked()
	}
	known := len(t.messages) + t.omitted
	if known >= 6 && total < known/2 {
		t.shrinks++
		if t.shrinks < chatShrinkTolerance {
			return nil
		}
	}
	t.shrinks = 0

	// Requests carry the messages their client already showed; a message seen before keeps
	// its sequence number and time, the reply streamed last keeps those of its stream.
	claimed := make(map[string]int)
	pending := t.pending
	for i := range messages {
		fp := chatFingerprint(messages[i])
		stamps := t.stamps[fp]
		if n := claimed[fp]; n < len(stamps) {
			messages[i].Seq, messages[i].At = stamps[n].seq, stamps[n].at
			claimed[fp] = n + 1
			continue
		}
		if pending != nil && messages[i].Role == "assistant" {
			messages[i].Seq, messages[i].At = pending.Seq, pending.At
			pending = nil
		} else {
			chatStore.seq++
			messages[i].Seq, messages[i].At = chatStore.seq, now
		}
		t.remember(fp, chatStamp{messages[i].Seq, messages[i].At})
		claimed[fp]++
	}
	t.messages = messages
	t.omitted = total - len(messages)
	t.pending = nil
	t.version++
	t.updated = now

	chatStore.seq++
	return &chatReplyRecorder{
		session:    id,
		msg:        &ChatMessage{Seq: chatStore.seq, Role: "assistant", At: now, Live: true},
		blockIndex: make(map[int64]int),
	}
}

func (t *chatTranscript) remember(fp string, stamp chatStamp) {
	t.stamps[fp] = append(t.stamps[fp], stamp)
	t.stampOrder = append(t.stampOrder, fp)
	for len(t.stampOrder) > maxChatFingerprints {
		old := t.stampOrder[0]
		t.stampOrder = t.stampOrder[1:]
		if list := t.stamps[old]; len(list) > 1 {
			t.stamps[old] = list[1:]
		} else {
			delete(t.stamps, old)
		}
	}
}

func evictChatTranscriptsLocked() {
	for len(chatStore.transcripts) > maxChatTranscripts {
		oldestID := ""
		var oldest time.Time
		for id, t := range chatStore.transcripts {
			if oldestID == "" || t.updated.Before(oldest) {
				oldestID, oldest = id, t.updated
			}
		}
		delete(chatStore.transcripts, oldestID)
	}
}

func chatFingerprint(msg ChatMessage) string {
	var b strings.Builder
	b.WriteString(msg.Role)
	for _, block := range msg.Blocks {
		if block.Kind == "thinking" || block.Kind == "context" {
			continue // clients drop or rewrite these between requests
		}
		b.WriteString("|")
		b.WriteString(block.Kind)
		b.WriteString(":")
		b.WriteString(block.Name)
		b.WriteString(":")
		text := []rune(strings.Join(strings.Fields(block.Text), " "))
		if len(text) > 160 {
			text = text[:160]
		}
		b.WriteString(string(text))
	}
	return b.String()
}

// publish copies the reply into the transcript as its pending message.
func (r *chatReplyRecorder) publish(final bool) {
	if r == nil {
		return
	}
	chatStore.Lock()
	defer chatStore.Unlock()
	t := chatStore.transcripts[r.session]
	if t == nil {
		return
	}
	if final {
		r.msg.Live = false
	}
	if len(r.msg.Blocks) == 0 && final {
		if t.pending != nil && t.pending.Seq == r.msg.Seq {
			t.pending = nil
			t.version++
		}
		return
	}
	copied := *r.msg
	copied.Blocks = append([]ChatBlock(nil), r.msg.Blocks...)
	t.pending = &copied
	t.version++
	t.updated = time.Now()
}

// observeResponse records a complete (non-streamed) reply.
func (r *chatReplyRecorder) observeResponse(payload []byte) {
	if r == nil {
		return
	}
	root := ojson.ParseBytes(payload)
	switch {
	case root.Get("content").IsArray(): // Anthropic messages
		r.msg.Blocks = parseAnthropicBlocks(root.Get("content"), "assistant")
	case root.Get("output").IsArray(): // OpenAI Responses
		for _, item := range root.Get("output").Array() {
			if _, blocks := parseResponsesItem(item); len(blocks) > 0 {
				r.msg.Blocks = append(r.msg.Blocks, blocks...)
			}
		}
	case root.Get("choices.0.message").Exists(): // OpenAI chat
		r.msg.Blocks = parseChatCompletionMessage(root.Get("choices.0.message"))
	}
	r.publish(true)
}

// observeChunk folds one streamed chunk (one or more SSE events) into the reply.
func (r *chatReplyRecorder) observeChunk(payload []byte) {
	if r == nil || len(payload) == 0 {
		return
	}
	changed := false
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		if r.observeEvent(ojson.Parse(line)) {
			changed = true
		}
	}
	if changed {
		r.publish(false)
	}
}

func (r *chatReplyRecorder) observeEvent(ev ojson.Result) bool {
	switch ev.Get("type").String() {
	// Anthropic messages stream.
	case "content_block_start":
		cb := ev.Get("content_block")
		block := ChatBlock{}
		switch cb.Get("type").String() {
		case "text":
			block.Kind = "text"
		case "thinking", "redacted_thinking":
			block.Kind = "thinking"
		case "tool_use", "server_tool_use", "mcp_tool_use":
			block.Kind, block.Name = "tool_use", cb.Get("name").String()
		default:
			return false
		}
		r.msg.Blocks = append(r.msg.Blocks, block)
		r.blockIndex[ev.Get("index").Int()] = len(r.msg.Blocks) - 1
		return true
	case "content_block_delta":
		i, ok := r.blockIndex[ev.Get("index").Int()]
		if !ok {
			return false
		}
		d := ev.Get("delta")
		switch d.Get("type").String() {
		case "text_delta":
			return r.appendTo(i, d.Get("text").String())
		case "thinking_delta":
			return r.appendTo(i, d.Get("thinking").String())
		case "input_json_delta":
			return r.appendTo(i, d.Get("partial_json").String())
		}
		return false

	// OpenAI Responses stream.
	case "response.output_item.added":
		item := ev.Get("item")
		block := ChatBlock{}
		switch item.Get("type").String() {
		case "message":
			block.Kind = "text"
		case "reasoning":
			block.Kind = "thinking"
		case "function_call", "custom_tool_call":
			block.Kind, block.Name = "tool_use", item.Get("name").String()
		case "local_shell_call", "web_search_call":
			block.Kind, block.Name = "tool_use", item.Get("type").String()
		default:
			return false
		}
		r.msg.Blocks = append(r.msg.Blocks, block)
		r.blockIndex[ev.Get("output_index").Int()] = len(r.msg.Blocks) - 1
		return true
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		i, ok := r.blockIndex[ev.Get("output_index").Int()]
		if !ok {
			return false
		}
		return r.appendTo(i, ev.Get("delta").String())
	case "response.output_item.done":
		// Items like local_shell_call carry their content only when done.
		i, ok := r.blockIndex[ev.Get("output_index").Int()]
		if !ok || r.msg.Blocks[i].Text != "" {
			return false
		}
		if _, blocks := parseResponsesItem(ev.Get("item")); len(blocks) == 1 {
			r.msg.Blocks[i] = blocks[0]
			return true
		}
		return false
	}

	// OpenAI chat completions stream.
	if delta := ev.Get("choices.0.delta"); delta.Exists() {
		changed := false
		if text := delta.Get("reasoning_content").String(); text != "" {
			changed = r.appendKind("thinking", "", text) || changed
		}
		if text := delta.Get("content").String(); text != "" {
			changed = r.appendKind("text", "", text) || changed
		}
		delta.Get("tool_calls").ForEach(func(_, call ojson.Result) bool {
			if name := call.Get("function.name").String(); name != "" {
				r.msg.Blocks = append(r.msg.Blocks, ChatBlock{Kind: "tool_use", Name: name})
				changed = true
			}
			if args := call.Get("function.arguments").String(); args != "" {
				changed = r.appendKind("tool_use", "", args) || changed
			}
			return true
		})
		return changed
	}
	return false
}

func (r *chatReplyRecorder) appendKind(kind, name, text string) bool {
	n := len(r.msg.Blocks)
	if n == 0 || r.msg.Blocks[n-1].Kind != kind {
		r.msg.Blocks = append(r.msg.Blocks, ChatBlock{Kind: kind, Name: name})
		n++
	}
	return r.appendTo(n-1, text)
}

func (r *chatReplyRecorder) appendTo(i int, text string) bool {
	if text == "" || i < 0 || i >= len(r.msg.Blocks) {
		return false
	}
	block := &r.msg.Blocks[i]
	if block.Truncated {
		return false
	}
	limit := chatBlockLimit(block.Kind)
	if len(block.Text)+len(text) > limit*4 || len([]rune(block.Text))+len([]rune(text)) > limit {
		block.Text, block.Truncated = truncateRunes(block.Text+text, limit)
		return true
	}
	block.Text += text
	return true
}

func chatBlockLimit(kind string) int {
	switch kind {
	case "thinking":
		return maxChatThinkingRunes
	case "tool_use":
		return maxChatToolInputRunes
	case "tool_result", "context":
		return maxChatToolOutRunes
	}
	return maxChatTextRunes
}

func truncateRunes(text string, limit int) (string, bool) {
	runes := []rune(text)
	if len(runes) <= limit {
		return text, false
	}
	return string(runes[:limit]), true
}

func newChatBlock(kind, name, text string) ChatBlock {
	text = strings.TrimSpace(text)
	block := ChatBlock{Kind: kind, Name: name}
	block.Text, block.Truncated = truncateRunes(text, chatBlockLimit(kind))
	return block
}

// teeChatStream forwards a stream unchanged while recording the reply.
func teeChatStream(ctx context.Context, rec *chatReplyRecorder, result *cliproxyexecutor.StreamResult) *cliproxyexecutor.StreamResult {
	if rec == nil || result == nil || result.Chunks == nil {
		return result
	}
	in := result.Chunks
	out := make(chan cliproxyexecutor.StreamChunk, cap(in))
	go func() {
		defer close(out)
		defer rec.publish(true)
		for chunk := range in {
			if chunk.Err == nil {
				rec.observeChunk(chunk.Payload)
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				go func() {
					for range in {
					}
				}()
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: result.Headers, Chunks: out}
}

var (
	systemReminderPattern = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
)

// parseChatRequest returns the last maxChatMessages messages of a request and how many it has.
func parseChatRequest(payload []byte) ([]ChatMessage, int) {
	root := ojson.ParseBytes(payload)
	var messages []ChatMessage
	if list := root.Get("messages"); list.IsArray() {
		items := list.Array()
		for _, item := range items {
			role := item.Get("role").String()
			var blocks []ChatBlock
			if role == "system" || role == "developer" {
				blocks = []ChatBlock{newChatBlock("context", role, toolResultText(item.Get("content")))}
				role = "system"
			} else if item.Get("tool_calls").Exists() || role == "tool" {
				blocks = parseChatCompletionMessage(item)
			} else {
				blocks = parseAnthropicBlocks(item.Get("content"), role)
			}
			messages = appendChatMessage(messages, role, blocks)
		}
	} else if list := root.Get("input"); list.IsArray() {
		for _, item := range list.Array() {
			role, blocks := parseResponsesItem(item)
			messages = appendChatMessage(messages, role, blocks)
		}
	} else if text := root.Get("input").String(); text != "" {
		messages = appendChatMessage(messages, "user", []ChatBlock{newChatBlock("text", "", text)})
	}
	total := len(messages)
	if total > maxChatMessages {
		messages = append([]ChatMessage(nil), messages[total-maxChatMessages:]...)
	}
	return messages, total
}

// appendChatMessage adds blocks as a message, merging consecutive items of the same role
// (Responses input lists each tool call and output as its own item).
func appendChatMessage(messages []ChatMessage, role string, blocks []ChatBlock) []ChatMessage {
	if len(blocks) == 0 || role == "" {
		return messages
	}
	if n := len(messages); n > 0 && messages[n-1].Role == role && role != "user" {
		messages[n-1].Blocks = append(messages[n-1].Blocks, blocks...)
		return messages
	}
	return append(messages, ChatMessage{Role: role, Blocks: blocks})
}

// parseUserText splits client-added context (system reminders, environment and
// instruction blocks) from what was typed.
func parseUserText(text string) []ChatBlock {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	if strings.HasPrefix(trimmed, "<") || strings.HasPrefix(trimmed, "# AGENTS.md instructions") {
		return []ChatBlock{newChatBlock("context", "", trimmed)}
	}
	var blocks []ChatBlock
	for _, reminder := range systemReminderPattern.FindAllString(trimmed, -1) {
		blocks = append(blocks, newChatBlock("context", "", reminder))
	}
	if rest := strings.TrimSpace(systemReminderPattern.ReplaceAllString(trimmed, "")); rest != "" {
		blocks = append([]ChatBlock{newChatBlock("text", "", rest)}, blocks...)
	}
	return blocks
}

func parseAnthropicBlocks(content ojson.Result, role string) []ChatBlock {
	textBlocks := func(text string) []ChatBlock {
		if role == "user" {
			return parseUserText(text)
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []ChatBlock{newChatBlock("text", "", text)}
	}
	if content.Type == ojson.String {
		return textBlocks(content.String())
	}
	var blocks []ChatBlock
	content.ForEach(func(_, block ojson.Result) bool {
		switch block.Get("type").String() {
		case "text":
			blocks = append(blocks, textBlocks(block.Get("text").String())...)
		case "thinking":
			if text := block.Get("thinking").String(); strings.TrimSpace(text) != "" {
				blocks = append(blocks, newChatBlock("thinking", "", text))
			}
		case "tool_use", "server_tool_use", "mcp_tool_use":
			blocks = append(blocks, newChatBlock("tool_use", block.Get("name").String(), block.Get("input").Raw))
		case "tool_result", "web_search_tool_result", "mcp_tool_result":
			b := newChatBlock("tool_result", "", toolResultText(block.Get("content")))
			b.Error = block.Get("is_error").Bool()
			blocks = append(blocks, b)
		case "image", "document":
			blocks = append(blocks, ChatBlock{Kind: "image", Name: block.Get("type").String()})
		}
		return true
	})
	return blocks
}

func toolResultText(content ojson.Result) string {
	if content.Type == ojson.String {
		return content.String()
	}
	var parts []string
	content.ForEach(func(_, part ojson.Result) bool {
		switch part.Get("type").String() {
		case "text", "input_text", "output_text":
			parts = append(parts, part.Get("text").String())
		case "image", "input_image":
			parts = append(parts, "[image]")
		}
		return true
	})
	if len(parts) == 0 && content.Exists() {
		return content.Raw
	}
	return strings.Join(parts, "\n")
}

func parseChatCompletionMessage(msg ojson.Result) []ChatBlock {
	var blocks []ChatBlock
	if text := msg.Get("reasoning_content").String(); text != "" {
		blocks = append(blocks, newChatBlock("thinking", "", text))
	}
	if msg.Get("role").String() == "tool" {
		return append(blocks, newChatBlock("tool_result", "", toolResultText(msg.Get("content"))))
	}
	if content := msg.Get("content"); content.Type == ojson.String {
		blocks = append(blocks, parseUserText(content.String())...)
	} else {
		blocks = append(blocks, newChatBlock("text", "", toolResultText(content)))
	}
	msg.Get("tool_calls").ForEach(func(_, call ojson.Result) bool {
		blocks = append(blocks, newChatBlock("tool_use", call.Get("function.name").String(), call.Get("function.arguments").String()))
		return true
	})
	out := blocks[:0]
	for _, b := range blocks {
		if b.Text != "" || b.Kind == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

// parseResponsesItem turns one OpenAI Responses input or output item into a role and blocks.
func parseResponsesItem(item ojson.Result) (string, []ChatBlock) {
	switch item.Get("type").String() {
	case "message", "":
		role := item.Get("role").String()
		content := item.Get("content")
		if role == "system" || role == "developer" {
			return "system", []ChatBlock{newChatBlock("context", role, toolResultText(content))}
		}
		if content.Type == ojson.String {
			return role, parseUserText(content.String())
		}
		var blocks []ChatBlock
		content.ForEach(func(_, part ojson.Result) bool {
			switch part.Get("type").String() {
			case "input_text", "output_text", "text":
				if role == "user" {
					blocks = append(blocks, parseUserText(part.Get("text").String())...)
				} else if text := part.Get("text").String(); strings.TrimSpace(text) != "" {
					blocks = append(blocks, newChatBlock("text", "", text))
				}
			case "input_image":
				blocks = append(blocks, ChatBlock{Kind: "image", Name: "image"})
			}
			return true
		})
		return role, blocks
	case "reasoning":
		var parts []string
		item.Get("summary").ForEach(func(_, part ojson.Result) bool {
			parts = append(parts, part.Get("text").String())
			return true
		})
		if text := strings.TrimSpace(strings.Join(parts, "\n\n")); text != "" {
			return "assistant", []ChatBlock{newChatBlock("thinking", "", text)}
		}
		return "assistant", nil
	case "function_call":
		return "assistant", []ChatBlock{newChatBlock("tool_use", item.Get("name").String(), item.Get("arguments").String())}
	case "custom_tool_call":
		return "assistant", []ChatBlock{newChatBlock("tool_use", item.Get("name").String(), item.Get("input").String())}
	case "local_shell_call":
		return "assistant", []ChatBlock{newChatBlock("tool_use", "shell", item.Get("action").Raw)}
	case "web_search_call":
		return "assistant", []ChatBlock{newChatBlock("tool_use", "web_search", item.Get("action").Raw)}
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return "user", []ChatBlock{newChatBlock("tool_result", "", toolResultText(item.Get("output")))}
	}
	return "", nil
}

// ChatTranscript is a session's conversation as served to the panel.
type ChatTranscript struct {
	Session  *RoutingSession `json:"session,omitempty"`
	Omitted  int             `json:"omitted"`
	Messages []ChatMessage   `json:"messages"`
	Version  int64           `json:"version"`
}

// ChatTranscript returns a session's conversation; ok is false when none is kept.
func (m *Manager) ChatTranscript(id string) (ChatTranscript, bool) {
	session := routingSessionByID(id)
	chatStore.Lock()
	defer chatStore.Unlock()
	t := chatStore.transcripts[id]
	if t == nil {
		return ChatTranscript{Session: session}, false
	}
	out := ChatTranscript{Session: session, Omitted: t.omitted, Version: t.version}
	out.Messages = make([]ChatMessage, 0, len(t.messages)+1)
	for _, msg := range t.messages {
		out.Messages = append(out.Messages, cloneChatMessage(msg, 0))
	}
	if t.pending != nil {
		out.Messages = append(out.Messages, cloneChatMessage(*t.pending, 0))
	}
	return out, true
}

// ChatTranscriptVersion reports a session's transcript version, or -1 when none is kept.
func (m *Manager) ChatTranscriptVersion(id string) int64 {
	chatStore.Lock()
	defer chatStore.Unlock()
	if t := chatStore.transcripts[id]; t != nil {
		return t.version
	}
	return -1
}

// ChatFeedItem is a message in the feed of all chats.
type ChatFeedItem struct {
	SessionID string      `json:"session_id"`
	Message   ChatMessage `json:"message"`
}

// ChatFeedResponse is the newest messages across all chats.
type ChatFeedResponse struct {
	Items    []ChatFeedItem            `json:"items"`
	Sessions map[string]RoutingSession `json:"sessions"`
}

// ChatFeed returns the newest messages across all chats, newest first. Long blocks are
// shortened; the transcript has them in full. Tool results and client context are left
// out unless includeTools is set. With origins, only chats started that way (see
// RoutingSession.Origin) are included.
func (m *Manager) ChatFeed(limit int, includeTools bool, origins ...string) ChatFeedResponse {
	if limit <= 0 || limit > 300 {
		limit = 80
	}
	sessions := make(map[string]RoutingSession)
	for _, s := range m.RoutingSessions() {
		sessions[s.ID] = s
	}
	allowed := make(map[string]bool)
	for _, origin := range origins {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = true
		}
	}
	chatStore.Lock()
	var items []ChatFeedItem
	for id, t := range chatStore.transcripts {
		if len(allowed) > 0 && !allowed[sessions[id].Origin] {
			continue
		}
		add := func(msg ChatMessage) {
			if !includeTools && !chatMessageHasProse(msg) {
				return
			}
			items = append(items, ChatFeedItem{SessionID: id, Message: msg})
		}
		for _, msg := range t.messages {
			add(msg)
		}
		if t.pending != nil {
			add(*t.pending)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Message.Seq > items[j].Message.Seq })
	if len(items) > limit {
		items = items[:limit]
	}
	for i := range items {
		items[i].Message = cloneChatMessage(items[i].Message, maxFeedBlockRunes)
		if !includeTools {
			kept := items[i].Message.Blocks[:0]
			for _, b := range items[i].Message.Blocks {
				if b.Kind == "text" || b.Kind == "tool_use" || b.Kind == "image" {
					kept = append(kept, b)
				}
			}
			items[i].Message.Blocks = kept
		}
	}
	chatStore.Unlock()

	out := ChatFeedResponse{Items: items, Sessions: make(map[string]RoutingSession)}
	for _, item := range items {
		if s, ok := sessions[item.SessionID]; ok {
			out.Sessions[item.SessionID] = s
		}
	}
	if out.Items == nil {
		out.Items = []ChatFeedItem{}
	}
	return out
}

// chatMessageHasProse reports whether a message has text a person wrote or the model said,
// as opposed to only tool traffic or client context.
func chatMessageHasProse(msg ChatMessage) bool {
	for _, b := range msg.Blocks {
		if b.Kind == "text" && b.Text != "" {
			return true
		}
	}
	return msg.Live && msg.Role == "assistant"
}

func cloneChatMessage(msg ChatMessage, blockLimit int) ChatMessage {
	msg.Blocks = append([]ChatBlock(nil), msg.Blocks...)
	if blockLimit > 0 {
		for i := range msg.Blocks {
			if text, cut := truncateRunes(msg.Blocks[i].Text, blockLimit); cut {
				msg.Blocks[i].Text, msg.Blocks[i].Truncated = text, true
			}
		}
	}
	return msg
}

func routingSessionByID(id string) *RoutingSession {
	routingActivity.Lock()
	defer routingActivity.Unlock()
	s := routingActivity.sessions[id]
	if s == nil {
		return nil
	}
	copied := *s
	copied.Models = make(map[string]int, len(s.Models))
	for k, v := range s.Models {
		copied.Models[k] = v
	}
	copied.Switches = append([]RoutingSwitch(nil), s.Switches...)
	return &copied
}
