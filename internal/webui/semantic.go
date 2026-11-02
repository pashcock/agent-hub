package webui

import (
	"context"
	"time"

	"github.com/pashcock/agent-hub/internal/semantic"
	"github.com/pashcock/agent-hub/internal/store"
)

type searchResult struct {
	Conversations []store.Conversation
	Count         int
	Hits          map[string]semantic.Hit
	Filter        store.ConversationFilter
	Notice        string
}

func (u *UI) find(ctx context.Context, f store.ConversationFilter, mode string) (searchResult, error) {
	result := searchResult{Filter: f, Hits: map[string]semantic.Hit{}}
	if mode == "hybrid" && f.Query != "" {
		if u.Semantic == nil {
			result.Notice = "Semantic search unavailable; showing literal matches."
		} else {
			searchCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			docs, err := u.st.SearchDocuments(searchCtx, f)
			var hits []semantic.Hit
			if err == nil {
				hits, err = semantic.Rank(searchCtx, u.Semantic, f.Query, docs)
			}
			cancel()
			if err != nil {
				result.Notice = "Semantic search unavailable: " + err.Error() + ". Showing literal matches."
			} else {
				result.Filter.Query = ""
				result.Filter.IDs = []string{}
				result.Notice = "Local meaning search + literal matches · best 200 conversations · similarity is not confidence."
				if len(hits) > 200 {
					hits = hits[:200]
				}
				for _, h := range hits {
					result.Filter.IDs = append(result.Filter.IDs, h.ConversationID)
					result.Hits[h.ConversationID] = h
				}
			}
		}
	}
	convs, count, err := u.st.SearchConversations(ctx, result.Filter)
	result.Conversations, result.Count = convs, count
	return result, err
}
