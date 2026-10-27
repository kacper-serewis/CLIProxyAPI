package auth

import (
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
)

const (
	maxRoutingSessions    = 200
	maxRoutingSwitches    = 20
	maxRoutingTitleLength = 80
)

// RoutingSwitch records a session moving from one credential to another.
type RoutingSwitch struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

// RoutingSession is the in-memory routing history of one client session, kept so the
// management panel can show which credential and model each conversation is using.
type RoutingSession struct {
	ID        string          `json:"id"`
	Client    string          `json:"client,omitempty"`
	Agent     string          `json:"agent,omitempty"`
	Parent    string          `json:"parent,omitempty"`
	Title     string          `json:"title,omitempty"`
	Provider  string          `json:"provider"`
	Auth      string          `json:"auth"`
	Model     string          `json:"model"`
	Models    map[string]int  `json:"models"`
	Requests  int             `json:"requests"`
	FirstSeen time.Time       `json:"first_seen"`
	LastSeen  time.Time       `json:"last_seen"`
	Switches  []RoutingSwitch `json:"switches,omitempty"`
}

// RoutingCandidate is one credential in the order soonest-reset would try it for a new session.
type RoutingCandidate struct {
	Auth        string    `json:"auth"`
	Known       bool      `json:"known"`
	LongReset   time.Time `json:"long_reset,omitempty"`
	ShortUsage  float64   `json:"short_usage"`
	Saturated   bool      `json:"saturated"`
	Unavailable bool      `json:"unavailable"`
	RetryAfter  time.Time `json:"retry_after,omitempty"`
}

var routingActivity = struct {
	sync.Mutex
	sessions map[string]*RoutingSession
}{sessions: make(map[string]*RoutingSession)}

// recordRoutingActivity notes which credential served a request. Requests without a
// recognisable session are not tracked: they never bind, so there is nothing to follow.
func recordRoutingActivity(selected *Auth, model string, opts cliproxyexecutor.Options, now time.Time) {
	if selected == nil {
		return
	}
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
		return
	}
	id = cliproxysession.BoundSessionIdentity(id)

	routingActivity.Lock()
	defer routingActivity.Unlock()
	session := routingActivity.sessions[id]
	if session == nil {
		session = &RoutingSession{
			ID:        id,
			Client:    info.ClientType,
			Agent:     info.AgentName,
			Parent:    info.ParentSessionID,
			Title:     routingSessionTitle(opts.OriginalRequest),
			Models:    make(map[string]int),
			FirstSeen: now,
		}
		routingActivity.sessions[id] = session
		evictRoutingSessionsLocked()
	} else if session.Auth != selected.ID {
		session.Switches = append(session.Switches, RoutingSwitch{At: now, From: session.Auth, To: selected.ID})
		if len(session.Switches) > maxRoutingSwitches {
			session.Switches = session.Switches[len(session.Switches)-maxRoutingSwitches:]
		}
	}
	session.Provider = strings.ToLower(selected.Provider)
	session.Auth = selected.ID
	session.Model = model
	session.Models[model]++
	session.Requests++
	session.LastSeen = now
}

func evictRoutingSessionsLocked() {
	for len(routingActivity.sessions) > maxRoutingSessions {
		var oldest *RoutingSession
		for _, session := range routingActivity.sessions {
			if oldest == nil || session.LastSeen.Before(oldest.LastSeen) {
				oldest = session
			}
		}
		delete(routingActivity.sessions, oldest.ID)
	}
}

// routingSessionTitle picks the first user text of a conversation so it can be
// recognised in the panel. Text starting with "<" is skipped: Claude Code prepends
// <system-reminder> blocks to the first user turn.
func routingSessionTitle(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	title := ""
	consider := func(text string) bool {
		// T3 Code opens a resumed thread with a context handoff; the user's actual
		// message follows its "User message:" marker.
		if strings.HasPrefix(strings.TrimSpace(text), "Context handoff") {
			_, after, found := strings.Cut(text, "User message:")
			if !found {
				return false
			}
			text = after
		}
		text = strings.Join(strings.Fields(text), " ")
		if text == "" || strings.HasPrefix(text, "<") {
			return false
		}
		title = text
		return true
	}
	// Anthropic messages and OpenAI chat use "messages"; the Responses API uses "input".
	for _, path := range []string{"messages", "input"} {
		gjson.GetBytes(payload, path).ForEach(func(_, message gjson.Result) bool {
			if message.Get("role").String() != "user" {
				return true
			}
			content := message.Get("content")
			if content.Type == gjson.String {
				return !consider(content.String())
			}
			content.ForEach(func(_, block gjson.Result) bool {
				return !consider(block.Get("text").String())
			})
			return title == ""
		})
		if title != "" {
			break
		}
	}
	if runes := []rune(title); len(runes) > maxRoutingTitleLength {
		title = string(runes[:maxRoutingTitleLength-1]) + "…"
	}
	return title
}

// RoutingSessions returns tracked sessions, most recently active first.
func (m *Manager) RoutingSessions() []RoutingSession {
	routingActivity.Lock()
	defer routingActivity.Unlock()
	sessions := make([]RoutingSession, 0, len(routingActivity.sessions))
	for _, session := range routingActivity.sessions {
		copied := *session
		copied.Models = make(map[string]int, len(session.Models))
		for model, count := range session.Models {
			copied.Models[model] = count
		}
		copied.Switches = append([]RoutingSwitch(nil), session.Switches...)
		sessions = append(sessions, copied)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].LastSeen.After(sessions[j].LastSeen) })
	return sessions
}

// RoutingQueue returns a provider's enabled credentials in the order soonest-reset ranks
// them for a new session. Credentials in cooldown are listed last.
func (m *Manager) RoutingQueue(provider string, now time.Time) []RoutingCandidate {
	var ranks []soonestResetRank
	for _, auth := range m.List() {
		if auth == nil || auth.Disabled || !strings.EqualFold(auth.Provider, provider) {
			continue
		}
		ranks = append(ranks, rankSoonestReset(auth, len(ranks), now))
	}
	unavailable := func(auth *Auth) bool { return auth.Unavailable && auth.NextRetryAfter.After(now) }
	sort.SliceStable(ranks, func(i, j int) bool {
		if a, b := unavailable(ranks[i].auth), unavailable(ranks[j].auth); a != b {
			return !a
		}
		return soonestResetLess(ranks[i], ranks[j])
	})
	queue := make([]RoutingCandidate, 0, len(ranks))
	for _, rank := range ranks {
		candidate := RoutingCandidate{
			Auth:        rank.auth.ID,
			Known:       rank.known,
			ShortUsage:  rank.shortUsage,
			Saturated:   rank.saturated,
			Unavailable: unavailable(rank.auth),
		}
		if rank.known {
			candidate.LongReset = rank.longReset
		}
		if candidate.Unavailable {
			candidate.RetryAfter = rank.auth.NextRetryAfter
		}
		queue = append(queue, candidate)
	}
	return queue
}
